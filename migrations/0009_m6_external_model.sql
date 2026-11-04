-- idp-cmi schema v9 (ADR-0001 D29): what may be sent to an external model is a tenant policy.
ALTER TABLE tenant_policies ADD COLUMN external_model_ceiling text NOT NULL DEFAULT 'internal'
  CHECK (external_model_ceiling IN ('public', 'internal', 'confidential', 'restricted'));
-- background workers (embedding projection, backfill) read the policy of every tenant
CREATE POLICY embedder ON tenant_policies USING (current_setting('app.role', true) = 'embedder');
