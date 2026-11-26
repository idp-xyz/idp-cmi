package context

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/cmi"
)

// Read and find (ADR-0001 D72-D76, contract 1.7.0): the two ways to look into one document by its id. Both go through the
// visibility of resolve (postgres.DocumentChunks) and produce a package sealed and stored like resolve's, so verification
// and erasure treat it like any other.

// InvalidError is a request that is wrong as written (422); every other error of the resolver is the service's own (a
// database that failed, a timeout) and is not told to the caller.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(err error) error {
	if err == nil {
		return nil
	}
	return &InvalidError{Msg: err.Error()}
}

const (
	DefaultAround = 1
	MaxAround     = 5
	DefaultHits   = 20
	MaxHits       = 100
	MaxFindText   = 1000 // characters of the text a find looks for
	// readExcerptBytes is the longest body a read item carries: a chunk is about 1500 bytes (cmi.KnowledgeChunkBytes), so this
	// is the whole chunk; a find shows a window of findWindowBytes around the first hit.
	readExcerptBytes = 6000
	findWindowBytes  = 1500
	// readDefaultItems is the item budget of a read or a find that names none: the token budget is what limits a read.
	readDefaultItems = 100
)

// ReadRequest asks for places in one document.
type ReadRequest struct {
	DocumentID         string       `json:"documentId"`
	Locator            *cmi.Locator `json:"locator,omitempty"`
	Around             *int         `json:"around,omitempty"` // chunks before and after each hit; default 1, at most 5
	Budget             *cmi.Budget  `json:"budget,omitempty"`
	Purpose            string       `json:"purpose,omitempty"`
	AsOf               *cmi.AsOf    `json:"asOf,omitempty"`
	SensitivityCeiling string       `json:"sensitivityCeiling,omitempty"`
	// RequireVersion is set by the API once the PDP has been asked about this version (D76): nothing else is read.
	RequireVersion string `json:"-"`
}

// FindRequest asks where a text is in one document.
type FindRequest struct {
	DocumentID         string      `json:"documentId"`
	Text               string      `json:"text"`
	MaxHits            int         `json:"maxHits,omitempty"` // default 20, at most 100
	Budget             *cmi.Budget `json:"budget,omitempty"`
	Purpose            string      `json:"purpose,omitempty"`
	AsOf               *cmi.AsOf   `json:"asOf,omitempty"`
	SensitivityCeiling string      `json:"sensitivityCeiling,omitempty"`
	RequireVersion     string      `json:"-"`
}

var rangeRE = regexp.MustCompile(`^([A-Z]{1,3})([0-9]+):([A-Z]{1,3})([0-9]+)$`)

func (r *ReadRequest) Validate() error { return invalid(r.validate()) }

func (r *ReadRequest) validate() error {
	if !postgres.IsUUID(r.DocumentID) {
		return errors.New("documentId must be a document id")
	}
	if len(r.Purpose) > 200 {
		return errors.New("purpose is too long")
	}
	if r.Around != nil && (*r.Around < 0 || *r.Around > MaxAround) {
		return errors.New("around must be within [0, 5]")
	}
	if err := validateBudget(r.Budget); err != nil {
		return err
	}
	if r.SensitivityCeiling != "" && !cmi.ValidSensitivity(r.SensitivityCeiling) {
		return errors.New("sensitivityCeiling is not a level")
	}
	l := r.Locator
	if l == nil {
		return nil
	}
	if l.Page < 0 || l.Slide < 0 || l.Page > 1<<30 || l.Slide > 1<<30 {
		return errors.New("locator page and slide must be positive numbers")
	}
	if len(l.Headings) > 16 {
		return errors.New("locator.headings has at most 16 entries")
	}
	for _, h := range l.Headings {
		if strings.TrimSpace(h) == "" || len(h) > 500 {
			return errors.New("locator.headings entries must be heading texts (at most 500 bytes)")
		}
	}
	if len(l.Sheet) > 500 || len(l.Table) > 500 {
		return errors.New("locator.sheet and locator.table are too long")
	}
	if l.Range != "" && parseRange(l.Range) == nil {
		return errors.New("locator.range must be a cell range such as A12:F40")
	}
	return nil
}

func (r *FindRequest) Validate() error { return invalid(r.validate()) }

func (r *FindRequest) validate() error {
	if !postgres.IsUUID(r.DocumentID) {
		return errors.New("documentId must be a document id")
	}
	if len(r.Purpose) > 200 {
		return errors.New("purpose is too long")
	}
	if len(r.Text) > 4*MaxFindText || utf8.RuneCountInString(r.Text) > MaxFindText || !utf8.ValidString(r.Text) {
		return errors.New("text is at most 1000 characters")
	}
	if len(foldText(r.Text).runes) == 0 {
		return errors.New("text is required")
	}
	if r.MaxHits < 0 || r.MaxHits > MaxHits {
		return errors.New("maxHits must be within [0, 100]")
	}
	if err := validateBudget(r.Budget); err != nil {
		return err
	}
	if r.SensitivityCeiling != "" && !cmi.ValidSensitivity(r.SensitivityCeiling) {
		return errors.New("sensitivityCeiling is not a level")
	}
	return nil
}

func validateBudget(b *cmi.Budget) error {
	if b != nil && (b.MaxTokens < 0 || b.MaxTokens > 32000 || b.MaxItems < 0 || b.MaxItems > 100) {
		return errors.New("budget.maxTokens must be within [0, 32000] and maxItems within [0, 100]")
	}
	return nil
}

// prepared is what read and find share before they look at chunks: the view, the budget, the policy and the document.
type prepared struct {
	v        postgres.Visibility
	q        postgres.DocQuery
	asOf     *cmi.AsOf
	budget   cmi.Budget
	policy   *postgres.Policy
	view     *postgres.DocView
	omitted  cmi.Omitted
	purposed string
	asked    string // the id the caller named, which may be another version of the document than the one read (D77)
}

// viewOf is the caller's view for one request: the as-of view checked and normalized, the request's ceiling lowering the
// caller's and never raising it (D74).
func (r *Resolver) viewOf(v postgres.Visibility, ceiling string, asOf *cmi.AsOf) (postgres.Visibility, error) {
	if asOf != nil {
		now := r.now()
		if err := asOf.Validate(now); err != nil {
			return v, invalid(err)
		}
		asOf.Normalize(now)
	}
	if ceiling != "" && cmi.Within(ceiling, v.Ceiling) {
		v.Ceiling = ceiling
	}
	return v, nil
}

// Target is the version of a document a read or a find would look into for the caller, found under the visibility of
// resolve and without any audit (D76): the API asks the PDP about this version, then runs the request with it.
type Target struct {
	ID, Source, Sensitivity string
}

// TargetOf finds the Target; a document the caller cannot see is the NOT_FOUND of every other way of not seeing it.
func (r *Resolver) TargetOf(ctx context.Context, tenant string, v postgres.Visibility, id, ceiling string, asOf *cmi.AsOf) (*Target, error) {
	v, err := r.viewOf(v, ceiling, asOf)
	if err != nil {
		return nil, err
	}
	view, err := r.Store.ResolveDocument(ctx, tenant, v, postgres.DocQuery{DocumentID: id, AsOf: asOf})
	if err != nil {
		return nil, err
	}
	return &Target{ID: view.Document.ID, Source: view.Document.Source, Sensitivity: view.Document.Sensitivity}, nil
}

func (r *Resolver) prepare(ctx context.Context, tenant string, v postgres.Visibility, id, require, purpose, ceiling, via string, asOf *cmi.AsOf, b *cmi.Budget, content bool) (*prepared, error) {
	v, err := r.viewOf(v, ceiling, asOf)
	if err != nil {
		return nil, err
	}
	budget := cmi.Budget{MaxTokens: DefaultMaxTokens, MaxItems: readDefaultItems}
	if b != nil {
		if b.MaxTokens > 0 {
			budget.MaxTokens = b.MaxTokens
		}
		if b.MaxItems > 0 {
			budget.MaxItems = b.MaxItems
		}
	}
	policy, err := r.Store.GetPolicy(ctx, tenant)
	if err != nil {
		return nil, err
	}
	q := postgres.DocQuery{DocumentID: id, AsOf: asOf, Via: via, RequireID: require}
	view, err := r.Store.DocumentChunks(ctx, tenant, v, q, content)
	if err != nil {
		return nil, err
	}
	q.DocumentID, q.RequireID = view.Document.ID, "" // the chunks are asked for the version that was found
	if purpose == "" {
		purpose = via
	}
	return &prepared{v: v, q: q, asOf: asOf, budget: budget, policy: policy, view: view, purposed: purpose, asked: id}, nil
}

// contents fetches the chunks of the version that was found, with their text, flags and places as they are now, under the
// visibility predicate and the revision of the document again (D74, D77).
func (r *Resolver) contents(ctx context.Context, tenant string, p *prepared, idx []int) (map[int]postgres.DocChunk, error) {
	seqs := make([]int, len(idx))
	for i, j := range idx {
		seqs[i] = p.view.Chunks[j].Seq
	}
	got, err := r.Store.ChunkContents(ctx, tenant, p.v, p.q, p.view.Document.Revision, seqs)
	if err != nil {
		return nil, err
	}
	if len(got) != len(seqs) {
		return nil, &postgres.Error{Code: "NOT_FOUND", Message: "document not found"} // withdrawn while it was being read
	}
	return got, nil
}

// item makes the package item of a chunk: what resolve makes of one, with the version's own id.
func (p *prepared) item(c postgres.DocChunk, body string) cmi.Item {
	d := &p.view.Document
	it := cmi.Item{Kind: cmi.ItemKnowledge, ID: d.ID, Source: d.Source, Trust: p.view.Trust, Ref: d.URI + "#" + itoa(c.Seq), Title: d.Title, Excerpt: sectionLine(c.Locator) + body,
		ObservedAt: d.CreatedAt, ValidUntil: d.ValidUntil, Sensitivity: d.Sensitivity, Entities: d.Entities, Locator: c.Locator}
	if p.policy.IncludeFlagged {
		it.Flags = c.Flags
	}
	it.Tokens = cmi.Tokens(it.Excerpt)
	return it
}

// usable says a chunk may be given: a flagged one only when the tenant policy lets flagged content through (D12). A flagged
// chunk that was matched is counted in omitted.flagged.
func (p *prepared) usable(c postgres.DocChunk) bool {
	return len(c.Flags) == 0 || p.policy.IncludeFlagged
}

func (r *Resolver) seal(ctx context.Context, tenant string, p *prepared, query string, items []cmi.Item, tokens int) (*cmi.Package, *cmi.Envelope, error) {
	now := r.now()
	budget := p.budget
	budget.UsedTokens = tokens
	pkg := &cmi.Package{ID: r.newID(), TenantID: tenant, RequestedBy: p.v.Subject, Purpose: p.purposed, Query: query, AsOf: p.asOf, Items: items, Budget: budget, AssembledAt: now.UTC().Truncate(time.Second)}
	if pkg.Items == nil {
		pkg.Items = []cmi.Item{}
	}
	if p.omitted != (cmi.Omitted{}) {
		o := p.omitted
		pkg.Omitted = &o
	}
	pkg.ExpiresAt = now.Add(time.Duration(p.policy.PackageTTLSeconds) * time.Second).UTC().Truncate(time.Second)
	env, err := cmi.Seal(pkg)
	if err != nil {
		return nil, nil, err
	}
	// the package names the version it consulted, items or none (D76)
	consulted := []string{p.view.Document.ID}
	if p.asked != p.view.Document.ID { // erasing only the version the caller named finds the package too
		consulted = append(consulted, p.asked)
	}
	if err := r.Store.SaveConsultingPackage(ctx, tenant, env, pkg, consulted); err != nil {
		return nil, nil, err
	}
	return pkg, env, nil
}

// readBatch is how many chunks of a read are fetched at a time: the text of a chunk is fetched only when the budget may still
// take it (D72), so a read of the start of a long document does not load the document.
const readBatch = 16

// Read gives the chunks of a document at a place (D72): the chunks the locator matches in document order - or, with no
// locator, from the start of the document - and around chunks on each side of the matches, as far as the budget goes.
func (r *Resolver) Read(ctx context.Context, tenant string, v postgres.Visibility, req ReadRequest) (*cmi.Package, *cmi.Envelope, error) {
	pkg, env, err := r.read(ctx, tenant, v, req)
	if changedWhileRead(err) { // the document was converted again between the two statements: once more (D77)
		pkg, env, err = r.read(ctx, tenant, v, req)
	}
	return pkg, env, err
}

// changedWhileRead is the CONFLICT of a document that changed between the statements of one read or find.
func changedWhileRead(err error) bool {
	var pe *postgres.Error
	return errors.As(err, &pe) && pe.Code == "CONFLICT"
}

func (r *Resolver) read(ctx context.Context, tenant string, v postgres.Visibility, req ReadRequest) (*cmi.Package, *cmi.Envelope, error) {
	if err := req.Validate(); err != nil {
		return nil, nil, err
	}
	p, err := r.prepare(ctx, tenant, v, req.DocumentID, req.RequireVersion, req.Purpose, req.SensitivityCeiling, "read", req.AsOf, req.Budget, false)
	if err != nil {
		return nil, nil, err
	}
	around := DefaultAround
	if req.Around != nil {
		around = *req.Around
	}
	chunks := p.view.Chunks
	matched := make([]bool, len(chunks))
	var order []int // the matched chunks that may be given, in document order
	for i, c := range chunks {
		if req.Locator == nil || matchesLocator(req.Locator, c.Locator) {
			matched[i] = true
			if p.usable(c) {
				order = append(order, i)
			} else {
				p.omitted.Flagged++
			}
		}
	}
	given := map[int]cmi.Item{}
	tokens := 0
	stopped := -1 // the first matched chunk the budget did not take
	for start := 0; start < len(order) && stopped < 0; start += readBatch {
		end := min(start+readBatch, len(order))
		text, err := r.contents(ctx, tenant, p, order[start:end])
		if err != nil {
			return nil, nil, err
		}
		for n := start; n < end; n++ {
			i := order[n]
			fresh := text[chunks[i].Seq]
			if !p.usable(fresh) { // flagged since the places were read (D12)
				p.omitted.Flagged++
				continue
			}
			body := excerpt(fresh.Content, readExcerptBytes)
			if body == "" {
				continue
			}
			it := p.item(fresh, body)
			it.Score = 1
			if len(given) >= p.budget.MaxItems || tokens+it.Tokens > p.budget.MaxTokens {
				stopped = n
				break
			}
			tokens += it.Tokens
			given[i] = it
		}
	}
	if stopped >= 0 {
		p.omitted.Budget += len(order) - stopped // a read is contiguous: what comes after the first chunk that did not fit is left
	}
	if around > 0 && req.Locator != nil {
		var near []int
		seen := map[int]bool{}
		for _, i := range order {
			if _, ok := given[i]; !ok {
				continue
			}
			for j := i - around; j <= i+around; j++ {
				if j >= 0 && j < len(chunks) && !matched[j] && !seen[j] && p.usable(chunks[j]) {
					seen[j] = true
					near = append(near, j)
				}
			}
		}
		sort.Ints(near)
		for start := 0; start < len(near) && len(given) < p.budget.MaxItems && tokens < p.budget.MaxTokens; start += readBatch {
			end := min(start+readBatch, len(near))
			text, err := r.contents(ctx, tenant, p, near[start:end])
			if err != nil {
				return nil, nil, err
			}
			for _, j := range near[start:end] {
				fresh := text[chunks[j].Seq]
				body := excerpt(fresh.Content, readExcerptBytes)
				if body == "" || !p.usable(fresh) {
					continue
				}
				it := p.item(fresh, body)
				if len(given) >= p.budget.MaxItems || tokens+it.Tokens > p.budget.MaxTokens {
					continue // context that does not fit is not a loss to report: the matches are what was asked
				}
				tokens += it.Tokens
				given[j] = it
			}
		}
	}
	items := make([]cmi.Item, 0, len(given))
	for i := range chunks {
		if it, ok := given[i]; ok {
			items = append(items, it)
		}
	}
	return r.seal(ctx, tenant, p, readQuery(req), items, tokens)
}

// maxQueryBytes is the longest query a package has: the contract's 4096 (resolve checks bytes, so does this).
const maxQueryBytes = 4096

// readQuery is what a read's package says it asked: "read", the document id and the locator as JSON (not escaped for HTML:
// the section names are read by people), cut on a character boundary where it is longer than a package's query may be.
func readQuery(req ReadRequest) string {
	q := "read " + req.DocumentID
	if req.Locator != nil {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(req.Locator); err == nil {
			if raw := strings.TrimSpace(b.String()); raw != "{}" {
				q += " " + raw
			}
		}
	}
	return cutAtCharacter(q, maxQueryBytes)
}

// cutAtCharacter is s cut to at most n bytes without cutting a character.
func cutAtCharacter(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Find gives the chunks of a document that hold a text, word for word (D73), in document order, with where the text is in
// each excerpt: at most MaxHits places over all, and as many chunks as the budget takes.
func (r *Resolver) Find(ctx context.Context, tenant string, v postgres.Visibility, req FindRequest) (*cmi.Package, *cmi.Envelope, error) {
	if err := req.Validate(); err != nil {
		return nil, nil, err
	}
	p, err := r.prepare(ctx, tenant, v, req.DocumentID, req.RequireVersion, req.Purpose, req.SensitivityCeiling, "find", req.AsOf, req.Budget, true)
	if err != nil {
		return nil, nil, err
	}
	maxHits := req.MaxHits
	if maxHits == 0 {
		maxHits = DefaultHits
	}
	found := findInChunks(p.view.Chunks, foldText(req.Text))
	var items []cmi.Item
	tokens, left := 0, maxHits
	for i, c := range p.view.Chunks {
		hits := found[i]
		if len(hits) == 0 {
			continue
		}
		if !p.usable(c) {
			p.omitted.Flagged++
			continue
		}
		if left == 0 {
			p.omitted.Budget++ // the hits limit is a budget too
			continue
		}
		taken := hits
		if len(taken) > left {
			taken = taken[:left]
		}
		win := windowAround(c.Content, taken[0], findWindowBytes)
		if win.text == "" {
			continue
		}
		it := p.item(c, win.text)
		it.Match, it.Score = cmi.MatchExact, 1
		it.Hits, it.HitCount = win.place(taken, utf8.RuneCountInString(sectionLine(c.Locator))), len(hits)
		if len(items) >= p.budget.MaxItems || tokens+it.Tokens > p.budget.MaxTokens {
			p.omitted.Budget++
			continue
		}
		tokens += it.Tokens
		left -= len(taken)
		items = append(items, it)
	}
	return r.seal(ctx, tenant, p, req.Text, items, tokens)
}

// ---- locator matching ----

// matchesLocator says a chunk at place c is covered by the requested locator: every field the request gives must hold (D72).
// A page or a slide is exact; headings are a prefix of the chunk's heading path; a sheet is exact and a range matches when
// the cells it names overlap the chunk's.
func matchesLocator(want *cmi.Locator, c *cmi.Locator) bool {
	if c == nil {
		c = &cmi.Locator{}
	}
	if want.Page > 0 && c.Page != want.Page {
		return false
	}
	if want.Slide > 0 && c.Slide != want.Slide {
		return false
	}
	if want.Table != "" && c.Table != want.Table {
		return false
	}
	if len(want.Headings) > 0 {
		if len(c.Headings) < len(want.Headings) {
			return false
		}
		for i, h := range want.Headings {
			if strings.TrimSpace(c.Headings[i]) != strings.TrimSpace(h) {
				return false
			}
		}
	}
	if want.Sheet != "" && c.Sheet != want.Sheet {
		return false
	}
	if want.Range != "" {
		a, b := parseRange(want.Range), parseRange(c.Range)
		if a == nil || b == nil || !a.overlaps(b) {
			return false
		}
	}
	return true
}

// cellRange is the corners of a range such as A12:F40: columns and rows, both ends included.
type cellRange struct{ c1, r1, c2, r2 int }

func parseRange(s string) *cellRange {
	m := rangeRE.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	col := func(letters string) int {
		n := 0
		for _, ch := range letters {
			n = n*26 + int(ch-'A') + 1
		}
		return n
	}
	row := func(digits string) int {
		if len(digits) > 9 {
			return 1 << 30
		}
		n := 0
		for _, ch := range digits {
			n = n*10 + int(ch-'0')
		}
		return n
	}
	out := &cellRange{col(m[1]), row(m[2]), col(m[3]), row(m[4])}
	if out.c1 > out.c2 {
		out.c1, out.c2 = out.c2, out.c1
	}
	if out.r1 > out.r2 {
		out.r1, out.r2 = out.r2, out.r1
	}
	return out
}

func (a *cellRange) overlaps(b *cellRange) bool {
	return a.c1 <= b.c2 && b.c1 <= a.c2 && a.r1 <= b.r2 && b.r1 <= a.r2
}
