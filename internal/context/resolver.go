// Package context assembles context packages (ADR-0001 D5): retrieve what the principal may see,
// drop what is unusable, order by trust then freshness then relevance, pack within the budget,
// seal and store. The resolver never calls a model or an external service.
package context

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/cmi"
	"idp-cmi/internal/embedding"
)

// Request is a resolve call.
type Request struct {
	Query              string       `json:"query"`
	Purpose            string       `json:"purpose,omitempty"`
	Scope              *cmi.Scope   `json:"scope,omitempty"`   // the thread / agent / subject the caller works in
	Scopes             []cmi.Scope  `json:"scopes,omitempty"`  // more of them (D22); unit scopes need membership
	AsOf               *cmi.AsOf    `json:"asOf,omitempty"`    // a bitemporal view (D25): what was valid and known at those times
	Budget             *cmi.Budget  `json:"budget,omitempty"`  // maxTokens (default 2000, at most 32000), maxItems (default 20, at most 100)
	Sources            []string     `json:"sources,omitempty"` // knowledge sources to consider (empty = all visible)
	Kinds              []string     `json:"kinds,omitempty"`   // memory | knowledge (empty = both)
	MaxAgeDays         int          `json:"maxAgeDays,omitempty"`
	SensitivityCeiling string       `json:"sensitivityCeiling,omitempty"` // may only lower the principal's ceiling
	Entities           []cmi.Entity `json:"entities,omitempty"`           // only content referencing one of these business objects (D13)
}

// Validate checks a request.
func (r *Request) Validate() error {
	if strings.TrimSpace(r.Query) == "" || len(r.Query) > 4096 {
		return errors.New("query is required (at most 4096 characters)")
	}
	if len(r.Purpose) > 200 {
		return errors.New("purpose is too long")
	}
	if r.Scope != nil {
		if err := r.Scope.Validate(); err != nil {
			return err
		}
	}
	if len(r.Scopes) > 32 {
		return errors.New("at most 32 scopes")
	}
	for _, sc := range r.Scopes {
		if err := sc.Validate(); err != nil {
			return err
		}
	}
	if r.Budget != nil && (r.Budget.MaxTokens < 0 || r.Budget.MaxTokens > 32000 || r.Budget.MaxItems < 0 || r.Budget.MaxItems > 100) {
		return errors.New("budget.maxTokens must be within [0, 32000] and maxItems within [0, 100]")
	}
	for _, s := range r.Sources {
		if !cmi.ValidName(s) {
			return errors.New("sources entries must be source names")
		}
	}
	for _, k := range r.Kinds {
		if k != cmi.ItemMemory && k != cmi.ItemKnowledge {
			return errors.New("kinds entries must be memory or knowledge")
		}
	}
	if r.MaxAgeDays < 0 || r.MaxAgeDays > 3650 {
		return errors.New("maxAgeDays out of range")
	}
	if r.SensitivityCeiling != "" && !cmi.ValidSensitivity(r.SensitivityCeiling) {
		return errors.New("sensitivityCeiling is not a level")
	}
	return cmi.ValidateEntities(r.Entities)
}

// SemanticFloor is the cosine a candidate found by the vector retriever alone must reach to be
// considered (D11, tuned on the labelled set of D27): below it, nearest neighbours are noise that
// would crowd out lexical hits within the budget.
const SemanticFloor = 0.35

// Resolver assembles packages. With an Embedder the query is also matched semantically (D11).
type Resolver struct {
	Store    *postgres.Store
	Embedder embedding.Embedder
	Now      func() time.Time
	NewID    func() string
	Log      *slog.Logger
}

var tokenRE = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// TSQuery turns free text into an OR query of its words and CJK bigrams for the 'simple' index.
func TSQuery(query string) string {
	seen := map[string]bool{}
	var terms []string
	add := func(t string) {
		t = strings.ToLower(t)
		if t == "" || seen[t] {
			return
		}
		seen[t] = true
		terms = append(terms, "'"+strings.ReplaceAll(t, "'", "''")+"'")
	}
	for _, w := range tokenRE.FindAllString(query, -1) {
		cjk := false
		for _, r := range w {
			if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
				cjk = true
			}
		}
		if cjk {
			for _, bg := range strings.Fields(cmi.Bigrams(w)) {
				add(bg)
			}
			continue
		}
		if len(w) >= 2 {
			add(w)
		}
	}
	if len(terms) > 64 {
		terms = terms[:64]
	}
	return strings.Join(terms, " | ")
}

type candidate struct {
	item     cmi.Item
	trust    int // 0 authoritative, 1 informational
	fresh    float64
	lexical  float64
	semantic float64
	dedupe   string
	created  time.Time
}

// Resolve assembles, seals and stores a package for the principal.
func (r *Resolver) Resolve(ctx context.Context, tenant string, v postgres.Visibility, req Request) (*cmi.Package, *cmi.Envelope, error) {
	if err := req.Validate(); err != nil {
		return nil, nil, err
	}
	now := r.now()
	at := now // the business time items must be valid at
	if req.AsOf != nil {
		if err := req.AsOf.Validate(now); err != nil {
			return nil, nil, err
		}
		req.AsOf.Normalize(now)
		at = req.AsOf.Valid
	}
	if req.SensitivityCeiling != "" && cmi.Within(req.SensitivityCeiling, v.Ceiling) {
		v.Ceiling = req.SensitivityCeiling
	}
	budget := cmi.Budget{MaxTokens: 2000, MaxItems: 20}
	if req.Budget != nil {
		if req.Budget.MaxTokens > 0 {
			budget.MaxTokens = req.Budget.MaxTokens
		}
		if req.Budget.MaxItems > 0 {
			budget.MaxItems = req.Budget.MaxItems
		}
	}
	wantMemory, wantKnowledge := len(req.Kinds) == 0, len(req.Kinds) == 0
	for _, k := range req.Kinds {
		if k == cmi.ItemMemory {
			wantMemory = true
		}
		if k == cmi.ItemKnowledge {
			wantKnowledge = true
		}
	}
	policy, err := r.Store.GetPolicy(ctx, tenant)
	if err != nil {
		return nil, nil, err
	}
	q := postgres.Query{TSQuery: TSQuery(req.Query), Entities: cmi.EntityKeys(req.Entities), IncludeFlagged: policy.IncludeFlagged, Limit: 200, AsOf: req.AsOf}
	if r.Embedder != nil {
		if vecs, err := r.Embedder.Embed(ctx, []string{req.Query}); err == nil && len(vecs) == 1 {
			q.Vector, q.Model = vecs[0], r.Embedder.Model()
		} else if r.Log != nil {
			r.Log.Warn("query embedding failed; lexical retrieval only", "error", err)
		}
	}
	var cands []candidate
	if q.TSQuery != "" || len(q.Vector) > 0 {
		if wantMemory {
			// D22: tenant, the caller, the caller's units, and what the request names (unit scopes it names
			// must be the caller's)
			scopes := []cmi.Scope{{Kind: cmi.ScopeTenant}, {Kind: cmi.ScopeSubject, ID: v.Subject}}
			units, err := r.Store.UnitsOf(ctx, tenant, v)
			if err != nil {
				return nil, nil, err
			}
			member := map[cmi.Scope]bool{}
			for _, u := range units {
				member[u] = true
				scopes = append(scopes, u)
			}
			requested := append([]cmi.Scope{}, req.Scopes...)
			if req.Scope != nil {
				requested = append(requested, *req.Scope)
			}
			for _, sc := range requested {
				if sc.Kind == cmi.ScopeTenant || member[sc] {
					continue
				}
				if sc.IsUnit() {
					return nil, nil, &postgres.Error{Code: "FORBIDDEN", Message: "not a member of " + sc.Kind + ":" + sc.ID}
				}
				scopes = append(scopes, sc)
			}
			hits, err := r.Store.SearchMemory(ctx, tenant, q, scopes, v)
			if err != nil {
				return nil, nil, err
			}
			for _, h := range hits {
				m := h.Record
				if req.MaxAgeDays > 0 && now.Sub(m.Provenance.ObservedAt) > time.Duration(req.MaxAgeDays)*24*time.Hour {
					continue
				}
				if !m.Validity.ValidUntil.IsZero() && !m.Validity.ValidUntil.After(at) {
					continue // expired by the resolver's clock, even before the sweeper marks it
				}
				scope := m.Scope
				item := cmi.Item{Kind: cmi.ItemMemory, ID: m.ID, Source: m.Provenance.Source, Trust: h.Trust, Ref: m.Provenance.Ref, Excerpt: excerpt(m.Content, 1200),
					ObservedAt: m.Provenance.ObservedAt, ValidUntil: m.Validity.ValidUntil, Sensitivity: m.Sensitivity, Entities: m.Entities, Scope: &scope, Match: h.Match}
				if policy.IncludeFlagged {
					item.Flags = m.Flags
				}
				cands = append(cands, candidate{item: item, trust: trustRank(h.Trust), fresh: freshness(at, m.Provenance.ObservedAt), lexical: h.Lexical, semantic: h.Semantic,
					dedupe: "m:" + m.Provenance.Source + "/" + m.Provenance.Ref, created: m.CreatedAt})
			}
		}
		if wantKnowledge {
			hits, err := r.Store.SearchKnowledge(ctx, tenant, q, req.Sources, v)
			if err != nil {
				return nil, nil, err
			}
			for _, h := range hits {
				d := h.Document
				if req.MaxAgeDays > 0 && now.Sub(d.CreatedAt) > time.Duration(req.MaxAgeDays)*24*time.Hour {
					continue
				}
				if !d.ValidUntil.IsZero() && !d.ValidUntil.After(at) {
					continue
				}
				item := cmi.Item{Kind: cmi.ItemKnowledge, ID: d.ID, Source: d.Source, Trust: h.Trust, Ref: d.URI + "#" + itoa(h.Seq), Title: d.Title, Excerpt: excerpt(h.Content, 1500),
					ObservedAt: d.CreatedAt, ValidUntil: d.ValidUntil, Sensitivity: d.Sensitivity, Entities: d.Entities, Match: h.Match}
				if policy.IncludeFlagged {
					item.Flags = h.Flags
				}
				cands = append(cands, candidate{item: item, trust: trustRank(h.Trust), fresh: freshness(at, d.CreatedAt), lexical: h.Lexical, semantic: h.Semantic,
					dedupe: "k:" + d.Source + "/" + d.URI + "#" + itoa(h.Seq), created: d.CreatedAt})
			}
		}
	}
	// semantic-only candidates below the floor are noise
	kept := cands[:0]
	for _, c := range cands {
		if c.lexical == 0 && c.semantic < SemanticFloor {
			continue
		}
		kept = append(kept, c)
	}
	cands = kept
	// relevance: lexical ranks normalized to the best lexical hit, semantic cosine as is, the larger
	// wins; then trust, then freshness-weighted relevance; ties by recency
	var maxLex float64
	for _, c := range cands {
		if c.lexical > maxLex {
			maxLex = c.lexical
		}
	}
	for i := range cands {
		rel := cands[i].semantic
		if maxLex > 0 && cands[i].lexical/maxLex > rel {
			rel = cands[i].lexical / maxLex
		}
		if rel < 0 {
			rel = 0
		}
		cands[i].item.Score = round4(rel * (0.5 + 0.5*cands[i].fresh))
		cands[i].item.Tokens = cmi.Tokens(cands[i].item.Excerpt)
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].trust != cands[b].trust {
			return cands[a].trust < cands[b].trust
		}
		if cands[a].item.Score != cands[b].item.Score {
			return cands[a].item.Score > cands[b].item.Score
		}
		return cands[a].created.After(cands[b].created)
	})
	seen := map[string]bool{}
	pkg := &cmi.Package{ID: r.newID(), TenantID: tenant, RequestedBy: v.Subject, Purpose: req.Purpose, Query: req.Query, Scope: req.Scope, AsOf: req.AsOf, Items: []cmi.Item{}, Budget: budget, AssembledAt: now.UTC().Truncate(time.Second)}
	for _, c := range cands {
		if seen[c.dedupe] || len(pkg.Items) >= budget.MaxItems {
			continue
		}
		if pkg.Budget.UsedTokens+c.item.Tokens > budget.MaxTokens {
			continue
		}
		seen[c.dedupe] = true
		pkg.Budget.UsedTokens += c.item.Tokens
		pkg.Items = append(pkg.Items, c.item)
	}
	if lag, err := r.Store.ProjectionLag(ctx, tenant); err == nil && lag > 0 {
		pkg.ProjectionLag = round4(lag) // D26: the consumer knows the vector index may be behind
	}
	pkg.ExpiresAt = now.Add(time.Duration(policy.PackageTTLSeconds) * time.Second).UTC().Truncate(time.Second)
	env, err := cmi.Seal(pkg)
	if err != nil {
		return nil, nil, err
	}
	if err := r.Store.SavePackage(ctx, tenant, env, pkg); err != nil {
		return nil, nil, err
	}
	return pkg, env, nil
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Resolver) newID() string {
	if r.NewID != nil {
		return r.NewID()
	}
	return postgres.NewUUID()
}

func trustRank(t string) int {
	if t == cmi.TrustAuthoritative {
		return 0
	}
	return 1
}

// freshness decays from 1 (now) towards 0 with a 30-day half-life.
func freshness(now, at time.Time) float64 {
	days := now.Sub(at).Hours() / 24
	if days < 0 {
		days = 0
	}
	return 1 / (1 + days/30)
}

func excerpt(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

func round4(f float64) float64 { return float64(int64(f*10000+0.5)) / 10000 }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
