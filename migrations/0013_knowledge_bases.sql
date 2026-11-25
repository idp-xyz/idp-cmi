-- Knowledge bases (enterprise / unit / personal). Existing documents are attached to a
-- tenant-scoped base named after their source, so visibility of what was already indexed
-- does not change.
--
-- The backfill below has to see every tenant's rows. The migrator is the schema owner, and FORCE row-level
-- security applies to an owner that is not a superuser, so each table the backfill reads or updates has
-- FORCE switched off for the length of this transaction (the owner bypasses a policy that is only ENABLEd)
-- and switched back on before it ends. A superuser owner is unaffected either way.

ALTER TABLE sources NO FORCE ROW LEVEL SECURITY;
ALTER TABLE knowledge_documents NO FORCE ROW LEVEL SECURITY;

CREATE TABLE knowledge_bases (
  id                   uuid PRIMARY KEY,
  tenant_id            uuid NOT NULL REFERENCES tenants(id),
  name                 text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$'),
  display_name         text NOT NULL,
  scope_kind           text NOT NULL CHECK (scope_kind IN ('tenant', 'organization', 'team', 'project', 'subject')),
  scope_id             text NOT NULL DEFAULT '',
  manager_subjects     text[] NOT NULL DEFAULT '{}',
  manager_roles        text[] NOT NULL DEFAULT '{}',
  default_sensitivity  text NOT NULL CHECK (default_sensitivity IN ('public', 'internal', 'confidential', 'restricted')),
  sensitivity_ceiling  text NOT NULL CHECK (sensitivity_ceiling IN ('public', 'internal', 'confidential', 'restricted')),
  source_id            uuid NOT NULL REFERENCES sources(id),
  quota_documents      int NOT NULL DEFAULT 0 CHECK (quota_documents >= 0),
  quota_bytes          bigint NOT NULL DEFAULT 0 CHECK (quota_bytes >= 0),
  ocr                  boolean NOT NULL DEFAULT false,
  status               text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
  created_by           text NOT NULL,
  created_at           timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name),
  CHECK (
    (scope_kind = 'tenant' AND scope_id = '')
    OR (scope_kind IN ('organization', 'team', 'project', 'subject') AND scope_id <> '')
  )
);
CREATE INDEX knowledge_bases_scope ON knowledge_bases (tenant_id, scope_kind, scope_id);
-- a member has one personal base, whatever it is called: it is found by its owner, not by its name
CREATE UNIQUE INDEX knowledge_bases_personal ON knowledge_bases (tenant_id, scope_id) WHERE scope_kind = 'subject';

INSERT INTO knowledge_bases (
  id, tenant_id, name, display_name, scope_kind, scope_id,
  default_sensitivity, sensitivity_ceiling, source_id, created_by
)
SELECT gen_random_uuid(), s.tenant_id, s.name, s.display_name, 'tenant', '',
       'internal', 'restricted', s.id, 'migration'
FROM sources s
WHERE EXISTS (
  SELECT 1 FROM knowledge_documents d WHERE d.source_id = s.id AND d.tenant_id = s.tenant_id
);

CREATE POLICY tenant_rows ON knowledge_bases USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT, UPDATE ON knowledge_bases TO idp_cmi_app;

ALTER TABLE knowledge_documents ADD COLUMN knowledge_base_id uuid REFERENCES knowledge_bases(id);

UPDATE knowledge_documents d
SET knowledge_base_id = kb.id
FROM knowledge_bases kb
WHERE kb.tenant_id = d.tenant_id AND kb.source_id = d.source_id AND kb.scope_kind = 'tenant' AND kb.name = (
  SELECT s.name FROM sources s WHERE s.id = d.source_id
);

ALTER TABLE knowledge_documents ALTER COLUMN knowledge_base_id SET NOT NULL;

-- A version's identity is (tenant, knowledge base, uri): the tenant is the base's, so the unique key is
-- (base, uri, version). The same uri in two bases is two documents.
ALTER TABLE knowledge_documents DROP CONSTRAINT knowledge_documents_tenant_id_uri_version_key;
ALTER TABLE knowledge_documents ADD CONSTRAINT knowledge_documents_base_uri_version_key UNIQUE (knowledge_base_id, uri, version);
CREATE INDEX knowledge_documents_base ON knowledge_documents (knowledge_base_id, status);

ALTER TABLE knowledge_bases ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge_bases FORCE ROW LEVEL SECURITY;
ALTER TABLE sources FORCE ROW LEVEL SECURITY;
ALTER TABLE knowledge_documents FORCE ROW LEVEL SECURITY;
