-- idp-cmi schema v7 (ADR-0001 D22): organization, team and project scopes; the tenant's unit
-- directory decides who sees a unit's memory; sharing copies a record into a unit explicitly.
CREATE TABLE units (
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  kind         text NOT NULL CHECK (kind IN ('organization', 'team', 'project')),
  id           text NOT NULL CHECK (id ~ '^[A-Za-z0-9_.:/@-]+$' AND length(id) <= 256),
  display_name text NOT NULL,
  parent_kind  text,
  parent_id    text,
  members      text[] NOT NULL DEFAULT '{}',   -- subjects
  roles        text[] NOT NULL DEFAULT '{}',   -- any holder of one of these roles is a member too
  updated_by   text NOT NULL,
  updated_at   timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, kind, id)
);
ALTER TABLE units ENABLE ROW LEVEL SECURITY;
ALTER TABLE units FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON units USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT, UPDATE, DELETE ON units TO idp_cmi_app;

ALTER TABLE memory_records DROP CONSTRAINT memory_records_scope_kind_check;
ALTER TABLE memory_records ADD CONSTRAINT memory_records_scope_kind_check CHECK (scope_kind IN ('tenant', 'subject', 'thread', 'agent', 'organization', 'team', 'project'));
ALTER TABLE memory_records ADD COLUMN shared_from uuid;   -- the record this one was shared from (D22)
