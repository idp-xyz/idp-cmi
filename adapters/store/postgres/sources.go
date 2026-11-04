package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
)

const sourceCols = `id::text, name, display_name, kind, trust, coalesce(base_url, ''), created_by, created_at`

func scanSource(row pgx.Row) (*cmi.Source, error) {
	var s cmi.Source
	if err := row.Scan(&s.ID, &s.Name, &s.DisplayName, &s.Kind, &s.Trust, &s.BaseURL, &s.CreatedBy, &s.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "source not found")
		}
		return nil, err
	}
	return &s, nil
}

func sourceWhere(ref string) (string, any) {
	if IsUUID(ref) {
		return `id = $1`, ref
	}
	return `name = $1`, ref
}

func getSource(ctx context.Context, tx pgx.Tx, ref string) (*cmi.Source, error) {
	where, arg := sourceWhere(ref)
	return scanSource(tx.QueryRow(ctx, `SELECT `+sourceCols+` FROM sources WHERE `+where, arg))
}

// CreateSource registers a source.
func (s *Store) CreateSource(ctx context.Context, tenant string, in cmi.Source, by string) (*cmi.Source, error) {
	if err := in.Validate(); err != nil {
		return nil, fail("INVALID", "%s", err)
	}
	var out *cmi.Source
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		id := NewUUID()
		var base any
		if in.BaseURL != "" {
			base = strings.TrimSuffix(in.BaseURL, "/")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO sources (id, tenant_id, name, display_name, kind, trust, base_url, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, tenant, in.Name, in.DisplayName, in.Kind, in.Trust, base, by); err != nil {
			if isUniqueViolation(err) {
				return fail("CONFLICT", "a source named %s exists", in.Name)
			}
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "source.create", id, map[string]any{"name": in.Name, "kind": in.Kind, "trust": in.Trust}); err != nil {
			return err
		}
		var err error
		out, err = getSource(ctx, tx, id)
		return err
	})
	return out, err
}

// GetSource returns a source by id or name.
func (s *Store) GetSource(ctx context.Context, tenant, ref string) (*cmi.Source, error) {
	var out *cmi.Source
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		out, err = getSource(ctx, tx, ref)
		return err
	})
	return out, err
}

// ListSources returns the tenant's sources by name.
func (s *Store) ListSources(ctx context.Context, tenant string) ([]cmi.Source, error) {
	out := []cmi.Source{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+sourceCols+` FROM sources ORDER BY name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			x, err := scanSource(rows)
			if err != nil {
				return err
			}
			out = append(out, *x)
		}
		return rows.Err()
	})
	return out, err
}

// Policy is a tenant's CMI policy (D7): sensitivity ceilings per role, default memory TTL, package TTL.
type Policy struct {
	ReaderCeiling           string    `json:"readerCeiling"`
	WriterCeiling           string    `json:"writerCeiling"`
	DefaultMemoryTTLDays    int       `json:"defaultMemoryTtlDays"`
	PackageTTLSeconds       int       `json:"packageTtlSeconds"`
	IncludeFlagged          bool      `json:"includeFlagged"`          // D12: let flagged content into packages (items then carry flags); default false = quarantine
	ConsolidateMinRecords   int       `json:"consolidateMinRecords"`   // D15: episodic records a group needs before it is distilled (default 5)
	ConsolidatedTTLDays     int       `json:"consolidatedTtlDays"`     // D15: validity of distilled statements (default 90)
	ConsolidatedAsCandidate bool      `json:"consolidatedAsCandidate"` // D21: distilled statements wait for an administrator\'s admission
	ExternalModelCeiling    string    `json:"externalModelCeiling"`    // D29: content above this level never reaches an external model (no vectors, no consolidation)
	UpdatedBy               string    `json:"updatedBy,omitempty"`
	UpdatedAt               time.Time `json:"updatedAt,omitzero"`
	Default                 bool      `json:"default"`
}

// DefaultPolicy applies to tenants without a stored policy.
var DefaultPolicy = Policy{ReaderCeiling: cmi.SensitivityInternal, WriterCeiling: cmi.SensitivityConfidential, PackageTTLSeconds: 3600, ConsolidateMinRecords: 5, ConsolidatedTTLDays: 90, ExternalModelCeiling: cmi.SensitivityInternal, Default: true}

// Validate checks a policy.
func (p Policy) Validate() error {
	if !cmi.ValidSensitivity(p.ReaderCeiling) || !cmi.ValidSensitivity(p.WriterCeiling) || !cmi.ValidSensitivity(p.ExternalModelCeiling) {
		return fail("INVALID", "ceilings must be public, internal, confidential or restricted")
	}
	if p.DefaultMemoryTTLDays < 0 || p.DefaultMemoryTTLDays > 3650 || p.PackageTTLSeconds < 60 || p.PackageTTLSeconds > 604800 {
		return fail("INVALID", "defaultMemoryTtlDays must be within [0, 3650], packageTtlSeconds within [60, 604800]")
	}
	if p.ConsolidateMinRecords < 2 || p.ConsolidateMinRecords > 100 || p.ConsolidatedTTLDays < 1 || p.ConsolidatedTTLDays > 3650 {
		return fail("INVALID", "consolidateMinRecords must be within [2, 100], consolidatedTtlDays within [1, 3650]")
	}
	return nil
}

func policyOf(ctx context.Context, tx pgx.Tx, tenant string) (Policy, error) {
	var p Policy
	var by *string
	err := tx.QueryRow(ctx, `SELECT reader_ceiling, writer_ceiling, default_memory_ttl_days, package_ttl_seconds, include_flagged, consolidate_min_records, consolidated_ttl_days, consolidated_as_candidate, external_model_ceiling, updated_by, updated_at FROM tenant_policies WHERE tenant_id = $1`, tenant).
		Scan(&p.ReaderCeiling, &p.WriterCeiling, &p.DefaultMemoryTTLDays, &p.PackageTTLSeconds, &p.IncludeFlagged, &p.ConsolidateMinRecords, &p.ConsolidatedTTLDays, &p.ConsolidatedAsCandidate, &p.ExternalModelCeiling, &by, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultPolicy, nil
	}
	if err != nil {
		return p, err
	}
	if by != nil {
		p.UpdatedBy = *by
	}
	return p, nil
}

// GetPolicy returns the tenant's policy.
func (s *Store) GetPolicy(ctx context.Context, tenant string) (*Policy, error) {
	var p Policy
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		p, err = policyOf(ctx, tx, tenant)
		return err
	})
	return &p, err
}

// SetPolicy stores the tenant's policy.
func (s *Store) SetPolicy(ctx context.Context, tenant string, p Policy, by string) (*Policy, error) {
	if p.ConsolidateMinRecords == 0 {
		p.ConsolidateMinRecords = DefaultPolicy.ConsolidateMinRecords
	}
	if p.ConsolidatedTTLDays == 0 {
		p.ConsolidatedTTLDays = DefaultPolicy.ConsolidatedTTLDays
	}
	if p.ExternalModelCeiling == "" {
		p.ExternalModelCeiling = DefaultPolicy.ExternalModelCeiling
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	var out Policy
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenant_policies (tenant_id, reader_ceiling, writer_ceiling, default_memory_ttl_days, package_ttl_seconds, include_flagged, consolidate_min_records, consolidated_ttl_days, consolidated_as_candidate, external_model_ceiling, updated_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (tenant_id) DO UPDATE SET reader_ceiling = EXCLUDED.reader_ceiling, writer_ceiling = EXCLUDED.writer_ceiling, default_memory_ttl_days = EXCLUDED.default_memory_ttl_days,
			  package_ttl_seconds = EXCLUDED.package_ttl_seconds, include_flagged = EXCLUDED.include_flagged, consolidate_min_records = EXCLUDED.consolidate_min_records,
			  consolidated_ttl_days = EXCLUDED.consolidated_ttl_days, consolidated_as_candidate = EXCLUDED.consolidated_as_candidate, external_model_ceiling = EXCLUDED.external_model_ceiling, updated_by = EXCLUDED.updated_by, updated_at = now()`,
			tenant, p.ReaderCeiling, p.WriterCeiling, p.DefaultMemoryTTLDays, p.PackageTTLSeconds, p.IncludeFlagged, p.ConsolidateMinRecords, p.ConsolidatedTTLDays, p.ConsolidatedAsCandidate, p.ExternalModelCeiling, by); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "policy.set", "", p); err != nil {
			return err
		}
		// D29: vectors of content now above the external-model ceiling are removed
		if _, err := tx.Exec(ctx, `UPDATE memory_records SET embedding = NULL, embedding_model = NULL WHERE embedding IS NOT NULL AND `+sensitivityRankSQL("sensitivity")+` > `+sensitivityRankSQL("$1"), p.ExternalModelCeiling); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE knowledge_chunks c SET embedding = NULL, embedding_model = NULL FROM knowledge_documents d WHERE d.id = c.document_id AND c.embedding IS NOT NULL AND `+sensitivityRankSQL("d.sensitivity")+` > `+sensitivityRankSQL("$1"), p.ExternalModelCeiling); err != nil {
			return err
		}
		var err error
		out, err = policyOf(ctx, tx, tenant)
		return err
	})
	return &out, err
}

// sensitivityRankSQL ranks a level expression in SQL (public 0 … restricted 3).
func sensitivityRankSQL(expr string) string {
	return `(CASE ` + expr + ` WHEN 'public' THEN 0 WHEN 'internal' THEN 1 WHEN 'confidential' THEN 2 ELSE 3 END)`
}
