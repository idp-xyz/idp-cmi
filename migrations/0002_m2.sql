-- idp-cmi schema v2 (ADR-0001 D11–D13): embeddings as PostgreSQL arrays (no extension), write-time
-- pollution flags, explicit entity references, the policy's quarantine switch.

-- D11: vectors of the configured embedding model; NULL until embedded (the backfill fills them).
ALTER TABLE memory_records ADD COLUMN embedding real[], ADD COLUMN embedding_model text;
ALTER TABLE knowledge_chunks ADD COLUMN embedding real[], ADD COLUMN embedding_model text;

-- cosine similarity over arrays: a brute-force comparison inside the visibility predicate. NULL when
-- either side is missing or the dimensions differ; 0 for a zero vector.
CREATE FUNCTION cosine_similarity(a real[], b real[]) RETURNS real LANGUAGE sql IMMUTABLE STRICT AS $$
  SELECT CASE WHEN cardinality(a) <> cardinality(b) OR cardinality(a) = 0 THEN NULL
         ELSE (SELECT CASE WHEN sqrt(sum(x*x)) * sqrt(sum(y*y)) = 0 THEN 0
                           ELSE sum(x*y) / (sqrt(sum(x*x)) * sqrt(sum(y*y))) END
               FROM unnest(a, b) AS t(x, y))::real END
$$;

-- D12: flags from the write-time scan; flagged content is quarantined from packages by default.
ALTER TABLE memory_records ADD COLUMN flags text[] NOT NULL DEFAULT '{}';
ALTER TABLE knowledge_chunks ADD COLUMN flags text[] NOT NULL DEFAULT '{}';
ALTER TABLE knowledge_documents ADD COLUMN flags text[] NOT NULL DEFAULT '{}';
CREATE INDEX memory_records_flagged ON memory_records (tenant_id) WHERE cardinality(flags) > 0;
ALTER TABLE tenant_policies ADD COLUMN include_flagged boolean NOT NULL DEFAULT false;

-- D13: explicit entity references of records and documents.
CREATE TABLE entity_refs (
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  object_kind text NOT NULL CHECK (object_kind IN ('memory', 'knowledge')),
  object_id   uuid NOT NULL,
  entity_type text NOT NULL CHECK (entity_type ~ '^[a-z][a-z0-9-]{0,62}$'),
  entity_id   text NOT NULL,
  PRIMARY KEY (object_kind, object_id, entity_type, entity_id)
);
CREATE INDEX entity_refs_lookup ON entity_refs (tenant_id, entity_type, entity_id);
ALTER TABLE entity_refs ENABLE ROW LEVEL SECURITY;
ALTER TABLE entity_refs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON entity_refs USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
GRANT SELECT, INSERT ON entity_refs TO idp_cmi_app;

-- the embedding backfill sees every tenant's rows without a vector, like the sweeper
CREATE POLICY embedder ON memory_records USING (current_setting('app.role', true) = 'embedder');
CREATE POLICY embedder_chunks ON knowledge_chunks USING (current_setting('app.role', true) = 'embedder');
CREATE POLICY embedder_docs ON knowledge_documents USING (current_setting('app.role', true) = 'embedder');
