-- IDP-CMI TECH-001: PostgreSQL canonical store (DESIGN / NOT PRODUCTION MIGRATION)
-- Baseline PostgreSQL >=16; DDL has NOT been run against a live PostgreSQL server.
-- Run under dedicated NOLOGIN migration owner; configure roles and RLS in 002.
BEGIN;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE SCHEMA IF NOT EXISTS cmi;

CREATE TABLE cmi.source_record (
 tenant_id uuid NOT NULL,
 source_id uuid NOT NULL DEFAULT gen_random_uuid(),
 source_system text NOT NULL,
 source_object_key text NOT NULL,
 source_type text NOT NULL CHECK(source_type IN('event','document','conversation','api_snapshot','human_attestation')),
 current_revision integer NOT NULL DEFAULT 1 CHECK(current_revision > 0),
 status text NOT NULL DEFAULT 'available' CHECK(status IN('available','restricted','revoked')),
 classification text NOT NULL CHECK(classification IN('public','internal','confidential','restricted')),
 source_trust text NOT NULL CHECK(source_trust IN('authoritative_domain','verified_document','internal_observation','user_statement','external_untrusted')),
 access_policy_ref text NOT NULL,
 retention_policy_ref text NOT NULL,
 authority_scope jsonb,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,source_id),
 UNIQUE(tenant_id,source_system,source_object_key)
);

CREATE TABLE cmi.source_revision (
 tenant_id uuid NOT NULL,
 source_id uuid NOT NULL,
 revision integer NOT NULL CHECK(revision>0),
 upstream_revision_key text NOT NULL,
 content_hash text NOT NULL CHECK(content_hash ~ '^sha256:[0-9a-f]{64}$'),
 content_location text NOT NULL,
 etag text,
 captured_at timestamptz NOT NULL,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 source_metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
 PRIMARY KEY(tenant_id,source_id,revision),
 UNIQUE(tenant_id,source_id,upstream_revision_key),
 FOREIGN KEY(tenant_id,source_id) REFERENCES cmi.source_record(tenant_id,source_id) ON DELETE CASCADE
);

CREATE TABLE cmi.memory_record (
 tenant_id uuid NOT NULL,
 memory_id uuid NOT NULL DEFAULT gen_random_uuid(),
 current_revision integer NOT NULL DEFAULT 1 CHECK(current_revision>0),
 status text NOT NULL DEFAULT 'candidate' CHECK(status IN('candidate','quarantined','active','disputed','superseded','expired','revoked')),
 kind text NOT NULL CHECK(kind IN('working','episodic','semantic','procedural','preference')),
 scope_type text NOT NULL CHECK(scope_type IN('tenant','group','principal','task','entity')),
 scope_ref text NOT NULL,
 subject_ref text NOT NULL,
 classification text NOT NULL CHECK(classification IN('public','internal','confidential','restricted')),
 access_policy_ref text NOT NULL,
 retention_policy_ref text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,memory_id)
);

CREATE TABLE cmi.memory_revision (
 tenant_id uuid NOT NULL,
 memory_id uuid NOT NULL,
 revision integer NOT NULL CHECK(revision>0),
 claim_text text NOT NULL CHECK(length(claim_text)>0),
 assertion_kind text NOT NULL CHECK(assertion_kind IN('observation','derived_claim','recommendation','approved_policy_reference')),
 language text NOT NULL,
 valid_from timestamptz,
 valid_to timestamptz,
 recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 confidence numeric(5,4) CHECK(confidence BETWEEN 0 AND 1),
 confidence_method text,
 extractor_id text,
 extractor_version text,
 model_profile_ref text,
 revision_status text NOT NULL CHECK(revision_status IN('candidate','quarantined','active','disputed','superseded','expired','revoked')),
 PRIMARY KEY(tenant_id,memory_id,revision),
 FOREIGN KEY(tenant_id,memory_id) REFERENCES cmi.memory_record(tenant_id,memory_id) ON DELETE CASCADE,
 CHECK(valid_to IS NULL OR valid_from IS NULL OR valid_to>valid_from)
);

CREATE TABLE cmi.memory_source_link (
 tenant_id uuid NOT NULL,
 memory_id uuid NOT NULL,
 memory_revision integer NOT NULL,
 source_id uuid NOT NULL,
 source_revision integer NOT NULL,
 content_span jsonb,
 PRIMARY KEY(tenant_id,memory_id,memory_revision,source_id,source_revision),
 FOREIGN KEY(tenant_id,memory_id,memory_revision) REFERENCES cmi.memory_revision(tenant_id,memory_id,revision) ON DELETE CASCADE,
 FOREIGN KEY(tenant_id,source_id,source_revision) REFERENCES cmi.source_revision(tenant_id,source_id,revision) ON DELETE CASCADE
);
CREATE INDEX memory_source_inverse ON cmi.memory_source_link(tenant_id,source_id,source_revision);

-- Visibility blocker MUST be checked on reads independently of async index status.
CREATE TABLE cmi.visibility_block (
 tenant_id uuid NOT NULL,
 resource_kind text NOT NULL CHECK(resource_kind IN('source','memory','bundle')),
 resource_id uuid NOT NULL,
 reason_code text NOT NULL,
 blocked_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 operation_id uuid NOT NULL,
 PRIMARY KEY(tenant_id,resource_kind,resource_id)
);

-- Bundle stores metadata only. Materialized text is stored separately with expiring access controls.
CREATE TABLE cmi.context_bundle (
 tenant_id uuid NOT NULL,
 bundle_id uuid NOT NULL DEFAULT gen_random_uuid(),
 principal_ref text NOT NULL,
 purpose text NOT NULL,
 task_ref text NOT NULL,
 policy_version text NOT NULL,
 compiler_version text NOT NULL,
 model_id text NOT NULL,
 tokenizer_id text NOT NULL,
 tokenizer_version text NOT NULL,
 hard_limit_tokens integer NOT NULL CHECK(hard_limit_tokens>0),
 output_reserve_tokens integer NOT NULL CHECK(output_reserve_tokens>=0),
 compiled_tokens integer NOT NULL CHECK(compiled_tokens>=0),
 manifest_hash text NOT NULL,
 manifest_location text,
 compiled_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 status text NOT NULL DEFAULT 'valid' CHECK(status IN('valid','invalidated','expired')),
 PRIMARY KEY(tenant_id,bundle_id),
 CHECK(output_reserve_tokens+compiled_tokens<=hard_limit_tokens),
 CHECK(expires_at>compiled_at)
);
CREATE TABLE cmi.context_bundle_item (
 tenant_id uuid NOT NULL,
 bundle_id uuid NOT NULL,
 item_order integer NOT NULL CHECK(item_order>=0),
 memory_id uuid,
 memory_revision integer,
 source_id uuid,
 source_revision integer,
 segment_kind text NOT NULL,
 token_count integer NOT NULL CHECK(token_count>=0),
 requires_live_revalidation boolean NOT NULL DEFAULT false,
 item_hash text NOT NULL,
 PRIMARY KEY(tenant_id,bundle_id,item_order),
 FOREIGN KEY(tenant_id,bundle_id) REFERENCES cmi.context_bundle(tenant_id,bundle_id) ON DELETE CASCADE
);
CREATE INDEX bundle_memory_dependency ON cmi.context_bundle_item(tenant_id,memory_id);
CREATE INDEX bundle_source_dependency ON cmi.context_bundle_item(tenant_id,source_id);

CREATE TABLE cmi.privacy_operation (
 tenant_id uuid NOT NULL,
 operation_id uuid NOT NULL DEFAULT gen_random_uuid(),
 target_kind text NOT NULL CHECK(target_kind IN('source','memory','principal')),
 target_ref text NOT NULL,
 status text NOT NULL CHECK(status IN('requested','blocked','purge_in_progress','verified','exception')),
 reason_code text NOT NULL,
 policy_ref text NOT NULL,
 requested_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 verified_at timestamptz,
 exception_ref text,
 PRIMARY KEY(tenant_id,operation_id)
);
CREATE TABLE cmi.erasure_tombstone (
 tenant_id uuid NOT NULL,
 tombstone_id uuid NOT NULL DEFAULT gen_random_uuid(),
 operation_id uuid NOT NULL,
 resource_kind text NOT NULL,
 resource_id uuid NOT NULL,
 erased_at timestamptz NOT NULL,
 retention_until timestamptz,
 PRIMARY KEY(tenant_id,tombstone_id),
 UNIQUE(tenant_id,resource_kind,resource_id),
 FOREIGN KEY(tenant_id,operation_id) REFERENCES cmi.privacy_operation(tenant_id,operation_id)
);

-- Keys scoped to principal AND operation; result_ref is a protected pointer, never raw PII response.
CREATE TABLE cmi.idempotency_record (
 tenant_id uuid NOT NULL,
 principal_ref text NOT NULL,
 operation_type text NOT NULL,
 idempotency_key text NOT NULL,
 request_hash text NOT NULL,
 http_status smallint,
 result_ref text,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 PRIMARY KEY(tenant_id,principal_ref,operation_type,idempotency_key)
);

CREATE TABLE cmi.outbox_event (
 tenant_id uuid NOT NULL,
 event_id uuid NOT NULL DEFAULT gen_random_uuid(),
 aggregate_kind text NOT NULL,
 aggregate_id uuid NOT NULL,
 aggregate_revision integer NOT NULL,
 event_type text NOT NULL,
 event_payload jsonb NOT NULL DEFAULT '{}'::jsonb,
 status text NOT NULL DEFAULT 'pending' CHECK(status IN('pending','leased','delivered','dead_letter')),
 attempt_count integer NOT NULL DEFAULT 0,
 leased_until timestamptz,
 next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,event_id),
 UNIQUE(tenant_id,aggregate_kind,aggregate_id,aggregate_revision,event_type)
);
CREATE INDEX outbox_claim_idx ON cmi.outbox_event(tenant_id,status,next_attempt_at,created_at);
CREATE TABLE cmi.projection_receipt (
 tenant_id uuid NOT NULL,
 event_id uuid NOT NULL,
 projection_name text NOT NULL,
 generation text NOT NULL,
 completed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,event_id,projection_name,generation),
 FOREIGN KEY(tenant_id,event_id) REFERENCES cmi.outbox_event(tenant_id,event_id) ON DELETE CASCADE
);

CREATE TABLE cmi.engine_mapping (
 tenant_id uuid NOT NULL,
 engine_name text NOT NULL,
 engine_generation text NOT NULL,
 memory_id uuid NOT NULL,
 memory_revision integer NOT NULL,
 engine_record_id text NOT NULL,
 indexed_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(tenant_id,engine_name,engine_generation,memory_id,memory_revision),
 UNIQUE(tenant_id,engine_name,engine_generation,engine_record_id)
);
CREATE TABLE cmi.audit_event_ref (
 tenant_id uuid NOT NULL,
 audit_id uuid NOT NULL DEFAULT gen_random_uuid(),
 action text NOT NULL,
 principal_ref text NOT NULL,
 resource_kind text NOT NULL,
 resource_id uuid,
 decision_id text,
 trace_id text,
 occurred_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 audit_store_ref text NOT NULL,
 PRIMARY KEY(tenant_id,audit_id)
);
COMMIT;
