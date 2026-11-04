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
	ScopeTenant  = "tenant"
	ScopeSubject = "subject"
	ScopeThread  = "thread"
	ScopeAgent   = "agent"
	KindEpisodic = "episodic"
	KindSemantic = "semantic"

	StatusActive     = "active"
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
	case ScopeSubject, ScopeThread, ScopeAgent:
		if !idRE.MatchString(s.ID) {
			return errors.New("scope id is required")
		}
	default:
		return errors.New("scope.kind must be tenant, subject, thread or agent")
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
	if m.Status != "" && m.Status != StatusActive && m.Status != StatusExpired && m.Status != StatusRevoked {
		return errors.New("status must be active, expired or revoked")
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
}

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
	Items         []Item    `json:"items"`
	Budget        Budget    `json:"budget"`
	AssembledAt   time.Time `json:"assembledAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

// SchemaVersion of the package document.
const SchemaVersion = "idp.cmi.package/v1"

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
