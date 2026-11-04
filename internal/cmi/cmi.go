// Package cmi holds the CMI's domain objects (ADR-0001 D1–D5): sources, memory records, knowledge
// documents and the sealed context package, with the rules every write and every package must
// satisfy. Nothing here touches a database or a network.
package cmi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"idp-cmi/internal/canonical"
)

// Sensitivity levels, lowest first. A principal sees content at or below its ceiling.
const (
	SensitivityPublic       = "public"
	SensitivityInternal     = "internal"
	SensitivityConfidential = "confidential"
	SensitivityRestricted   = "restricted"
)

var sensitivityRank = map[string]int{SensitivityPublic: 0, SensitivityInternal: 1, SensitivityConfidential: 2, SensitivityRestricted: 3}

// ValidSensitivity reports whether s is a level.
func ValidSensitivity(s string) bool { _, ok := sensitivityRank[s]; return ok }

// Within reports whether level is at or below ceiling.
func Within(level, ceiling string) bool { return sensitivityRank[level] <= sensitivityRank[ceiling] }

// Source kinds and trust.
const (
	SourceDomainSystem  = "domain-system"
	SourceDocumentStore = "document-store"
	SourceAgent         = "agent"
	SourceUser          = "user"
	TrustAuthoritative  = "authoritative"
	TrustInformational  = "informational"
)

// Memory scopes, kinds and statuses.
const (
	ScopeTenant       = "tenant"
	ScopeSubject      = "subject"
	ScopeThread       = "thread"
	ScopeAgent        = "agent"
	ScopeOrganization = "organization" // units of the tenant's directory (D22)
	ScopeTeam         = "team"
	ScopeProject      = "project"
	KindEpisodic      = "episodic"
	KindSemantic      = "semantic"

	StatusCandidate  = "candidate" // written but not yet admitted (D21); never packed
	StatusActive     = "active"
	StatusDisputed   = "disputed" // contradicted by an authoritative record (D21); never packed until resolved
	StatusExpired    = "expired"
	StatusRevoked    = "revoked"
	StatusSuperseded = "superseded"
)

var (
	nameRE  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
	idRE    = regexp.MustCompile(`^[A-Za-z0-9_.:/@-]{1,256}$`)
	tagRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
	refRE   = regexp.MustCompile(`^[\x21-\x7e]{1,512}$`)
	uriRE   = regexp.MustCompile(`^[\x21-\x7e]+$`) // length checked separately (RE2 caps repeat counts)
	roleRE  = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	digestR = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ValidName reports whether s may name a source.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Source is a registered origin of content (D2).
type Source struct {
	ID          string    `json:"sourceId"`
	Name        string    `json:"name"`
	DisplayName string    `json:"displayName"`
	Kind        string    `json:"kind"`
	Trust       string    `json:"trust"`
	BaseURL     string    `json:"baseUrl,omitempty"`
	CreatedBy   string    `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Validate checks a source as registered. Agent and user sources are always informational.
func (s *Source) Validate() error {
	if !ValidName(s.Name) {
		return errors.New("name must match ^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$")
	}
	if strings.TrimSpace(s.DisplayName) == "" || len(s.DisplayName) > 200 {
		return errors.New("displayName is required (at most 200 characters)")
	}
	switch s.Kind {
	case SourceDomainSystem, SourceDocumentStore:
	case SourceAgent, SourceUser:
		if s.Trust != TrustInformational {
			return errors.New("agent and user sources are informational")
		}
	default:
		return errors.New("kind must be domain-system, document-store, agent or user")
	}
	if s.Trust != TrustAuthoritative && s.Trust != TrustInformational {
		return errors.New("trust must be authoritative or informational")
	}
	if s.BaseURL != "" && (len(s.BaseURL) > 512 || !(strings.HasPrefix(s.BaseURL, "https://") || strings.HasPrefix(s.BaseURL, "http://"))) {
		return errors.New("baseUrl must be an http(s) URL")
	}
	return nil
}

// Provenance says where content came from (D1).
type Provenance struct {
	Source     string    `json:"source"`     // source name
	Ref        string    `json:"ref"`        // the source's own reference
	ObservedAt time.Time `json:"observedAt"` // when the source held this
	RecordedBy string    `json:"recordedBy"` // the principal or run that wrote it
}

// ACL limits who sees content inside the tenant: empty lists mean every subject / any role.
type ACL struct {
	Subjects []string `json:"subjects,omitempty"`
	Roles    []string `json:"roles,omitempty"`
}

// Validate checks an ACL.
func (a ACL) Validate() error {
	if len(a.Subjects) > 256 || len(a.Roles) > 64 {
		return errors.New("acl lists are too long")
	}
	for _, s := range a.Subjects {
		if !idRE.MatchString(s) {
			return fmt.Errorf("acl subject %q is invalid", s)
		}
	}
	for _, r := range a.Roles {
		if !roleRE.MatchString(r) {
			return fmt.Errorf("acl role %q is invalid", r)
		}
	}
	return nil
}

// Entity is an explicit reference to a business object (D13): a type name and the object's own id.
type Entity struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

var (
	entityTypeRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	flagRE       = regexp.MustCompile(`^injection:[a-z_]+$`)
)

// ValidateEntities checks an entity list: at most 32, each typed and identified, no duplicates.
func ValidateEntities(es []Entity) error {
	if len(es) > 32 {
		return errors.New("at most 32 entities")
	}
	seen := map[Entity]bool{}
	for _, e := range es {
		if !entityTypeRE.MatchString(e.Type) || !idRE.MatchString(e.ID) {
			return fmt.Errorf("entity %s:%s is invalid (type ^[a-z][a-z0-9-]{0,62}$, id up to 256 visible characters)", e.Type, e.ID)
		}
		if seen[e] {
			return fmt.Errorf("entity %s:%s is duplicated", e.Type, e.ID)
		}
		seen[e] = true
	}
	return nil
}

// SortEntities orders entities by type then id so equal lists are equal.
func SortEntities(es []Entity) []Entity {
	if es == nil {
		return []Entity{}
	}
	sort.Slice(es, func(i, j int) bool {
		if es[i].Type != es[j].Type {
			return es[i].Type < es[j].Type
		}
		return es[i].ID < es[j].ID
	})
	return es
}

// EntityKeys renders entities as "type:id" for the store's filters.
func EntityKeys(es []Entity) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.Type+":"+e.ID)
	}
	return out
}

// Scope binds a memory to a tenant, a subject, a thread or an agent.
type Scope struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
}

// Validate checks a scope.
func (s Scope) Validate() error {
	switch s.Kind {
	case ScopeTenant:
		if s.ID != "" {
			return errors.New("a tenant scope has no id")
		}
	case ScopeSubject, ScopeThread, ScopeAgent, ScopeOrganization, ScopeTeam, ScopeProject:
		if !idRE.MatchString(s.ID) {
			return errors.New("scope id is required")
		}
	default:
		return errors.New("scope.kind must be tenant, subject, thread, agent, organization, team or project")
	}
	return nil
}

// IsUnit reports whether the scope is one of the directory's unit kinds (D22).
func (s Scope) IsUnit() bool {
	return s.Kind == ScopeOrganization || s.Kind == ScopeTeam || s.Kind == ScopeProject
}

// Unit is an entry of the tenant's directory (D22): who is a member of an organization, team or
// project. Membership decides who may read and write the unit's memory.
type Unit struct {
	Kind        string    `json:"kind"`
	ID          string    `json:"id"`
	DisplayName string    `json:"displayName"`
	Parent      *Scope    `json:"parent,omitempty"`
	Members     []string  `json:"members"`
	Roles       []string  `json:"roles"`
	UpdatedBy   string    `json:"updatedBy,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt,omitzero"`
}

// Validate checks a unit as registered.
func (u *Unit) Validate() error {
	if !(Scope{Kind: u.Kind, ID: u.ID}).IsUnit() || !idRE.MatchString(u.ID) {
		return errors.New("kind must be organization, team or project, with an id")
	}
	if strings.TrimSpace(u.DisplayName) == "" || len(u.DisplayName) > 200 {
		return errors.New("displayName is required (at most 200 characters)")
	}
	if u.Parent != nil && (!u.Parent.IsUnit() || u.Parent.Validate() != nil || (u.Parent.Kind == u.Kind && u.Parent.ID == u.ID)) {
		return errors.New("parent must be another unit")
	}
	if len(u.Members) > 10000 || len(u.Roles) > 64 {
		return errors.New("unit lists are too long")
	}
	for _, m := range u.Members {
		if !idRE.MatchString(m) {
			return fmt.Errorf("member %q is invalid", m)
		}
	}
	for _, r := range u.Roles {
		if !roleRE.MatchString(r) {
			return fmt.Errorf("role %q is invalid", r)
		}
	}
	return nil
}

// Validity bounds when content may be used.
type Validity struct {
	ValidFrom  time.Time `json:"validFrom"`
	ValidUntil time.Time `json:"validUntil,omitzero"` // zero = until revoked
}

// MemoryRecord is one remembered thing (D3).
type MemoryRecord struct {
	ID          string          `json:"recordId"`
	Scope       Scope           `json:"scope"`
	Kind        string          `json:"kind"`
	Content     string          `json:"content"`
	Structured  json.RawMessage `json:"structured,omitempty"`
	Tags        []string        `json:"tags"`
	Provenance  Provenance      `json:"provenance"`
	Validity    Validity        `json:"validity"`
	Status      string          `json:"status"`
	Revocation  *Revocation     `json:"revocation,omitempty"`
	Sensitivity string          `json:"sensitivity"`
	ACL         ACL             `json:"acl"`
	Entities    []Entity        `json:"entities,omitempty"`   // business objects the record refers to (D13)
	Flags       []string        `json:"flags,omitempty"`      // write-time scan findings (D12); set by the CMI, never by the writer
	Version     int             `json:"version,omitempty"`    // bumped by every state change; If-Match names the expected one (D19)
	DisputedBy  string          `json:"disputedBy,omitempty"` // the record that contradicts this one (D21)
	SharedFrom  string          `json:"sharedFrom,omitempty"` // the record this one was explicitly shared from (D22)
	CreatedAt   time.Time       `json:"createdAt"`
}

// Revocation records who withdrew content and why.
type Revocation struct {
	At     time.Time `json:"at"`
	By     string    `json:"by"`
	Reason string    `json:"reason,omitempty"`
}

// MaxContent bounds a memory's text; MaxStructured its structured part.
const (
	MaxContent    = 64 << 10
	MaxStructured = 64 << 10
)

// Validate checks a record as written.
func (m *MemoryRecord) Validate() error {
	if err := m.Scope.Validate(); err != nil {
		return err
	}
	if m.Kind != KindEpisodic && m.Kind != KindSemantic {
		return errors.New("kind must be episodic or semantic")
	}
	if strings.TrimSpace(m.Content) == "" || len(m.Content) > MaxContent {
		return fmt.Errorf("content is required (at most %d bytes)", MaxContent)
	}
	if len(m.Structured) > MaxStructured || (len(m.Structured) > 0 && !json.Valid(m.Structured)) {
		return errors.New("structured must be JSON (at most 64 KiB)")
	}
	if len(m.Tags) > 32 {
		return errors.New("at most 32 tags")
	}
	seen := map[string]bool{}
	for _, t := range m.Tags {
		if !tagRE.MatchString(t) || seen[t] {
			return fmt.Errorf("tag %q is invalid or duplicated", t)
		}
		seen[t] = true
	}
	if !ValidName(m.Provenance.Source) || !refRE.MatchString(m.Provenance.Ref) || m.Provenance.ObservedAt.IsZero() || !idRE.MatchString(m.Provenance.RecordedBy) {
		return errors.New("provenance needs source, ref, observedAt and recordedBy")
	}
	if !m.Validity.ValidUntil.IsZero() && !m.Validity.ValidUntil.After(m.Validity.ValidFrom) {
		return errors.New("validity.validUntil must be after validFrom")
	}
	if !ValidSensitivity(m.Sensitivity) {
		return errors.New("sensitivity must be public, internal, confidential or restricted")
	}
	switch m.Status {
	case "", StatusCandidate, StatusActive, StatusDisputed, StatusExpired, StatusRevoked:
	default:
		return errors.New("status must be candidate, active, disputed, expired or revoked")
	}
	if err := ValidateEntities(m.Entities); err != nil {
		return err
	}
	for _, f := range m.Flags {
		if !flagRE.MatchString(f) {
			return fmt.Errorf("flag %q is not a scan finding", f)
		}
	}
	if m.Version < 0 {
		return errors.New("version must be positive")
	}
	return m.ACL.Validate()
}

// Normalize sorts tags and defaults the lists so equal records are equal.
func (m *MemoryRecord) Normalize() {
	if m.Tags == nil {
		m.Tags = []string{}
	}
	sort.Strings(m.Tags)
	if m.ACL.Subjects == nil {
		m.ACL.Subjects = []string{}
	}
	if m.ACL.Roles == nil {
		m.ACL.Roles = []string{}
	}
	if m.Status == "" {
		m.Status = StatusActive
	}
	if len(m.Entities) > 0 {
		m.Entities = SortEntities(m.Entities)
	} else {
		m.Entities = nil
	}
	if len(m.Flags) == 0 {
		m.Flags = nil
	}
}

// Document is a knowledge document (D4): its text is indexed in chunks, not returned whole.
type Document struct {
	ID            string      `json:"documentId"`
	Source        string      `json:"source"`
	URI           string      `json:"uri"`
	Title         string      `json:"title"`
	Version       int         `json:"version"`
	ContentSHA256 string      `json:"contentSha256"`
	ByteSize      int         `json:"byteSize"`
	ChunkCount    int         `json:"chunkCount"`
	Status        string      `json:"status"`
	ValidUntil    time.Time   `json:"validUntil,omitzero"`
	Sensitivity   string      `json:"sensitivity"`
	ACL           ACL         `json:"acl"`
	Entities      []Entity    `json:"entities,omitempty"` // business objects the document refers to (D13)
	Flags         []string    `json:"flags,omitempty"`    // union of its chunks' write-time scan findings (D12)
	Revision      int         `json:"revision,omitempty"` // bumped by every state change of this document row (D19); version is the URI's
	Revocation    *Revocation `json:"revocation,omitempty"`
	CreatedBy     string      `json:"createdBy"`
	CreatedAt     time.Time   `json:"createdAt"`
}

// MaxDocument bounds a document's text.
const MaxDocument = 4 << 20

// ValidateDocument checks a document as submitted (text separately).
func ValidateDocument(d *Document, text string) error {
	if !ValidName(d.Source) {
		return errors.New("source is required")
	}
	if len(d.URI) > 2048 || !uriRE.MatchString(d.URI) {
		return errors.New("uri is required (visible ASCII, at most 2048 characters)")
	}
	if strings.TrimSpace(d.Title) == "" || len(d.Title) > 500 {
		return errors.New("title is required (at most 500 characters)")
	}
	if strings.TrimSpace(text) == "" || len(text) > MaxDocument {
		return fmt.Errorf("text is required (at most %d bytes)", MaxDocument)
	}
	if !ValidSensitivity(d.Sensitivity) {
		return errors.New("sensitivity must be public, internal, confidential or restricted")
	}
	if err := ValidateEntities(d.Entities); err != nil {
		return err
	}
	return d.ACL.Validate()
}

// Chunk splits text into index units of about chunkSize bytes on paragraph, then line, boundaries.
func Chunk(text string, chunkSize int) []string {
	if chunkSize <= 0 {
		chunkSize = 1500
	}
	var out []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for _, para := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		if cur.Len()+len(para)+2 > chunkSize && cur.Len() > 0 {
			flush()
		}
		if len(para) > chunkSize { // a long paragraph: split on lines, then hard
			for _, line := range strings.Split(para, "\n") {
				for len(line) > chunkSize {
					cut := chunkSize
					for cut > 0 && !utf8Boundary(line, cut) {
						cut--
					}
					cur.WriteString(line[:cut])
					flush()
					line = line[cut:]
				}
				if cur.Len()+len(line)+1 > chunkSize && cur.Len() > 0 {
					flush()
				}
				cur.WriteString(line)
				cur.WriteString("\n")
			}
			continue
		}
		cur.WriteString(para)
		cur.WriteString("\n\n")
	}
	flush()
	return out
}

func utf8Boundary(s string, i int) bool { return i >= len(s) || (s[i]&0xC0) != 0x80 }

// Bigrams returns the CJK character bigrams of text as space-separated tokens, so a 'simple'
// full-text index can match Chinese, Japanese and Korean queries (D4).
func Bigrams(text string) string {
	var sb strings.Builder
	var run []rune
	emit := func() {
		for i := 0; i+1 < len(run); i++ {
			sb.WriteString(string(run[i : i+2]))
			sb.WriteByte(' ')
		}
		if len(run) == 1 {
			sb.WriteString(string(run))
			sb.WriteByte(' ')
		}
		run = run[:0]
	}
	for _, r := range text {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			run = append(run, r)
			continue
		}
		emit()
	}
	emit()
	return strings.TrimSpace(sb.String())
}

// AsOf is a bitemporal view (D25): valid = the business time the content must be valid at,
// recorded = the registration time by which the CMI must have known it (and not yet revoked it).
// A zero field means "now" for that axis.
type AsOf struct {
	Valid    time.Time `json:"valid,omitzero"`
	Recorded time.Time `json:"recorded,omitzero"`
}

// Validate checks an as-of view: at least one axis, neither in the future.
func (a *AsOf) Validate(now time.Time) error {
	if a.Valid.IsZero() && a.Recorded.IsZero() {
		return errors.New("asOf needs valid and/or recorded")
	}
	if a.Valid.After(now.Add(time.Minute)) || a.Recorded.After(now.Add(time.Minute)) {
		return errors.New("asOf cannot be in the future")
	}
	return nil
}

// Normalize fills a missing valid axis with now; a missing recorded axis stays zero and means the
// database's own now (the registration clock is the database's). Precision is kept: the recorded
// axis is compared with registration instants.
func (a *AsOf) Normalize(now time.Time) {
	if a.Valid.IsZero() {
		a.Valid = now
	}
	a.Valid = a.Valid.UTC()
	if !a.Recorded.IsZero() {
		a.Recorded = a.Recorded.UTC()
	}
}

// RecordedArg is the recorded axis as a query argument: nil = the database's now.
func (a *AsOf) RecordedArg() any {
	if a.Recorded.IsZero() {
		return nil
	}
	return a.Recorded
}

// Item kinds of a package.
const (
	ItemMemory    = "memory"
	ItemKnowledge = "knowledge"
)

// Item is one piece of context in a package (D5).
type Item struct {
	Kind        string    `json:"kind"`
	ID          string    `json:"id"` // record id or document id
	Source      string    `json:"source"`
	Trust       string    `json:"trust"`
	Ref         string    `json:"ref"`
	Title       string    `json:"title,omitempty"`
	Excerpt     string    `json:"excerpt"`
	ObservedAt  time.Time `json:"observedAt"`
	ValidUntil  time.Time `json:"validUntil,omitzero"`
	Sensitivity string    `json:"sensitivity"`
	Score       float64   `json:"score"`
	Tokens      int       `json:"tokens"`
	Entities    []Entity  `json:"entities,omitempty"` // the item's entity references (D13)
	Scope       *Scope    `json:"scope,omitempty"`    // the memory's scope (D22); knowledge has none
	Match       string    `json:"match,omitempty"`    // which retriever found it: lexical, semantic or both (D11)
	Flags       []string  `json:"flags,omitempty"`    // present only when the tenant policy lets flagged content through (D12)
}

// Match values of an item.
const (
	MatchLexical  = "lexical"
	MatchSemantic = "semantic"
	MatchBoth     = "both"
)

// Budget is what the package may contain.
type Budget struct {
	MaxTokens  int `json:"maxTokens"`
	UsedTokens int `json:"usedTokens"`
	MaxItems   int `json:"maxItems"`
}

// Package is the sealed, content-addressed result of a resolve (D5).
type Package struct {
	SchemaVersion string    `json:"schemaVersion"`
	ID            string    `json:"packageId"`
	TenantID      string    `json:"tenantId"`
	RequestedBy   string    `json:"requestedBy"`
	Purpose       string    `json:"purpose"`
	Query         string    `json:"query"`
	Scope         *Scope    `json:"scope,omitempty"`
	AsOf          *AsOf     `json:"asOf,omitempty"` // the bitemporal view the package was assembled for (D25)
	Items         []Item    `json:"items"`
	Budget        Budget    `json:"budget"`
	AssembledAt   time.Time `json:"assembledAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

// SchemaVersion of the package document.
const SchemaVersion = "idp.cmi.package/v1"

// Verification is what a consumer learns about a package at delivery time (D24): whether any item
// it cites has since been revoked, expired, disputed, superseded or erased, and each item's current
// status and version. It never extends the package's validity.
type Verification struct {
	PackageID  string       `json:"packageId"`
	Stale      bool         `json:"stale"`
	StaleItems []string     `json:"staleItems"`
	Redacted   bool         `json:"redacted"`
	Items      []ItemStatus `json:"items"`
	ExpiresAt  time.Time    `json:"expiresAt"`
	VerifiedAt time.Time    `json:"verifiedAt"`
}

// ItemStatus is one cited item's current state.
type ItemStatus struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Status  string `json:"status"` // active | candidate | disputed | superseded | expired | revoked | missing
	Version int    `json:"version,omitempty"`
	Usable  bool   `json:"usable"` // active and within validity
}

// Envelope is a package with its digest.
type Envelope struct {
	Digest   string          `json:"digest"`
	Document json.RawMessage `json:"document"`
}

// Seal normalizes the package and wraps it with its digest.
func Seal(p *Package) (*Envelope, error) {
	p.SchemaVersion = SchemaVersion
	if p.Items == nil {
		p.Items = []Item{}
	}
	raw, err := canonical.Canonical(p)
	if err != nil {
		return nil, err
	}
	digest, err := canonical.Digest(json.RawMessage(raw))
	if err != nil {
		return nil, err
	}
	return &Envelope{Digest: digest, Document: raw}, nil
}

// Open verifies an envelope and decodes the package.
func Open(e *Envelope) (*Package, error) {
	digest, err := canonical.Digest(e.Document)
	if err != nil {
		return nil, err
	}
	if digest != e.Digest || !digestR.MatchString(e.Digest) {
		return nil, errors.New("package digest does not match its document")
	}
	var p Package
	dec := json.NewDecoder(strings.NewReader(string(e.Document)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	if p.SchemaVersion != SchemaVersion {
		return nil, errors.New("unknown package schema version")
	}
	return &p, nil
}

// Tokens estimates the token count of text (about four bytes per token).
func Tokens(s string) int { return (len(s) + 3) / 4 }

// ContentSHA256 digests document text.
func ContentSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}
