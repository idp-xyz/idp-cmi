-- idp-cmi schema v4 (ADR-0001 D19): idempotent writes and expected versions.

-- a write's idempotency key: same key + same request replays the first answer, same key + a
-- different request is a conflict; keys are swept after 24 hours
CREATE TABLE idempotency_keys (
  tenant_id      uuid NOT NULL REFERENCES tenants(id),
  subject        text NOT NULL,
  key            text NOT NULL,
  request_sha256 text NOT NULL,
  status         int  NOT NULL,
  response       jsonb NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, subject, key)
);
CREATE INDEX idempotency_keys_age ON idempotency_keys (created_at);
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON idempotency_keys USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY sweeper ON idempotency_keys USING (current_setting('app.role', true) = 'sweeper');
GRANT SELECT, INSERT, DELETE ON idempotency_keys TO idp_cmi_app;

-- every state change of a record or document bumps its version; If-Match names the one expected
ALTER TABLE memory_records ADD COLUMN version int NOT NULL DEFAULT 1 CHECK (version >= 1);
ALTER TABLE knowledge_documents ADD COLUMN revision int NOT NULL DEFAULT 1 CHECK (revision >= 1);
