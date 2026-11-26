-- KB8 review (ADR-0001 D76, D77): four columns. (This file was amended in place after review: nothing outside test databases ever
-- applied an earlier form of it.)
--
-- * knowledge_documents.in_force_since: the moment a version became active (D52, D64). A view by time (asOf) shows a version, and
--   lets it replace its predecessor, only from that moment; NULL means the version has never been in force and so is never
--   shown and replaces nothing. Once set it is never cleared: a version that was in force keeps its past (D77: such a version
--   takes no new source text either). A trigger sets it: a version written active is in force from its creation; one written
--   pending (it waits for the text of its original) has none, and gets one when its text is committed and it becomes active.
-- * context_packages.consulted: the versions of documents a read or a find looked into (the version read, and the id the caller
--   named when that was another), kept even when the package has no item, so that erasing a document finds the packages that
--   consulted it (their query names what was asked for).
-- * knowledge_chunks.cut_after: the chunk ends at a hard cut of a long line, so the text goes on in the next chunk with nothing
--   between (D77). A find joins the two chunks of a seam with nothing there when it is true, with one space when it is false, and
--   tries both when it is NULL (unknown: a row written before this release that the inference below takes for a cut). The chunker
--   marks a cut only when neither character next to it is white space; a cut next to a space is false (the space is one space).
-- * knowledge_documents.text_original_sha256: the digest of the original the version's extracted text came from. A version that
--   holds extracted text is converted again only from that original, and takes no other one (D77).
--
-- The backfills read every tenant's rows: FORCE row-level security is switched off for this transaction on the tables they
-- touch, as in 0013, and back on before the end.
--
-- How the backfills decide:
--   * in_force_since: a version that is pending now is NULL (it is waiting for its text); every other version, revoked ones
--     included, gets created_at. Before this release every version was written active and a PDF or Office original then made it
--     pending, so the data cannot tell a revoked version that was withdrawn while it waited from one that was in force and was
--     withdrawn later; when the data cannot tell, the version is taken to have been in force (D77). A version made active by a
--     conversion before this migration has created_at, not its commit time: that history was never kept.
--   * cut_after: inferred, and unknown (NULL) where the inference says cut, false everywhere else. A chunk of prose that is nearly as long as the size limit (1497 bytes or more), is not a row of a table
--     or a worksheet, and is followed by a chunk of the same section, was cut where the size was reached. A chunk that fills the
--     limit by chance is taken for a cut as well; the cost is that a find tries the seam both ways.

ALTER TABLE knowledge_documents NO FORCE ROW LEVEL SECURITY;

ALTER TABLE knowledge_documents ADD COLUMN in_force_since timestamptz;

-- backfill begin
UPDATE knowledge_documents d SET in_force_since = d.created_at WHERE d.status <> 'pending';
-- backfill end

ALTER TABLE knowledge_documents ADD COLUMN text_original_sha256 text;
-- the original a converted version has now is the one its text came from, as far as the data says
UPDATE knowledge_documents SET text_original_sha256 = original_sha256 WHERE text_source = 'extracted';

CREATE FUNCTION knowledge_documents_in_force() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'pending' AND NEW.in_force_since IS NULL THEN
      NEW.in_force_since := NEW.created_at;
    END IF;
  ELSIF OLD.status = 'pending' AND NEW.status IN ('active', 'superseded') THEN
    NEW.in_force_since := coalesce(OLD.in_force_since, now());
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER knowledge_documents_in_force BEFORE INSERT OR UPDATE ON knowledge_documents
  FOR EACH ROW EXECUTE FUNCTION knowledge_documents_in_force();

ALTER TABLE knowledge_chunks NO FORCE ROW LEVEL SECURITY;
ALTER TABLE knowledge_chunks ADD COLUMN cut_after boolean DEFAULT false;
-- cut backfill begin
UPDATE knowledge_chunks c SET cut_after = NULL
WHERE octet_length(c.content) >= 1497 AND c.table_name = '' AND c.sheet = ''
  AND EXISTS (SELECT 1 FROM knowledge_chunks n WHERE n.document_id = c.document_id AND n.seq = c.seq + 1 AND n.heading_path = c.heading_path
              AND n.page IS NOT DISTINCT FROM c.page AND n.slide IS NOT DISTINCT FROM c.slide AND n.table_name = '' AND n.sheet = '');
-- cut backfill end
ALTER TABLE knowledge_chunks FORCE ROW LEVEL SECURITY;

ALTER TABLE knowledge_documents FORCE ROW LEVEL SECURITY;

ALTER TABLE context_packages ADD COLUMN consulted uuid[] NOT NULL DEFAULT '{}';
CREATE INDEX context_packages_consulted ON context_packages USING gin (consulted);
