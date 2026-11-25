-- Locator columns for structured chunks. index_text is what search and embedding use;
-- content stays the body shown to a model. Existing rows keep indexing their body.
-- embed_error records why a chunk could not be embedded; such a chunk is skipped by the backfill
-- and the projector instead of stalling them. The statements that touch existing rows run with FORCE
-- row-level security off for this transaction, as in 0013, so a schema owner that is not a superuser sees them.

ALTER TABLE knowledge_chunks NO FORCE ROW LEVEL SECURITY;

ALTER TABLE knowledge_chunks ADD COLUMN heading_path text NOT NULL DEFAULT '';
ALTER TABLE knowledge_chunks ADD COLUMN page int;
ALTER TABLE knowledge_chunks ADD COLUMN table_name text NOT NULL DEFAULT '';
ALTER TABLE knowledge_chunks ADD COLUMN index_text text;
-- embed_error: the embedder refused this text itself (a permanent, input error); it is skipped for the model named in
-- embed_error_model and tried again when the model changes. embed_attempts / embed_retry_at: a transient failure is retried
-- later, with a growing wait.
ALTER TABLE knowledge_chunks ADD COLUMN embed_error text;
ALTER TABLE knowledge_chunks ADD COLUMN embed_error_model text;
ALTER TABLE knowledge_chunks ADD COLUMN embed_attempts int NOT NULL DEFAULT 0;
ALTER TABLE knowledge_chunks ADD COLUMN embed_retry_at timestamptz;

UPDATE knowledge_chunks SET index_text = content WHERE index_text IS NULL;
ALTER TABLE knowledge_chunks ALTER COLUMN index_text SET NOT NULL;

ALTER TABLE knowledge_chunks FORCE ROW LEVEL SECURITY;
