-- idp-cmi schema v11 (ADR-0001 D41): facts. A fact is a memory record whose structured part names a key and a value
-- ({"fact": {"key": "...", "value": "..."}}); writing a new value closes the validity of the current one in the same
-- transaction. The index finds the current fact of a (scope, key) without scanning the scope.
CREATE INDEX memory_records_fact_key ON memory_records (tenant_id, scope_kind, scope_id, ((structured->'fact'->>'key')))
  WHERE structured ? 'fact' AND status = 'active';
