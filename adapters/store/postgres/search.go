package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
	"idp-cmi/internal/embedding"
)

// Query is one retrieval: the lexical query, the query vector (with the model that made it), the
// entity filter ("type:id" keys, any-of), whether flagged content may be returned, and the limit per
// retriever. Either TSQuery or Vector may be empty.
type Query struct {
	TSQuery        string
	Vector         []float32
	Model          string
	Entities       []string
	IncludeFlagged bool
	Limit          int
}

// Hit is how a retriever matched: lexical rank (ts_rank), semantic rank (cosine) and the summary.
type Hit struct {
	Lexical  float64
	Semantic float64
	Match    string
}

func (h *Hit) merge(other Hit) {
	if other.Lexical > h.Lexical {
		h.Lexical = other.Lexical
	}
	if other.Semantic > h.Semantic {
		h.Semantic = other.Semantic
	}
	switch {
	case h.Lexical > 0 && h.Semantic > 0:
		h.Match = cmi.MatchBoth
	case h.Semantic > 0:
		h.Match = cmi.MatchSemantic
	default:
		h.Match = cmi.MatchLexical
	}
}

// entityFilterSQL keeps rows that reference any of the requested entities (no entities = no filter).
// $kind is the object kind, $col the object id column, $n the text[] parameter.
func entityFilterSQL(kind, col, param string) string {
	return ` AND (cardinality(` + param + `::text[]) = 0 OR EXISTS (SELECT 1 FROM entity_refs e WHERE e.object_kind = '` + kind + `' AND e.object_id = ` + col +
		` AND (e.entity_type || ':' || e.entity_id) = ANY(` + param + `::text[])))`
}

// entitiesSQL is the aggregated entity list of an object as a JSON text column.
func entitiesSQL(kind, col string) string {
	return `(SELECT coalesce(json_agg(json_build_object('type', e.entity_type, 'id', e.entity_id) ORDER BY e.entity_type, e.entity_id), '[]')::text FROM entity_refs e WHERE e.object_kind = '` + kind + `' AND e.object_id = ` + col + `)`
}

func decodeEntities(raw string) []cmi.Entity {
	var out []cmi.Entity
	_ = json.Unmarshal([]byte(raw), &out)
	if len(out) == 0 {
		return nil
	}
	return out
}

func insertEntities(ctx context.Context, tx pgx.Tx, tenant, kind, id string, es []cmi.Entity) error {
	if len(es) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, e := range es {
		b.Queue(`INSERT INTO entity_refs (tenant_id, object_kind, object_id, entity_type, entity_id) VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`, tenant, kind, id, e.Type, e.ID)
	}
	return tx.SendBatch(ctx, b).Close()
}

// embed vectorizes texts with the store's embedder; nil (and no error) when none is configured or
// the endpoint fails - the write goes through without vectors and the backfill fills them later.
func (s *Store) embed(ctx context.Context, texts []string) ([][]float32, string) {
	if s.Embedder == nil || len(texts) == 0 {
		return nil, ""
	}
	vecs, err := s.Embedder.Embed(ctx, texts)
	if err != nil || len(vecs) != len(texts) {
		if s.Log != nil {
			s.Log.Warn("embedding failed; rows left for the backfill", "error", err)
		}
		return nil, ""
	}
	return vecs, s.Embedder.Model()
}

var _ embedding.Embedder = (*embedding.Fake)(nil)
