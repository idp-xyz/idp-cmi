-- KB8 review (ADR-0001 D76): two columns.
--
-- * knowledge_documents.in_force_since: the moment a version became active (D52, D64). A view by time (asOf) shows a version, and
--   lets it replace its predecessor, only from that moment; NULL means the version has never been in force and so is never
--   shown and replaces nothing. A trigger keeps it: a version written active is in force from its creation; one that waits
--   for its text (status pending) has none, and gets one when it becomes active; a version that is made pending loses it.
-- * context_packages.consulted: the versions of documents a read or a find looked into, kept even when the package has no
--   item, so that erasing a document finds the packages that consulted it (their query names what was asked for).
--
-- The backfill reads every tenant's rows: FORCE row-level security is switched off for this transaction on the tables it
-- touches, as in 0013, and back on before the end.
--
-- How the backfill decides (a version that was in force has a moment; one that never was has none):
--   * not pending and not revoked (active, superseded, expired): created_at, the moment it was written active;
--   * pending: NULL, it is waiting for its text;
--   * revoked: created_at when it was in force at some time - it has no conversion job, or one of its jobs succeeded or
--     was truncated (the text was committed, so it was active, and it was withdrawn afterwards); NULL when it has jobs and
--     none of them committed (it was withdrawn while pending, its placeholder text was never in force).
-- A version made active by a conversion before this migration has created_at here, not its commit time: the history that
-- would say otherwise was never kept, and it only affects views of times inside a conversion that has already finished.

ALTER TABLE knowledge_documents NO FORCE ROW LEVEL SECURITY;
ALTER TABLE knowledge_conversions NO FORCE ROW LEVEL SECURITY;

ALTER TABLE knowledge_documents ADD COLUMN in_force_since timestamptz;

-- backfill begin
UPDATE knowledge_documents d SET in_force_since = d.created_at
WHERE d.status <> 'pending'
  AND (d.status <> 'revoked'
       OR NOT EXISTS (SELECT 1 FROM knowledge_conversions j WHERE j.document_id = d.id)
       OR EXISTS (SELECT 1 FROM knowledge_conversions j WHERE j.document_id = d.id AND j.state IN ('succeeded', 'truncated')));
-- backfill end

CREATE FUNCTION knowledge_documents_in_force() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.status <> 'pending' AND NEW.in_force_since IS NULL THEN
      NEW.in_force_since := NEW.created_at;
    END IF;
  ELSIF NEW.status IS DISTINCT FROM OLD.status THEN
    IF NEW.status = 'pending' THEN
      NEW.in_force_since := NULL;
    ELSIF OLD.status = 'pending' AND NEW.status IN ('active', 'superseded') THEN
      NEW.in_force_since := now();
    END IF;
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER knowledge_documents_in_force BEFORE INSERT OR UPDATE ON knowledge_documents
  FOR EACH ROW EXECUTE FUNCTION knowledge_documents_in_force();

ALTER TABLE knowledge_documents FORCE ROW LEVEL SECURITY;
ALTER TABLE knowledge_conversions FORCE ROW LEVEL SECURITY;

ALTER TABLE context_packages ADD COLUMN consulted uuid[] NOT NULL DEFAULT '{}';
CREATE INDEX context_packages_consulted ON context_packages USING gin (consulted);
