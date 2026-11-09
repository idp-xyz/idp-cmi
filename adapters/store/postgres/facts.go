package postgres

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
)

// Facts (ADR-0001 D41): a memory record with a key and a value, of which at most one is current for a (scope, key).
// Writing a new value closes the validity of the current one - valid_until = the new one's valid_from, the status stays
// active - and inserts the new record, in one transaction under an advisory lock on (tenant, scope, key). The history
// stays: the bitemporal queries of D25 still show the value that held on a past date, while the usable predicate (valid
// until in the future) sees only the current value.

// FactKeyRE is the shape of a fact key.
var FactKeyRE = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,62}$`)

// MaxFactValue bounds a value.
const MaxFactValue = 500

// FactOutcome says what an upsert did.
type FactOutcome struct {
	Outcome string            `json:"outcome"` // created (no current value), updated (the old one was closed), unchanged (same value)
	Record  *cmi.MemoryRecord `json:"record"`
	Closed  []string          `json:"closed,omitempty"` // record ids whose validity was closed
}

func normalizeValue(v string) string { return strings.ToLower(strings.Join(strings.Fields(v), " ")) }

// UpsertFact writes the fact in `in` (its scope, content, provenance, sensitivity ...) under key and value.
func (s *Store) UpsertFact(ctx context.Context, tenant string, in cmi.MemoryRecord, key, value, by, ceiling string) (*FactOutcome, error) {
	value = strings.TrimSpace(value)
	if !FactKeyRE.MatchString(key) {
		return nil, fail("INVALID", "key must match %s", FactKeyRE)
	}
	if value == "" || utf8.RuneCountInString(value) > MaxFactValue {
		return nil, fail("INVALID", "value must be 1 to %d characters", MaxFactValue)
	}
	if err := in.Scope.Validate(); err != nil {
		return nil, fail("INVALID", "%s", err)
	}
	if in.Validity.ValidFrom.IsZero() {
		in.Validity.ValidFrom = s.now()
	}
	if !in.Validity.ValidUntil.IsZero() {
		return nil, fail("INVALID", "a fact has no end date of its own: the next value closes it")
	}
	in.Kind = cmi.KindSemantic
	structured, _ := json.Marshal(map[string]any{"fact": map[string]string{"key": key, "value": value}})
	in.Structured = structured
	var out *FactOutcome
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, tenant+"|"+in.Scope.Kind+"|"+in.Scope.ID+"|"+key); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id::text, valid_from, structured->'fact'->>'value' FROM memory_records
			WHERE scope_kind = $1 AND scope_id = $2 AND status = 'active' AND erasure_id IS NULL AND structured->'fact'->>'key' = $3
			  AND (valid_until IS NULL OR valid_until > $4) ORDER BY valid_from DESC`, in.Scope.Kind, in.Scope.ID, key, in.Validity.ValidFrom)
		if err != nil {
			return err
		}
		type cur struct {
			id    string
			from  time.Time
			value string
		}
		var current []cur
		for rows.Next() {
			var c cur
			if err := rows.Scan(&c.id, &c.from, &c.value); err != nil {
				rows.Close()
				return err
			}
			current = append(current, c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(current) > 0 && normalizeValue(current[0].value) == normalizeValue(value) {
			rec, err := scanMemory(tx.QueryRow(ctx, `SELECT `+memoryCols+` FROM memory_records m JOIN sources s ON s.id = m.source_id WHERE m.id = $1`, current[0].id))
			if err != nil {
				return err
			}
			out = &FactOutcome{Outcome: "unchanged", Record: rec}
			return nil
		}
		if len(current) > 0 && current[0].from.After(in.Validity.ValidFrom) {
			return fail("CONFLICT", "the current value of %q is newer than this statement (valid from %s)", key, current[0].from.UTC().Format(time.RFC3339))
		}
		var closed []string
		for _, c := range current {
			closed = append(closed, c.id)
		}
		if len(closed) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE memory_records SET valid_until = $2, version = version + 1 WHERE id = ANY($1::uuid[])`, closed, in.Validity.ValidFrom); err != nil {
				return err
			}
			if err := Audit(ctx, tx, tenant, by, "memory.fact.close", key, map[string]any{"scope": in.Scope, "closed": closed}); err != nil {
				return err
			}
		}
		rec, err := s.insertMemory(ctx, tx, tenant, in, by, ceiling)
		if err != nil {
			return err
		}
		out = &FactOutcome{Outcome: "created", Record: rec, Closed: closed}
		if len(closed) > 0 {
			out.Outcome = "updated"
		}
		return nil
	})
	return out, err
}
