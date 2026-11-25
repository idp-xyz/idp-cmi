package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
)

const kbCols = `kb.id::text, kb.name, kb.display_name, kb.scope_kind, kb.scope_id, kb.manager_subjects, kb.manager_roles,
	kb.default_sensitivity, kb.sensitivity_ceiling, s.name, kb.quota_documents, kb.quota_bytes, kb.ocr, kb.status, kb.created_by, kb.created_at`

// KnowledgeSettings are the operator's settings for the knowledge bases (config file, section knowledge).
type KnowledgeSettings struct {
	// IndexContext puts the document title, the heading path and the table name in front of each chunk's body in the
	// text that is indexed and embedded (D56). Off by default: it was measured and not adopted.
	IndexContext bool
	// PersonalQuotaDocuments and PersonalQuotaBytes are the quota of a personal base that has none of its own (D55);
	// 0 = no limit. Bytes count the indexed text and the originals.
	PersonalQuotaDocuments int
	PersonalQuotaBytes     int64
}

// DefaultKnowledgeSettings are what Open starts with: no title or heading in the index, a member's personal base
// holds up to 500 documents and 200 MiB.
func DefaultKnowledgeSettings() KnowledgeSettings {
	return KnowledgeSettings{PersonalQuotaDocuments: 500, PersonalQuotaBytes: 200 << 20}
}

// kbNamedSetting is the transaction-local setting that carries the other members whose personal bases the
// request named (D53). bindView sets it; kbScopeSQL reads it. Unset, it names nobody.
const kbNamedSetting = `app.kb_named`

const kbNamedSep = "\x1f"

// kbScopeSQL hides documents whose base the caller is not meant to read by default (D53): the enterprise
// bases, the bases of the caller's units and the caller's own personal base - for everyone, administrators included -
// plus the personal bases of the other members the request names. An archived base leaves retrieval.
// Placeholders $1 (subject) and $2 (roles) are the ones visibleSQL uses.
const kbScopeSQL = `
	AND EXISTS (
		SELECT 1 FROM knowledge_bases kb
		LEFT JOIN units u ON u.tenant_id = kb.tenant_id AND u.kind = kb.scope_kind AND u.id = kb.scope_id
		WHERE kb.id = d.knowledge_base_id AND kb.status = 'active' AND (
			kb.scope_kind = 'tenant'
			OR (kb.scope_kind = 'subject' AND (kb.scope_id = $1 OR kb.scope_id = ANY(string_to_array(coalesce(current_setting('app.kb_named', true), ''), E'\x1f'))))
			OR (kb.scope_kind IN ('organization', 'team', 'project') AND ($1 = ANY(coalesce(u.members, '{}')) OR coalesce(u.roles, '{}') && $2::text[]))
		)
	)`

// kbListSQL is the list/get-by-name predicate on a base kb: administrators see every enterprise and unit base
// (archived ones too); a member sees the bases of kbScopeSQL. Another member's personal base is in neither list:
// it is read by naming it (GetKnowledgeBase), which is audited.
const kbListSQL = `
	AND ('cmi-admin' = ANY($2::text[]) OR kb.status = 'active')
	AND (
		kb.scope_kind = 'tenant'
		OR (kb.scope_kind = 'subject' AND kb.scope_id = $1)
		OR (kb.scope_kind IN ('organization', 'team', 'project') AND ('cmi-admin' = ANY($2::text[]) OR EXISTS (
			SELECT 1 FROM units u
			WHERE u.tenant_id = kb.tenant_id AND u.kind = kb.scope_kind AND u.id = kb.scope_id
			  AND ($1 = ANY(u.members) OR u.roles && $2::text[])
		)))
	)`

// kbNamedSQL is the get-by-name predicate: kbListSQL, and for an administrator also another member's personal base.
const kbNamedSQL = `
	AND ('cmi-admin' = ANY($2::text[]) OR kb.status = 'active')
	AND (
		kb.scope_kind = 'tenant'
		OR (kb.scope_kind = 'subject' AND (kb.scope_id = $1 OR 'cmi-admin' = ANY($2::text[])))
		OR (kb.scope_kind IN ('organization', 'team', 'project') AND ('cmi-admin' = ANY($2::text[]) OR EXISTS (
			SELECT 1 FROM units u
			WHERE u.tenant_id = kb.tenant_id AND u.kind = kb.scope_kind AND u.id = kb.scope_id
			  AND ($1 = ANY(u.members) OR u.roles && $2::text[])
		)))
	)`

// bindView tells the transaction which other members' personal bases the request named (D53). Every function
// that reads documents with kbScopeSQL calls it first; forgetting it only narrows the view.
func bindView(ctx context.Context, tx pgx.Tx, v Visibility) error {
	named := strings.Join(v.NamedSubjects, kbNamedSep)
	_, err := tx.Exec(ctx, `SELECT set_config('app.kb_named', $1, true)`, named)
	return err
}

func scanKnowledgeBase(row pgx.Row) (*cmi.KnowledgeBase, error) {
	var k cmi.KnowledgeBase
	if err := row.Scan(&k.ID, &k.Name, &k.DisplayName, &k.Scope.Kind, &k.Scope.ID, &k.Managers.Subjects, &k.Managers.Roles,
		&k.DefaultSensitivity, &k.SensitivityCeiling, &k.Source, &k.QuotaDocuments, &k.QuotaBytes, &k.OCR, &k.Status, &k.CreatedBy, &k.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "knowledge base not found")
		}
		return nil, err
	}
	if k.Managers.Subjects == nil {
		k.Managers.Subjects = []string{}
	}
	if k.Managers.Roles == nil {
		k.Managers.Roles = []string{}
	}
	return &k, nil
}

func rolesOrEmpty(roles []string) []string {
	if roles == nil {
		return []string{}
	}
	return roles
}

// CreateKnowledgeBase registers a base. A subject-scoped base must use a user source. The name prefix
// "personal-" is reserved for the bases the service creates for members (D53).
func (s *Store) CreateKnowledgeBase(ctx context.Context, tenant string, in cmi.KnowledgeBase, by string) (*cmi.KnowledgeBase, error) {
	if err := in.Validate(); err != nil {
		return nil, fail("INVALID", "%s", err)
	}
	if in.Name == cmi.PersonalAlias {
		return nil, fail("INVALID", "the name %s is reserved (kb://%s/ is the alias of a member's personal base)", in.Name, in.Name)
	}
	if strings.HasPrefix(in.Name, cmi.PersonalBasePrefix) {
		return nil, fail("INVALID", "the name prefix %s is reserved for members' personal bases", cmi.PersonalBasePrefix)
	}
	if in.Status != cmi.KnowledgeBaseActive {
		return nil, fail("INVALID", "a knowledge base is created active")
	}
	if in.Managers.Subjects == nil {
		in.Managers.Subjects = []string{}
	}
	if in.Managers.Roles == nil {
		in.Managers.Roles = []string{}
	}
	var out *cmi.KnowledgeBase
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		src, err := getSource(ctx, tx, in.Source)
		if err != nil {
			return fail("CONFLICT", "source %s is not registered", in.Source)
		}
		if in.Scope.Kind == cmi.ScopeSubject && src.Kind != cmi.SourceUser {
			return fail("INVALID", "a personal knowledge base uses a user source")
		}
		if in.Scope.IsUnit() {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM units WHERE kind = $1 AND id = $2`, in.Scope.Kind, in.Scope.ID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return fail("INVALID", "unit %s/%s is not in the directory", in.Scope.Kind, in.Scope.ID)
			}
		}
		id := NewUUID()
		if _, err := tx.Exec(ctx, `INSERT INTO knowledge_bases (
			id, tenant_id, name, display_name, scope_kind, scope_id, manager_subjects, manager_roles,
			default_sensitivity, sensitivity_ceiling, source_id, quota_documents, quota_bytes, ocr, status, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'active',$15)`,
			id, tenant, in.Name, in.DisplayName, in.Scope.Kind, in.Scope.ID, in.Managers.Subjects, in.Managers.Roles,
			in.DefaultSensitivity, in.SensitivityCeiling, src.ID, in.QuotaDocuments, in.QuotaBytes, in.OCR, by); err != nil {
			if isUniqueViolation(err) {
				if constraintOf(err) == "knowledge_bases_personal" {
					return fail("CONFLICT", "member %s already has a personal knowledge base", in.Scope.ID)
				}
				return fail("CONFLICT", "a knowledge base named %s exists", in.Name)
			}
			return err
		}
		if err := Audit(ctx, tx, tenant, by, "knowledge_base.create", id, map[string]any{"name": in.Name, "scope": in.Scope, "source": in.Source}); err != nil {
			return err
		}
		out, err = scanKnowledgeBase(tx.QueryRow(ctx, `SELECT `+kbCols+` FROM knowledge_bases kb JOIN sources s ON s.id = kb.source_id WHERE kb.id = $1`, id))
		return err
	})
	return out, err
}

// GetKnowledgeBase returns a base the caller can see. Anyone else gets not-found. An administrator naming another
// member's personal base reads it, and the read is audited (D53).
func (s *Store) GetKnowledgeBase(ctx context.Context, tenant, name string, v Visibility) (*cmi.KnowledgeBase, error) {
	var out *cmi.KnowledgeBase
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		k, err := scanKnowledgeBase(tx.QueryRow(ctx, `SELECT `+kbCols+` FROM knowledge_bases kb JOIN sources s ON s.id = kb.source_id
			WHERE kb.name = $3`+kbNamedSQL, v.Subject, rolesOrEmpty(v.Roles), name))
		if err != nil {
			return err
		}
		if k.Scope.Kind == cmi.ScopeSubject && k.Scope.ID != v.Subject {
			if err := Audit(ctx, tx, tenant, v.Subject, "knowledge_base.read", k.ID, map[string]any{"name": k.Name, "subject": k.Scope.ID}); err != nil {
				return err
			}
		}
		out = k
		return nil
	})
	return out, err
}

// ListKnowledgeBases returns the bases the caller can see, by name.
func (s *Store) ListKnowledgeBases(ctx context.Context, tenant string, v Visibility) ([]cmi.KnowledgeBase, error) {
	out := []cmi.KnowledgeBase{}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+kbCols+` FROM knowledge_bases kb JOIN sources s ON s.id = kb.source_id
			WHERE true`+kbListSQL+` ORDER BY kb.name`, v.Subject, rolesOrEmpty(v.Roles))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			k, err := scanKnowledgeBase(rows)
			if err != nil {
				return err
			}
			out = append(out, *k)
		}
		return rows.Err()
	})
	return out, err
}

// ArchiveKnowledgeBase stops retrieval and further uploads. Documents stay until retention.
func (s *Store) ArchiveKnowledgeBase(ctx context.Context, tenant, name, by string) (*cmi.KnowledgeBase, error) {
	return s.setBaseStatus(ctx, tenant, name, by, cmi.KnowledgeBaseActive, cmi.KnowledgeBaseArchived, "knowledge_base.archive", Visibility{Roles: []string{"cmi-admin"}})
}

// UnarchiveKnowledgeBase reopens an archived base. An administrator may reopen any; a member only their own
// personal base, so that an archived one does not lock its owner out (D53).
func (s *Store) UnarchiveKnowledgeBase(ctx context.Context, tenant, name string, v Visibility) (*cmi.KnowledgeBase, error) {
	return s.setBaseStatus(ctx, tenant, name, v.Subject, cmi.KnowledgeBaseArchived, cmi.KnowledgeBaseActive, "knowledge_base.unarchive", v)
}

func (s *Store) setBaseStatus(ctx context.Context, tenant, name, by, from, to, action string, v Visibility) (*cmi.KnowledgeBase, error) {
	var out *cmi.KnowledgeBase
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		b, err := loadBase(ctx, tx, name, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail("NOT_FOUND", "knowledge base not found")
		}
		if err != nil {
			return err
		}
		if !isAdmin(v.Roles) && !(b.ScopeKind == cmi.ScopeSubject && b.ScopeID == v.Subject) {
			return fail("NOT_FOUND", "knowledge base not found")
		}
		if b.Status != from {
			return fail("CONFLICT", "knowledge base %s is %s", name, b.Status)
		}
		if _, err := tx.Exec(ctx, `UPDATE knowledge_bases SET status = $2 WHERE id = $1`, b.ID, to); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, by, action, b.ID, map[string]any{"name": name}); err != nil {
			return err
		}
		out, err = scanKnowledgeBase(tx.QueryRow(ctx, `SELECT `+kbCols+` FROM knowledge_bases kb JOIN sources s ON s.id = kb.source_id WHERE kb.id = $1`, b.ID))
		return err
	})
	return out, err
}

// baseRow is a base as the upload paths use it.
type baseRow struct {
	ID, Name                    string
	ScopeKind, ScopeID          string
	ManagerSubjects             []string
	ManagerRoles                []string
	DefaultSensitivity, Ceiling string
	SourceID                    string
	QuotaDocuments              int
	QuotaBytes                  int64
	Status                      string
}

const baseRowCols = `id::text, name, scope_kind, scope_id, manager_subjects, manager_roles, default_sensitivity, sensitivity_ceiling, source_id::text, quota_documents, quota_bytes, status`

func scanBaseRow(row pgx.Row) (*baseRow, error) {
	var b baseRow
	err := row.Scan(&b.ID, &b.Name, &b.ScopeKind, &b.ScopeID, &b.ManagerSubjects, &b.ManagerRoles, &b.DefaultSensitivity, &b.Ceiling, &b.SourceID, &b.QuotaDocuments, &b.QuotaBytes, &b.Status)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// loadBase reads a base by name; lock takes the row lock that serializes uploads, quota checks and
// version numbers of that base (D51, D55).
func loadBase(ctx context.Context, tx pgx.Tx, name string, lock bool) (*baseRow, error) {
	q := `SELECT ` + baseRowCols + ` FROM knowledge_bases WHERE name = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	return scanBaseRow(tx.QueryRow(ctx, q, name))
}

func loadPersonalBase(ctx context.Context, tx pgx.Tx, subject string, lock bool) (*baseRow, error) {
	q := `SELECT ` + baseRowCols + ` FROM knowledge_bases WHERE scope_kind = 'subject' AND scope_id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	return scanBaseRow(tx.QueryRow(ctx, q, subject))
}

// ensureSourceBase finds, or creates on first use, the enterprise base named after the source. Creation is
// INSERT ... ON CONFLICT DO NOTHING followed by a SELECT, so concurrent first uploads all proceed (S4).
func ensureSourceBase(ctx context.Context, tx pgx.Tx, tenant string, src *cmi.Source, by string) (*baseRow, error) {
	if strings.HasPrefix(src.Name, cmi.PersonalBasePrefix) || src.Name == cmi.PersonalAlias {
		return nil, fail("INVALID", "a source named %s... cannot have a default knowledge base: name the base", cmi.PersonalBasePrefix)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO knowledge_bases (
		id, tenant_id, name, display_name, scope_kind, scope_id,
		default_sensitivity, sensitivity_ceiling, source_id, created_by)
		VALUES ($1,$2,$3,$4,'tenant','','internal','restricted',$5,$6) ON CONFLICT DO NOTHING`,
		NewUUID(), tenant, src.Name, src.DisplayName, src.ID, by); err != nil {
		return nil, err
	}
	return loadBase(ctx, tx, src.Name, true)
}

// ensurePersonalBase finds the member's personal base by its owner, creating it on first use (S4, D53). The
// name is derived from the subject; it is never what the base is looked up by.
func ensurePersonalBase(ctx context.Context, tx pgx.Tx, tenant string, src *cmi.Source, subject string) (*baseRow, error) {
	if src.Kind != cmi.SourceUser {
		return nil, fail("INVALID", "a personal knowledge base uses a user source")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO knowledge_bases (
		id, tenant_id, name, display_name, scope_kind, scope_id, manager_subjects,
		default_sensitivity, sensitivity_ceiling, source_id, created_by)
		VALUES ($1,$2,$3,'Personal knowledge base','subject',$4,$5,'internal','restricted',$6,$4) ON CONFLICT DO NOTHING`,
		NewUUID(), tenant, cmi.PersonalBaseName(subject), subject, []string{subject}, src.ID); err != nil {
		return nil, err
	}
	b, err := loadPersonalBase(ctx, tx, subject, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail("CONFLICT", "the personal knowledge base of %s could not be created", subject)
	}
	return b, err
}

func isAdmin(roles []string) bool {
	return contains(roles, "cmi-admin")
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		if contains(b, x) {
			return true
		}
	}
	return false
}

// canWrite rejects a caller who may not add documents to (upload) or withdraw documents from the base.
// A base the caller cannot read is not found. A personal base is written, and its documents' originals are
// attached, by its owner alone; an administrator may withdraw a document of it (revoke) but not add to it.
func (b *baseRow) canWrite(ctx context.Context, tx pgx.Tx, v Visibility, upload bool) error {
	if b.ScopeKind == cmi.ScopeSubject {
		if b.ScopeID == v.Subject {
			return b.activeOrConflict()
		}
		if isAdmin(v.Roles) && !upload {
			return nil
		}
		return fail("NOT_FOUND", "knowledge base %s is not registered", b.Name)
	}
	if isAdmin(v.Roles) {
		return b.activeOrConflict()
	}
	if contains(b.ManagerSubjects, v.Subject) || overlaps(b.ManagerRoles, v.Roles) {
		return b.activeOrConflict()
	}
	readable := b.ScopeKind == cmi.ScopeTenant
	if !readable {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM units WHERE kind = $1 AND id = $2 AND ($3 = ANY(members) OR roles && $4::text[])`,
			b.ScopeKind, b.ScopeID, v.Subject, rolesOrEmpty(v.Roles)).Scan(&n); err != nil {
			return err
		}
		readable = n > 0
	}
	if !readable {
		return fail("NOT_FOUND", "knowledge base %s is not registered", b.Name)
	}
	return fail("FORBIDDEN", "not a manager of knowledge base %s", b.Name)
}

func (b *baseRow) activeOrConflict() error {
	if b.Status != cmi.KnowledgeBaseActive {
		return fail("CONFLICT", "knowledge base %s is archived", b.Name)
	}
	return nil
}

// effectiveQuota is the quota that applies to the base: its own, or for a personal base without one the
// member default from the settings (D55).
func (s *Store) effectiveQuota(b *baseRow) (docs int, bytes int64) {
	docs, bytes = b.QuotaDocuments, b.QuotaBytes
	if b.ScopeKind == cmi.ScopeSubject {
		if docs == 0 {
			docs = s.Knowledge.PersonalQuotaDocuments
		}
		if bytes == 0 {
			bytes = s.Knowledge.PersonalQuotaBytes
		}
	}
	return docs, bytes
}

// baseUsage is what a base holds that counts against its quota (D55): the documents with a live version (a
// document counts once, however many versions it has) and, in bytes, the indexed text and the original of every
// live version that no newer version in force has replaced. Revoked, erased, expired and replaced versions
// do not count.
type baseUsage struct {
	Documents   int
	Bytes       int64
	URIBytes    int64 // of Bytes: the version of the uri asked about that is in force now
	HasURI      bool
	DocBytes    int64 // of Bytes: the document asked about (an original being attached or replaced)
	DocOriginal int64 // of DocBytes: its current original
	DocCounted  bool
}

func usageOf(ctx context.Context, tx pgx.Tx, baseID, uri, docID string) (baseUsage, error) {
	var u baseUsage
	err := tx.QueryRow(ctx, `
		SELECT count(DISTINCT uri)::int,
		       coalesce(sum(size), 0)::bigint,
		       coalesce(sum(size) FILTER (WHERE uri = $2 AND in_force), 0)::bigint,
		       coalesce(bool_or(uri = $2), false),
		       coalesce(sum(size) FILTER (WHERE id::text = $3), 0)::bigint,
		       coalesce(sum(original) FILTER (WHERE id::text = $3), 0)::bigint,
		       coalesce(bool_or(id::text = $3), false)
		FROM (
			SELECT d.id, d.uri, d.byte_size::bigint + coalesce(d.original_byte_size, 0) AS size, coalesce(d.original_byte_size, 0)::bigint AS original,
			       `+windowSQL("d", "now()")+` AS in_force
			FROM knowledge_documents d
			WHERE d.knowledge_base_id = $1 AND d.erasure_id IS NULL AND d.status IN ('active', 'superseded')
			  AND (d.valid_until IS NULL OR d.valid_until > now())`+notReplacedSQL("d", "now()")+`
		) x`, baseID, uri, docID).
		Scan(&u.Documents, &u.Bytes, &u.URIBytes, &u.HasURI, &u.DocBytes, &u.DocOriginal, &u.DocCounted)
	return u, err
}

// checkNewVersionQuota decides whether a version of textBytes of uri fits the base: a new version of a document the
// base already holds does not count the document twice, and a version that comes into force at once frees the
// bytes of the one it replaces.
func (s *Store) checkNewVersionQuota(ctx context.Context, tx pgx.Tx, b *baseRow, uri string, textBytes int, inForce bool) error {
	quotaDocs, quotaBytes := s.effectiveQuota(b)
	if quotaDocs == 0 && quotaBytes == 0 {
		return nil
	}
	u, err := usageOf(ctx, tx, b.ID, uri, "")
	if err != nil {
		return err
	}
	docs, bytes := u.Documents, u.Bytes+int64(textBytes)
	if !u.HasURI {
		docs++
	}
	if inForce {
		bytes -= u.URIBytes
	}
	if quotaDocs > 0 && docs > quotaDocs {
		return fail("QUOTA_EXCEEDED", "knowledge base %s already holds %d documents", b.Name, u.Documents)
	}
	if quotaBytes > 0 && bytes > quotaBytes {
		return fail("QUOTA_EXCEEDED", "knowledge base %s is over its byte quota", b.Name)
	}
	return nil
}

// checkOriginalQuota decides whether attaching an original of size bytes to the document fits the base, counting
// the original it replaces only once.
func (s *Store) checkOriginalQuota(ctx context.Context, tx pgx.Tx, b *baseRow, docID string, size int64) error {
	_, quotaBytes := s.effectiveQuota(b)
	if quotaBytes == 0 {
		return nil
	}
	u, err := usageOf(ctx, tx, b.ID, "", docID)
	if err != nil {
		return err
	}
	if !u.DocCounted { // a version that is replaced or revoked is not counted, and neither is its original
		return nil
	}
	if u.Bytes-u.DocOriginal+size > quotaBytes {
		return fail("QUOTA_EXCEEDED", "knowledge base %s is over its byte quota", b.Name)
	}
	return nil
}

// auditPersonalRead records that the caller read a document or an original in another member's personal
// base (D53), like D42's memory.read_other: who, whose, which, and how.
func auditPersonalRead(ctx context.Context, tx pgx.Tx, tenant string, v Visibility, baseName, docID, via string) error {
	var id, scopeID string
	err := tx.QueryRow(ctx, `SELECT id::text, scope_id FROM knowledge_bases WHERE name = $1 AND scope_kind = 'subject'`, baseName).Scan(&id, &scopeID)
	if errors.Is(err, pgx.ErrNoRows) || scopeID == v.Subject {
		return nil
	}
	if err != nil {
		return err
	}
	return Audit(ctx, tx, tenant, v.Subject, "knowledge.read_other", id, map[string]any{"via": via, "subject": scopeID, "document": docID, "client": v.Client, "actor": v.Actor})
}
