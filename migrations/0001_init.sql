-- idp-cmi schema v1 (ADR-0001 D1–D7). Run by the schema owner (`idp-cmi migrate`). The service
-- connects as a login role that is a member of idp_cmi_app: not a superuser, no BYPASSRLS.

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'idp_cmi_app') THEN
    CREATE ROLE idp_cmi_app NOLOGIN NOBYPASSRLS;
  END IF;
END $$;

CREATE FUNCTION current_tenant() RETURNS uuid LANGUAGE sql STABLE AS $$
  SELECT nullif(current_setting('app.tenant_id', true), '')::uuid
$$;

CREATE TABLE tenants (
  id         uuid PRIMARY KEY,
  slug       text NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
  created_at timestamptz NOT NULL DEFAULT now()
);

-- D2: where content comes from. Trust orders the package; agent and user sources are informational.
CREATE TABLE sources (
  id          uuid PRIMARY KEY,
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  name        text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$'),
  display_name text NOT NULL,
  kind        text NOT NULL CHECK (kind IN ('domain-system', 'document-store', 'agent', 'user')),
  trust       text NOT NULL CHECK (trust IN ('authoritative', 'informational')),
  base_url    text,
  created_by  text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name)
);

-- D3: memory records are appended, then expired or revoked; never edited.
CREATE TABLE memory_records (
  id            uuid PRIMARY KEY,
  tenant_id     uuid NOT NULL REFERENCES tenants(id),
  source_id     uuid NOT NULL REFERENCES sources(id),
  scope_kind    text NOT NULL CHECK (scope_kind IN ('tenant', 'subject', 'thread', 'agent')),
  scope_id      text NOT NULL DEFAULT '',
  kind          text NOT NULL CHECK (kind IN ('episodic', 'semantic')),
  content       text NOT NULL,
  structured    jsonb,
  tags          text[] NOT NULL DEFAULT '{}',
  prov_ref      text NOT NULL,              -- the source's own reference (document id, order number, run id ...)
  prov_observed_at timestamptz NOT NULL,
  prov_recorded_by text NOT NULL,           -- the principal or run the writer names as the origin of the record
  created_by    text NOT NULL,              -- the authenticated writer; only it (or an administrator) may revoke the record
  valid_from    timestamptz NOT NULL DEFAULT now(),
  valid_until   timestamptz,                -- NULL = until revoked
  status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'expired', 'revoked')),
  revoked_at    timestamptz,
  revoked_by    text,
  revoked_reason text,
  sensitivity   text NOT NULL CHECK (sensitivity IN ('public', 'internal', 'confidential', 'restricted')),
  acl_subjects  text[] NOT NULL DEFAULT '{}',   -- empty = every subject of the tenant (within sensitivity)
  acl_roles     text[] NOT NULL DEFAULT '{}',
  search        tsvector NOT NULL,          -- 'simple' tokens of the content plus CJK character bigrams (computed on insert)
  created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX memory_records_search ON memory_records USING gin (search);
CREATE INDEX memory_records_active ON memory_records (tenant_id, status, valid_until);
CREATE INDEX memory_records_scope ON memory_records (tenant_id, scope_kind, scope_id);
CREATE INDEX memory_records_ref ON memory_records (tenant_id, prov_ref);

-- D4: knowledge documents and their indexed chunks; a new version of the same URI supersedes the old.
CREATE TABLE knowledge_documents (
  id           uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  source_id    uuid NOT NULL REFERENCES sources(id),
  uri          text NOT NULL,
  title        text NOT NULL,
  version      int  NOT NULL CHECK (version >= 1),
  content_sha256 text NOT NULL,
  byte_size    int  NOT NULL,
  chunk_count  int  NOT NULL,
  status       text NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'superseded', 'revoked', 'expired')),
  valid_until  timestamptz,
  sensitivity  text NOT NULL CHECK (sensitivity IN ('public', 'internal', 'confidential', 'restricted')),
  acl_subjects text[] NOT NULL DEFAULT '{}',
  acl_roles    text[] NOT NULL DEFAULT '{}',
  revoked_at   timestamptz,
  revoked_by   text,
  revoked_reason text,
  created_by   text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, uri, version)
);
CREATE INDEX knowledge_documents_uri ON knowledge_documents (tenant_id, uri, status);

CREATE TABLE knowledge_chunks (
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  document_id uuid NOT NULL REFERENCES knowledge_documents(id),
  seq         int  NOT NULL,
  content     text NOT NULL,
  search      tsvector NOT NULL,
  PRIMARY KEY (document_id, seq)
);
CREATE INDEX knowledge_chunks_search ON knowledge_chunks USING gin (search);

-- D5: packages are sealed results; the items they cite are kept so a later read can tell what went stale.
CREATE TABLE context_packages (
  id           uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES tenants(id),
  requested_by text NOT NULL,
  digest       text NOT NULL,
  document     text NOT NULL,     -- canonical JSON of the package (byte-exact; the digest covers it)
  item_refs    jsonb NOT NULL,    -- [{kind, id}] of the records and documents cited
  created_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL
);
CREATE INDEX context_packages_by_tenant ON context_packages (tenant_id, created_at DESC);

CREATE TABLE tenant_policies (
  tenant_id       uuid PRIMARY KEY REFERENCES tenants(id),
  reader_ceiling  text NOT NULL DEFAULT 'internal' CHECK (reader_ceiling IN ('public', 'internal', 'confidential', 'restricted')),
  writer_ceiling  text NOT NULL DEFAULT 'confidential' CHECK (writer_ceiling IN ('public', 'internal', 'confidential', 'restricted')),
  default_memory_ttl_days int NOT NULL DEFAULT 0 CHECK (default_memory_ttl_days >= 0),   -- 0 = no default expiry
  package_ttl_seconds int NOT NULL DEFAULT 3600 CHECK (package_ttl_seconds BETWEEN 60 AND 604800),
  updated_by      text,
  updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE audit_events (
  id          bigserial PRIMARY KEY,
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  at          timestamptz NOT NULL DEFAULT now(),
  subject     text NOT NULL,
  action      text NOT NULL,
  resource_id text,
  detail      jsonb
);

DO $$ DECLARE t text; BEGIN
  FOREACH t IN ARRAY ARRAY['sources', 'memory_records', 'knowledge_documents', 'knowledge_chunks', 'context_packages', 'tenant_policies', 'audit_events'] LOOP
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
    EXECUTE format('CREATE POLICY tenant_rows ON %I USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant())', t);
  END LOOP;
END $$;
-- the expiry sweeper sees every tenant's records to find what to expire, then binds each tenant
CREATE POLICY sweeper ON memory_records USING (current_setting('app.role', true) = 'sweeper');
CREATE POLICY sweeper_docs ON knowledge_documents USING (current_setting('app.role', true) = 'sweeper');

GRANT SELECT ON tenants TO idp_cmi_app;
GRANT SELECT, INSERT, UPDATE ON sources, memory_records, knowledge_documents, knowledge_chunks, tenant_policies TO idp_cmi_app;
GRANT SELECT, INSERT ON context_packages, audit_events TO idp_cmi_app;
GRANT USAGE ON SEQUENCE audit_events_id_seq TO idp_cmi_app;
