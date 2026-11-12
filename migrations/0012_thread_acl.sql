-- idp-cmi schema v12 (ADR-0001 D44, D45; security review 2026-11-10 O1, O3).
--
-- D44: a thread's memory written without an ACL was readable by every reader of the tenant who named the thread, and a
-- thread id is in runs, packages and logs. New records get their writer and the administrators as their ACL; the ones
-- written before are given the same here: the subjects that wrote to that thread (the consolidation and evaluation
-- workers are not readers), and the administrator role.
--
-- D45: connector headers that look like credentials are refused from now on; the ones stored before are redacted here
-- (the name is kept, so an administrator sees what to remove; the ingester refuses such a connector). The pattern is
-- internal/cmi/credentials.go's.
--
-- Row-level security is forced on both tables, also for the owner, so each tenant is bound in turn; a tenant where
-- anything changed gets an audit event saying how much.
DO $$
DECLARE
  t uuid;
  narrowed int;
  redacted int;
BEGIN
  FOR t IN SELECT id FROM tenants LOOP
    PERFORM set_config('app.tenant_id', t::text, true);

    WITH writers AS (
      SELECT scope_id,
             coalesce(array_agg(DISTINCT created_by ORDER BY created_by)
                      FILTER (WHERE created_by NOT IN ('idp-cmi/consolidation', 'idp-cmi/evaluation')), '{}') AS subjects
      FROM memory_records
      WHERE scope_kind = 'thread' AND cardinality(acl_subjects) = 0 AND cardinality(acl_roles) = 0
      GROUP BY scope_id)
    UPDATE memory_records m SET acl_subjects = w.subjects, acl_roles = ARRAY['cmi-admin']
    FROM writers w
    WHERE m.scope_kind = 'thread' AND m.scope_id = w.scope_id AND cardinality(m.acl_subjects) = 0 AND cardinality(m.acl_roles) = 0;
    GET DIAGNOSTICS narrowed = ROW_COUNT;

    UPDATE connectors SET config = jsonb_set(config, '{headers}', (
        SELECT coalesce(jsonb_agg(CASE
                 WHEN lower(trim(split_part(h, ':', 1))) ~ '(auth|token|secret|key|cookie|session|pass|credential|signature|csrf|xsrf|jwt|bearer)'
                   OR lower(trim(substr(h, position(':' in h) + 1))) ~ '^(bearer|basic|digest|token|negotiate)\s'
                 THEN to_jsonb(trim(split_part(h, ':', 1)) || ': <redacted>') ELSE to_jsonb(h) END ORDER BY o), '[]'::jsonb)
        FROM jsonb_array_elements_text(config->'headers') WITH ORDINALITY AS x(h, o)))
    WHERE jsonb_typeof(config->'headers') = 'array'
      AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(config->'headers') AS y(h)
                  WHERE (lower(trim(split_part(h, ':', 1))) ~ '(auth|token|secret|key|cookie|session|pass|credential|signature|csrf|xsrf|jwt|bearer)'
                         OR lower(trim(substr(h, position(':' in h) + 1))) ~ '^(bearer|basic|digest|token|negotiate)\s')
                    AND h NOT LIKE '%: <redacted>');
    GET DIAGNOSTICS redacted = ROW_COUNT;

    IF narrowed > 0 OR redacted > 0 THEN
      INSERT INTO audit_events (tenant_id, subject, action, detail)
      VALUES (t, 'idp-cmi/migrate', 'migration.0012_thread_acl', jsonb_build_object('threadRecordsGivenAnACL', narrowed, 'connectorsWithHeadersRedacted', redacted));
    END IF;
  END LOOP;
  PERFORM set_config('app.tenant_id', '', true);
END $$;
