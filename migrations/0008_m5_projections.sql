-- idp-cmi schema v8 (ADR-0001 D26): the embedding projection is driven by an outbox written in the
-- same transaction as the canonical row. The full-text index stays synchronous (same transaction, no
-- projection); vectors are a rebuildable projection whose backlog is visible.
CREATE TABLE projection_events (
  id           bigserial PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  kind         text NOT NULL CHECK (kind IN ('memory', 'chunk')),
  object_id    uuid NOT NULL,            -- record id, or document id for chunks
  seq          int  NOT NULL DEFAULT 0,  -- chunk sequence; 0 for records
  version      int  NOT NULL,            -- the row version the event describes; older events never overwrite newer rows
  action       text NOT NULL CHECK (action IN ('upsert', 'delete')),
  attempts     int  NOT NULL DEFAULT 0,
  created_at   timestamptz NOT NULL DEFAULT now(),
  processed_at timestamptz
);
CREATE INDEX projection_events_pending ON projection_events (id) WHERE processed_at IS NULL;
CREATE INDEX projection_events_tenant_pending ON projection_events (tenant_id, created_at) WHERE processed_at IS NULL;
ALTER TABLE projection_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE projection_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON projection_events USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY projector ON projection_events USING (current_setting('app.role', true) = 'embedder');
GRANT SELECT, INSERT, UPDATE ON projection_events TO idp_cmi_app;
GRANT USAGE ON SEQUENCE projection_events_id_seq TO idp_cmi_app;
