-- idp-cmi schema v20 (ADR-0001 D79): an erasure retires the queued projection events of the objects it erases. The reason
-- is recorded with the event; NULL means it was processed by the projection worker.
ALTER TABLE projection_events ADD COLUMN processed_reason text CHECK (processed_reason IN ('erased'));

-- events that are still queued for an object that is already erased are retired now (the backfill of this rule)
ALTER TABLE projection_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE memory_records NO FORCE ROW LEVEL SECURITY;
ALTER TABLE knowledge_documents NO FORCE ROW LEVEL SECURITY;
UPDATE projection_events p SET processed_at = now(), processed_reason = 'erased'
 WHERE p.processed_at IS NULL AND (
   (p.kind = 'memory' AND EXISTS (SELECT 1 FROM memory_records m WHERE m.id = p.object_id AND m.erasure_id IS NOT NULL))
   OR (p.kind = 'chunk' AND EXISTS (SELECT 1 FROM knowledge_documents d WHERE d.id = p.object_id AND d.erasure_id IS NOT NULL)));
ALTER TABLE projection_events FORCE ROW LEVEL SECURITY;
ALTER TABLE memory_records FORCE ROW LEVEL SECURITY;
ALTER TABLE knowledge_documents FORCE ROW LEVEL SECURITY;
