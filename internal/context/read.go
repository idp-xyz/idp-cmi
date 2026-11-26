package context

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/cmi"
)

// Read and find (ADR-0001 D71-D75, contract 1.7.0): the two ways to look into one document by its id. Both go through the
// visibility of resolve (postgres.DocumentChunks) and produce a package sealed and stored like resolve's, so verification
// and erasure treat it like any other.
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
}

var rangeRE = regexp.MustCompile(`^([A-Z]{1,3})([0-9]+):([A-Z]{1,3})([0-9]+)$`)

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
	asOf     *cmi.AsOf
	budget   cmi.Budget
	policy   *postgres.Policy
	view     *postgres.DocView
	omitted  cmi.Omitted
	purposed string
}

func (r *Resolver) prepare(ctx context.Context, tenant string, v postgres.Visibility, id, purpose, ceiling, via string, asOf *cmi.AsOf, b *cmi.Budget) (*prepared, error) {
	now := r.now()
	if asOf != nil {
		if err := asOf.Validate(now); err != nil {
			return nil, err
		}
		asOf.Normalize(now)
	}
	if ceiling != "" && cmi.Within(ceiling, v.Ceiling) {
		v.Ceiling = ceiling
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
	view, err := r.Store.DocumentChunks(ctx, tenant, v, postgres.DocQuery{DocumentID: id, AsOf: asOf, Via: via})
	if err != nil {
		return nil, err
	}
	if purpose == "" {
		purpose = via
	}
	return &prepared{v: v, asOf: asOf, budget: budget, policy: policy, view: view, purposed: purpose}, nil
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
	if err := r.Store.SavePackage(ctx, tenant, env, pkg); err != nil {
		return nil, nil, err
	}
	return pkg, env, nil
}

// Read gives the chunks of a document at a place (D71): the chunks the locator matches in document order - or, with no
// locator, from the start of the document - and around chunks on each side of the matches, as far as the budget goes.
func (r *Resolver) Read(ctx context.Context, tenant string, v postgres.Visibility, req ReadRequest) (*cmi.Package, *cmi.Envelope, error) {
	if err := req.validate(); err != nil {
		return nil, nil, err
	}
	p, err := r.prepare(ctx, tenant, v, req.DocumentID, req.Purpose, req.SensitivityCeiling, "read", req.AsOf, req.Budget)
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
	for n, i := range order {
		body := excerpt(chunks[i].Content, readExcerptBytes)
		if body == "" {
			continue
		}
		it := p.item(chunks[i], body)
		it.Score = 1
		if len(given) >= p.budget.MaxItems || tokens+it.Tokens > p.budget.MaxTokens {
			stopped = n
			break
		}
		tokens += it.Tokens
		given[i] = it
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
		sortInts(near)
		for _, j := range near {
			body := excerpt(chunks[j].Content, readExcerptBytes)
			if body == "" {
				continue
			}
			it := p.item(chunks[j], body)
			if len(given) >= p.budget.MaxItems || tokens+it.Tokens > p.budget.MaxTokens {
				continue // context that does not fit is not a loss to report: the matches are what was asked
			}
			tokens += it.Tokens
			given[j] = it
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

func readQuery(req ReadRequest) string {
	q := "read " + req.DocumentID
	if req.Locator != nil {
		if raw, err := json.Marshal(req.Locator); err == nil && string(raw) != "{}" {
			q += " " + string(raw)
		}
	}
	return q
}

// Find gives the chunks of a document that hold a text, word for word (D72), in document order, with where the text is in
// each: at most MaxHits places over all, and as many chunks as the budget takes.
func (r *Resolver) Find(ctx context.Context, tenant string, v postgres.Visibility, req FindRequest) (*cmi.Package, *cmi.Envelope, error) {
	if err := req.validate(); err != nil {
		return nil, nil, err
	}
	p, err := r.prepare(ctx, tenant, v, req.DocumentID, req.Purpose, req.SensitivityCeiling, "find", req.AsOf, req.Budget)
	if err != nil {
		return nil, nil, err
	}
	maxHits := req.MaxHits
	if maxHits == 0 {
		maxHits = DefaultHits
	}
	needle := foldText(req.Text)
	var items []cmi.Item
	tokens, left := 0, maxHits
	for _, c := range p.view.Chunks {
		hits := findAll(c.Content, needle)
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
		if len(hits) > left {
			hits = hits[:left]
		}
		body := window(c.Content, hits[0], findWindowBytes)
		if body == "" {
			continue
		}
		it := p.item(c, body)
		it.Match, it.Score, it.Hits = cmi.MatchExact, 1, hits
		if len(items) >= p.budget.MaxItems || tokens+it.Tokens > p.budget.MaxTokens {
			p.omitted.Budget++
			continue
		}
		tokens += it.Tokens
		left -= len(hits)
		items = append(items, it)
	}
	return r.seal(ctx, tenant, p, req.Text, items, tokens)
}

// ---- locator matching ----

// matchesLocator says a chunk at place c is covered by the requested locator: every field the request gives must hold (D71).
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

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

// ---- find: matching ----

// folded is a text made comparable: a run of white space is one space, A-Z are a-z (ASCII only: other scripts are matched
// as they are, so a Chinese text is matched character by character), and for each character where it began and ended
// in the original, counted in characters (code points).
type folded struct {
	runes      []rune
	start, end []int
}

func foldText(s string) folded {
	f := foldFull(s)
	// the needle is the text without the white space at its ends
	for len(f.runes) > 0 && f.runes[0] == ' ' {
		f.runes, f.start, f.end = f.runes[1:], f.start[1:], f.end[1:]
	}
	for len(f.runes) > 0 && f.runes[len(f.runes)-1] == ' ' {
		n := len(f.runes) - 1
		f.runes, f.start, f.end = f.runes[:n], f.start[:n], f.end[:n]
	}
	return f
}

// findAll returns where the needle is in text, without overlaps, in order. Offsets and lengths are in characters of text.
func findAll(text string, needle folded) []cmi.TextHit {
	n := len(needle.runes)
	if n == 0 {
		return nil
	}
	hay := foldFull(text)
	var out []cmi.TextHit
	for i := 0; i+n <= len(hay.runes); {
		j := 0
		for j < n && hay.runes[i+j] == needle.runes[j] {
			j++
		}
		if j < n {
			i++
			continue
		}
		start, end := hay.start[i], hay.end[i+n-1]
		out = append(out, cmi.TextHit{Offset: start, Length: end - start})
		i += n
	}
	return out
}

// foldFull is foldText without trimming (the text searched keeps its ends).
func foldFull(s string) folded {
	var f folded
	src := []rune(s)
	for i := 0; i < len(src); {
		r := src[i]
		if unicode.IsSpace(r) {
			j := i
			for j < len(src) && unicode.IsSpace(src[j]) {
				j++
			}
			f.runes, f.start, f.end = append(f.runes, ' '), append(f.start, i), append(f.end, j)
			i = j
			continue
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		f.runes, f.start, f.end = append(f.runes, r), append(f.start, i), append(f.end, i+1)
		i++
	}
	return f
}

// window is at most max bytes of text around the first hit, cut between characters: it starts from the hit (the beginning of
// it when the hit alone is longer) and grows to both sides, and a cut is marked with an ellipsis. A text that fits is given
// whole.
func window(text string, hit cmi.TextHit, max int) string {
	if len(text) <= max {
		return strings.TrimSpace(text)
	}
	src := []rune(text)
	a := min(max0(hit.Offset), len(src))
	b := min(a+hit.Length, len(src))
	bytes, end := 0, a
	for end < b && bytes+utf8.RuneLen(src[end]) <= max {
		bytes += utf8.RuneLen(src[end])
		end++
	}
	b = end
	for grew := true; grew; {
		grew = false
		if a > 0 && bytes+utf8.RuneLen(src[a-1]) <= max {
			a--
			bytes += utf8.RuneLen(src[a])
			grew = true
		}
		if b < len(src) && bytes+utf8.RuneLen(src[b]) <= max {
			bytes += utf8.RuneLen(src[b])
			b++
			grew = true
		}
	}
	out := strings.TrimSpace(string(src[a:b]))
	if a > 0 {
		out = "…" + out
	}
	if b < len(src) {
		out += "…"
	}
	return out
}

func max0(n int) int { return max(n, 0) }
