-- idp-cmi schema v5 (ADR-0001 D20): erasure as a controlled process. Requesting an erasure sets a
-- synchronous read barrier (erasure_id on the targets; every read path hides their plaintext from
-- that moment); a Worker then purges each domain and records what it did.

CREATE TABLE erasures (
  id            uuid PRIMARY KEY,
  tenant_id     uuid NOT NULL REFERENCES tenants(id),
  requested_by  text NOT NULL,
  reason        text NOT NULL,
  selector      jsonb NOT NULL,                 -- what was asked for: recordIds, documentIds, ref, subject, entity
  record_ids    uuid[] NOT NULL DEFAULT '{}',   -- the targets the barrier caught
  document_ids  uuid[] NOT NULL DEFAULT '{}',
  status        text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'running', 'verified', 'failed')),
  domains       jsonb NOT NULL DEFAULT '{}',    -- per domain: {status, rows, at, error}
  attempts      int  NOT NULL DEFAULT 0,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX erasures_by_tenant ON erasures (tenant_id, created_at DESC);
ALTER TABLE erasures ENABLE ROW LEVEL SECURITY;
ALTER TABLE erasures FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON erasures USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT, UPDATE ON erasures TO idp_cmi_app;

ALTER TABLE memory_records ADD COLUMN erasure_id uuid;
ALTER TABLE knowledge_documents ADD COLUMN erasure_id uuid;
CREATE INDEX memory_records_erasure ON memory_records (erasure_id) WHERE erasure_id IS NOT NULL;
CREATE INDEX knowledge_documents_erasure ON knowledge_documents (erasure_id) WHERE erasure_id IS NOT NULL;
-- packages whose items were erased: the stored document is redacted, the original digest is kept as
-- the record of what was sealed; redacted says the two no longer match by design
ALTER TABLE context_packages ADD COLUMN redacted boolean NOT NULL DEFAULT false;
GRANT UPDATE ON context_packages TO idp_cmi_app;
GRANT DELETE ON entity_refs TO idp_cmi_app;
GRANT UPDATE ON audit_events TO idp_cmi_app;
