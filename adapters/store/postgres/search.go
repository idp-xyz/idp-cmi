package postgres

import (
	"context"
	"encoding/json"
	"errors"

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
	AsOf           *cmi.AsOf // a bitemporal view instead of "now" (D25)
	// KnowledgeBases and DocTypes narrow the knowledge items to those bases (by name) and document types; empty = no filter.
	// They narrow what is visible, never widen it (D72).
	KnowledgeBases []string
	DocTypes       []string
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

// excludeSQL drops rows of the sources a PDP obligation excludes (D23); $param is a text[].
func excludeSQL(param string) string {
	return ` AND NOT (s.name = ANY(` + param + `::text[]))`
}

func excluded(v Visibility) []string {
	if v.ExcludeSources == nil {
		return []string{}
	}
	return v.ExcludeSources
}

// baseDocTypeSQL keeps knowledge rows in the named bases and of the named document types ($kb and $dt are text[] parameters;
// an empty array = no filter).
func baseDocTypeSQL(kb, dt string) string {
	return ` AND (cardinality(` + kb + `::text[]) = 0 OR EXISTS (SELECT 1 FROM knowledge_bases kbn WHERE kbn.id = d.knowledge_base_id AND kbn.name = ANY(` + kb + `)))` +
		` AND (cardinality(` + dt + `::text[]) = 0 OR d.doc_type = ANY(` + dt + `))`
}

func orEmpty(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
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
// EmbedBatch is how many texts go to the embedder in one call.
const EmbedBatch = 64

func (s *Store) embed(ctx context.Context, texts []string) ([][]float32, string) {
	if s.Embedder == nil || len(texts) == 0 {
		return nil, ""
	}
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += EmbedBatch {
		end := min(i+EmbedBatch, len(texts))
		vecs, err := s.Embedder.Embed(ctx, texts[i:end])
		if err != nil || len(vecs) != end-i {
			if s.Log != nil {
				s.Log.Warn("embedding failed; rows left for the backfill", "error", err)
			}
			return nil, ""
		}
		out = append(out, vecs...)
	}
	return out, s.Embedder.Model()
}

// noteEndpointDown says, when the embedder answered every text with a refusal of the caller (401, 403, 404...), that it is
// refusing: a log line at error level and the "refused" count, so the outage is seen rather than only retried.
func (s *Store) noteEndpointDown(err error) {
	if !embedding.Refused(err) {
		return
	}
	if s.Log != nil {
		s.Log.Error("the embedding endpoint is refusing requests (check its credential and address); rows wait for the next round", "error", err)
	}
	if s.OnEmbedFailure != nil {
		s.OnEmbedFailure("refused")
	}
}

// embedFail is why one text could not be embedded. Permanent is an input error (the endpoint refused the text itself: it
// will refuse it again); anything else is transient and the text is tried again later (D58).
type embedFail struct {
	Reason    string
	Permanent bool
	Err       error
}

// embedEach embeds texts so that one text the embedder cannot take does not keep the others from being embedded. It tries
// the batch; on a failure it tries each text alone. vecs[i] is nil for a text that failed, with its failure in fails[i].
// When no text succeeds alone and some failure is transient the endpoint is taken to be down, and the error is returned:
// nothing is recorded for that. (When every failure is permanent they are all recorded: the texts are the problem.)
func (s *Store) embedEach(ctx context.Context, texts []string) (vecs [][]float32, fails []embedFail, err error) {
	vecs = make([][]float32, len(texts))
	fails = make([]embedFail, len(texts))
	if len(texts) == 0 {
		return vecs, fails, nil
	}
	got, berr := s.Embedder.Embed(ctx, texts)
	if berr == nil && len(got) == len(texts) {
		return got, fails, nil
	}
	if berr == nil {
		berr = errors.New("the embedder returned the wrong number of vectors")
	}
	if len(texts) == 1 && !embedding.Permanent(berr) {
		s.noteEndpointDown(berr)
		return nil, nil, berr
	}
	ok, transient := 0, 0
	var last error
	for i, t := range texts {
		if len(texts) == 1 {
			fails[i] = embedFail{Reason: berr.Error(), Permanent: true, Err: berr}
			break
		}
		one, e := s.Embedder.Embed(ctx, []string{t})
		if e == nil && len(one) != 1 {
			e = errors.New("the embedder returned no vector")
		}
		if e != nil {
			fails[i] = embedFail{Reason: e.Error(), Permanent: embedding.Permanent(e), Err: e}
			if !fails[i].Permanent {
				transient++
			}
			last = e
			continue
		}
		vecs[i] = one[0]
		ok++
	}
	if ok == 0 && transient > 0 {
		s.noteEndpointDown(last)
		return nil, nil, last
	}
	return vecs, fails, nil
}

var _ embedding.Embedder = (*embedding.Fake)(nil)
