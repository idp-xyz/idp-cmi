-- Document validity window, category, owner and review date.
-- valid_from is NULL for a version written without one, and NULL means "from the beginning": the
-- bitemporal view of such a document is what it was before this column existed. Existing rows stay NULL.

ALTER TABLE knowledge_documents ADD COLUMN valid_from timestamptz;
ALTER TABLE knowledge_documents ADD COLUMN doc_type text NOT NULL DEFAULT '';
ALTER TABLE knowledge_documents ADD COLUMN owner text NOT NULL DEFAULT '';
ALTER TABLE knowledge_documents ADD COLUMN review_due timestamptz;
