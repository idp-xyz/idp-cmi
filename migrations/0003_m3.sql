-- idp-cmi schema v3 (ADR-0001 D15–D17): consolidation, connectors and ingestion jobs, evaluations.

ALTER TABLE tenant_policies
  ADD COLUMN consolidate_min_records int NOT NULL DEFAULT 5 CHECK (consolidate_min_records BETWEEN 2 AND 100),
  ADD COLUMN consolidated_ttl_days int NOT NULL DEFAULT 90 CHECK (consolidated_ttl_days BETWEEN 1 AND 3650);

-- D15: an episodic record is consumed by at most one consolidation; the record itself is untouched.
ALTER TABLE memory_records ADD COLUMN consolidation_id uuid;
CREATE INDEX memory_records_unconsolidated ON memory_records (tenant_id, scope_kind, scope_id) WHERE kind = 'episodic' AND status = 'active' AND consolidation_id IS NULL;

CREATE TABLE consolidations (
  id           uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  scope_kind   text NOT NULL,
  scope_id     text NOT NULL DEFAULT '',
  sensitivity  text NOT NULL,
  acl_subjects text[] NOT NULL DEFAULT '{}',
  acl_roles    text[] NOT NULL DEFAULT '{}',
  model        text NOT NULL,
  status       text NOT NULL CHECK (status IN ('completed', 'rejected', 'failed')),
  input_ids    uuid[] NOT NULL,
  output_ids   uuid[] NOT NULL DEFAULT '{}',
  detail       jsonb,
  created_by   text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX consolidations_by_tenant ON consolidations (tenant_id, created_at DESC);

-- D16: connectors enumerate documents of a document-store source; jobs fetch and index them.
CREATE TABLE connectors (
  id               uuid PRIMARY KEY,
  tenant_id        uuid NOT NULL REFERENCES tenants(id),
  source_id        uuid NOT NULL REFERENCES sources(id),
  name             text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$'),
  kind             text NOT NULL CHECK (kind IN ('http-manifest', 'filesystem')),
  config           jsonb NOT NULL,
  schedule_seconds int  NOT NULL DEFAULT 0 CHECK (schedule_seconds >= 0),
  last_sync_at     timestamptz,
  created_by       text NOT NULL,
  created_at       timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name)
);

CREATE TABLE ingest_jobs (
  id           uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  connector_id uuid NOT NULL REFERENCES connectors(id),
  status       text NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'done', 'skipped', 'failed')),
  attempts     int  NOT NULL DEFAULT 0,
  lease_owner  text,
  lease_until  timestamptz,
  uri          text NOT NULL,
  url          text NOT NULL,
  title        text NOT NULL,
  sensitivity  text NOT NULL CHECK (sensitivity IN ('public', 'internal', 'confidential', 'restricted')),
  acl_subjects text[] NOT NULL DEFAULT '{}',
  acl_roles    text[] NOT NULL DEFAULT '{}',
  entities     jsonb NOT NULL DEFAULT '[]',
  error        text,
  document_id  uuid,
  created_at   timestamptz NOT NULL DEFAULT now(),
  finished_at  timestamptz
);
CREATE INDEX ingest_jobs_claim ON ingest_jobs (tenant_id, status, lease_until) WHERE status IN ('queued', 'running');
CREATE INDEX ingest_jobs_by_connector ON ingest_jobs (connector_id, created_at DESC);

-- D17: evaluation runs and their evidence.
CREATE TABLE evaluations (
  id         uuid PRIMARY KEY,
  tenant_id  uuid NOT NULL REFERENCES tenants(id),
  status     text NOT NULL CHECK (status IN ('PASSED', 'FAILED')),
  checks     jsonb NOT NULL,
  created_by text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX evaluations_by_tenant ON evaluations (tenant_id, created_at DESC);

DO $$ DECLARE t text; BEGIN
  FOREACH t IN ARRAY ARRAY['consolidations', 'connectors', 'ingest_jobs', 'evaluations'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY tenant_rows ON %I USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant())', t);
  END LOOP;
END $$;
GRANT SELECT, INSERT ON consolidations, evaluations TO idp_cmi_app;
GRANT SELECT, INSERT, UPDATE ON connectors, ingest_jobs TO idp_cmi_app;
