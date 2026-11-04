-- idp-cmi schema v6 (ADR-0001 D21): candidate and disputed memory, transitions with reasons.
ALTER TABLE memory_records DROP CONSTRAINT memory_records_status_check;
ALTER TABLE memory_records ADD CONSTRAINT memory_records_status_check CHECK (status IN ('candidate', 'active', 'disputed', 'expired', 'revoked'));
ALTER TABLE memory_records ADD COLUMN disputed_by uuid;   -- the record whose structured.contradicts named this one
ALTER TABLE tenant_policies ADD COLUMN consolidated_as_candidate boolean NOT NULL DEFAULT false;
CREATE INDEX memory_records_review ON memory_records (tenant_id, status) WHERE status IN ('candidate', 'disputed');
