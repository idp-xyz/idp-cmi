-- KB3 (ADR-0001 D61-D70): originals of PDF and Office files are converted to text in a dedicated idp-sandbox instance.
--
-- * knowledge_conversions: one row per conversion of an original (a queue with leases, retries and the outcome).
-- * knowledge_ticket_uses: the tickets CMI signed for the sandbox's file transfers and that were presented, so that a ticket
--   works once, across restarts and replicas.
-- * knowledge_documents.status 'pending': a version whose text is still being extracted. It is not in force, replaces
--   nothing, is not searched or embedded; it becomes 'active' when the text is committed.
-- * knowledge_documents.text_source: where the text of a version came from ('supplied' by the writer or 'extracted').
-- * knowledge_chunks: slide, sheet and cell_range locate a chunk of a presentation or a spreadsheet.
-- The tables follow the others: row-level security enabled and forced; the conversion worker (app.role = 'converter') claims
-- across tenants, as the embedder does.

ALTER TABLE knowledge_documents DROP CONSTRAINT knowledge_documents_status_check;
ALTER TABLE knowledge_documents ADD CONSTRAINT knowledge_documents_status_check CHECK (status IN ('active', 'superseded', 'revoked', 'expired', 'pending'));
ALTER TABLE knowledge_documents ADD COLUMN text_source text NOT NULL DEFAULT 'supplied' CHECK (text_source IN ('supplied', 'extracted'));

ALTER TABLE knowledge_chunks ADD COLUMN slide int;
ALTER TABLE knowledge_chunks ADD COLUMN sheet text NOT NULL DEFAULT '';
ALTER TABLE knowledge_chunks ADD COLUMN cell_range text NOT NULL DEFAULT '';

CREATE TABLE knowledge_conversions (
  id              uuid PRIMARY KEY,
  tenant_id       uuid NOT NULL REFERENCES tenants(id),
  document_id     uuid NOT NULL REFERENCES knowledge_documents(id),
  document_version int NOT NULL,
  original_key    text NOT NULL,
  original_sha256 text NOT NULL,
  media_type      text NOT NULL,
  byte_size       bigint NOT NULL,
  requested_by    text NOT NULL,
  state           text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'succeeded', 'truncated', 'failed')),
  attempts        int NOT NULL DEFAULT 0,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  lease_owner     text,
  lease_until     timestamptz,
  error_code      text,
  error_detail    text,
  notice          text,
  workspace_id    text,            -- the sandbox workspace of the running attempt, so that an attempt that died can be cleaned up
  result_key      text,            -- the staged converted text in the object store, until it is committed
  result_sha256   text,
  result_bytes    int,
  chunk_count     int,
  created_at      timestamptz NOT NULL DEFAULT now(),
  started_at      timestamptz,
  finished_at     timestamptz,
  updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX knowledge_conversions_claim ON knowledge_conversions (next_attempt_at) WHERE state IN ('queued', 'running');
CREATE INDEX knowledge_conversions_document ON knowledge_conversions (document_id, created_at DESC);

CREATE TABLE knowledge_ticket_uses (
  jti         text PRIMARY KEY,
  tenant_id   uuid NOT NULL REFERENCES tenants(id),
  document_id uuid NOT NULL REFERENCES knowledge_documents(id),
  job_id      uuid NOT NULL,
  kind        text NOT NULL CHECK (kind IN ('read', 'write')),
  expires_at  timestamptz NOT NULL,
  used_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX knowledge_ticket_uses_expiry ON knowledge_ticket_uses (expires_at);

ALTER TABLE knowledge_conversions ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge_conversions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON knowledge_conversions USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY converter ON knowledge_conversions USING (current_setting('app.role', true) = 'converter');
ALTER TABLE knowledge_ticket_uses ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge_ticket_uses FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_rows ON knowledge_ticket_uses USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
CREATE POLICY converter ON knowledge_ticket_uses USING (current_setting('app.role', true) = 'converter');

GRANT SELECT, INSERT, UPDATE, DELETE ON knowledge_conversions, knowledge_ticket_uses TO idp_cmi_app;
-- a committed conversion replaces the chunks of its version
GRANT DELETE ON knowledge_chunks TO idp_cmi_app;
