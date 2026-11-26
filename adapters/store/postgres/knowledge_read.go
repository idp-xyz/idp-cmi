package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
)

// DocQuery names the document a read or a find is about (D71-D73). Via says which of the two it is, for the audit.
type DocQuery struct {
	DocumentID string
	AsOf       *cmi.AsOf // the view of D25/D52; nil = now
	Via        string    // read | find
	// RequireID, when set, is the version the caller has already been cleared to read (the PDP was asked about it, D76): the
	// version found must be this one, or the document is not found.
	RequireID string
	// NamedOwner is the member whose personal base the request names (D78): a document in another member's personal base is
	// served only when it is that member's and the caller may name members. Sources narrows the documents to those of the named
	// sources, like a resolve's (D78); empty = no filter.
	NamedOwner string
	Sources    []string
}

// DocChunk is one chunk of the document with where it sits. Content is empty when only the places were asked for.
type DocChunk struct {
	Seq     int
	Content string
	Flags   []string
	Locator *cmi.Locator
	// CutAfter: true when the chunk ends at a hard cut of a long line (the text goes on in the next chunk with nothing between),
	// false when it ends at a line, a paragraph or a cut next to white space (one space), nil when that is not known: a chunk
	// written before cut_after existed that looks like a cut (D77).
	CutAfter *bool
}

// DocView is the version of a document the caller may read, with its chunks in document order.
type DocView struct {
	Document cmi.Document
	Trust    string
	Chunks   []DocChunk
}

// visibleDocSQL is the predicate of a document the caller may read now or as of a view, the one resolve uses for knowledge
// (D7, D52, D53): the caller's ceiling and ACL, the excluded sources, the bases of the caller's scope, the version that is
// current. A version that is still waiting for its text is not current in either form (knowledgeCurrentSQL takes only active
// and superseded; knowledgeAsOfSQL takes versions that were in force by then, D76). Placeholders: $1 subject, $2 roles, $3
// ceiling rank, $4 excluded sources; $5 is the document id, $6 the sources of the request; a view adds $7 (valid) and $8 (recorded).
func visibleDocSQL(asOf bool) string {
	if asOf {
		return sourcesSQL + kbScopeSQL + excludeSQL("$4") + knowledgeAsOfSQL("$7", "$8")
	}
	return sourcesSQL + aclSQL + kbScopeSQL + excludeSQL("$4") + knowledgeCurrentSQL
}

// sourcesSQL is the request's sources filter ($6, a text[]; empty = none) (D78).
const sourcesSQL = ` AND (cardinality($6::text[]) = 0 OR s.name = ANY($6))`

const docChunkFrom = ` FROM knowledge_chunks c JOIN knowledge_documents d ON d.id = c.document_id JOIN sources s ON s.id = d.source_id, LATERAL (SELECT d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles) x`

func docArgs(v Visibility, q DocQuery, id string) []any {
	args := []any{v.Subject, rolesOrEmpty(v.Roles), rank(v.Ceiling), excluded(v), id, orEmpty(q.Sources)}
	if q.AsOf != nil {
		args = append(args, q.AsOf.Valid, q.AsOf.RecordedArg())
	}
	return args
}

// bindDocView makes the view of the caller for the document the id names: the personal base of another member is visible to a
// caller who may name it (the writing service, an administrator): the document id names it, as a subject scope does in a
// resolve (D53).
func bindDocView(ctx context.Context, tx pgx.Tx, v Visibility, id, named string) (Visibility, error) {
	if !IsUUID(id) {
		return v, fail("NOT_FOUND", "document not found")
	}
	var owner string
	err := tx.QueryRow(ctx, `SELECT kb.scope_id FROM knowledge_documents d JOIN knowledge_bases kb ON kb.id = d.knowledge_base_id WHERE d.id = $1 AND kb.scope_kind = 'subject'`, id).Scan(&owner)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return v, err
	}
	if err == nil && owner != v.Subject && v.MayNameMembers && named == owner { // only by naming the member (D53, D78)
		v.NamedSubjects = append(append([]string{}, v.NamedSubjects...), owner)
	}
	return v, bindView(ctx, tx, v)
}

// resolveDocument finds the version the caller may read: the one current at q.AsOf for the base and uri of the version the id
// names (D52). A version that was erased or withdrawn names nothing.
func resolveDocument(ctx context.Context, tx pgx.Tx, v Visibility, q DocQuery) (*DocView, error) {
	var view DocView
	var x docScan
	row := tx.QueryRow(ctx, `SELECT `+docCols+`, s.trust FROM knowledge_documents d JOIN sources s ON s.id = d.source_id, LATERAL (SELECT d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles) x
		WHERE (d.knowledge_base_id, d.uri) = (SELECT n.knowledge_base_id, n.uri FROM knowledge_documents n WHERE n.id = $5 AND n.erasure_id IS NULL AND n.status <> 'revoked')`+visibleDocSQL(q.AsOf != nil)+` ORDER BY d.version DESC LIMIT 1`, docArgs(v, q, q.DocumentID)...)
	if err := row.Scan(append(x.dest(&view.Document), &view.Trust)...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail("NOT_FOUND", "document not found")
		}
		return nil, err
	}
	x.finish(&view.Document)
	if q.RequireID != "" && view.Document.ID != q.RequireID {
		return nil, fail("NOT_FOUND", "document not found")
	}
	return &view, nil
}

// ResolveDocument says which version of a document the caller would read, without reading it and without any audit: the
// API asks the PDP about that version before the read (D76).
func (s *Store) ResolveDocument(ctx context.Context, tenant string, v Visibility, q DocQuery) (*DocView, error) {
	if !IsUUID(q.DocumentID) {
		return nil, fail("INVALID", "documentId must be a document id")
	}
	var out *DocView
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		bv, err := bindDocView(ctx, tx, v, q.DocumentID, q.NamedOwner)
		if err != nil {
			return err
		}
		out, err = resolveDocument(ctx, tx, bv, q)
		return err
	})
	return out, err
}

// DocumentChunks finds the version of a document that is current for the caller (at q.AsOf, D52) and returns its chunks. With
// content = false the chunks come without their text (their place and flags only), so that a read can choose which to fetch
// (ChunkContents). Every statement carries the visibility predicate of resolve, so a document the caller cannot see -
// another tenant's, another member's, above the ceiling, outside the ACL, erased, revoked, expired, not yet in force, still
// pending - is the same NOT_FOUND as one that does not exist. Reading a document of another member's personal base is
// audited (D53) by AuditDocumentRead, once, after the read has succeeded (D78): a read that is repeated, or fails, leaves no
// second record or none.
func (s *Store) DocumentChunks(ctx context.Context, tenant string, v Visibility, q DocQuery, content bool) (*DocView, error) {
	if !IsUUID(q.DocumentID) {
		return nil, fail("INVALID", "documentId must be a document id")
	}
	var out *DocView
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		out = nil
		v, err := bindDocView(ctx, tx, v, q.DocumentID, q.NamedOwner)
		if err != nil {
			return err
		}
		view, err := resolveDocument(ctx, tx, v, q)
		if err != nil {
			return err
		}
		// the chunks: the predicate is applied again, so a document withdrawn since the statement above leaves nothing
		cols := ``
		if content {
			cols = `, c.content`
		}
		rows, err := tx.Query(ctx, `SELECT c.seq, c.flags, c.heading_path, c.page, c.table_name, c.slide, c.sheet, c.cell_range, c.cut_after`+cols+docChunkFrom+`
			WHERE d.id = $5`+visibleDocSQL(q.AsOf != nil)+` ORDER BY c.seq`, docArgs(v, q, view.Document.ID)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c DocChunk
			var path, table, sheet, cells string
			var page, slide *int
			dest := []any{&c.Seq, &c.Flags, &path, &page, &table, &slide, &sheet, &cells, &c.CutAfter}
			if content {
				dest = append(dest, &c.Content)
			}
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			if len(c.Flags) == 0 {
				c.Flags = nil
			}
			c.Locator = locatorOf(path, page, table, slide, sheet, cells)
			view.Chunks = append(view.Chunks, c)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		if len(view.Chunks) == 0 {
			return fail("NOT_FOUND", "document not found")
		}
		out = view
		return nil
	})
	return out, err
}

func locatorOf(path string, page *int, table string, slide *int, sheet, cells string) *cmi.Locator {
	if path == "" && page == nil && table == "" && slide == nil && sheet == "" {
		return nil
	}
	l := &cmi.Locator{Table: table, Sheet: sheet, Range: cells}
	if slide != nil {
		l.Slide = *slide
	}
	if path != "" {
		l.Headings = cmi.SplitHeadingPath(path)
	}
	if page != nil {
		l.Page = *page
	}
	return l
}

// ChunkContents returns the chunks the seqs name of the version q.DocumentID names (the id DocumentChunks resolved, not a
// sibling), with their text, flags and place as they are now, from one statement that carries the visibility predicate and the
// revision of the document the places were read at (D77): a conversion that replaced the chunks since (it deletes and inserts
// them and bumps the revision) leaves no row, so the text of one version is never paired with the places or the flags of
// another, and a new injection flag is seen. A chunk that is no longer readable is missing from the result; when the document
// is there but at another revision the answer is a CONFLICT: read it again.
func (s *Store) ChunkContents(ctx context.Context, tenant string, v Visibility, q DocQuery, revision int, seqs []int) (map[int]DocChunk, error) {
	out := map[int]DocChunk{}
	if len(seqs) == 0 {
		return out, nil
	}
	if s.OnChunkContents != nil {
		s.OnChunkContents()
	}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		v, err := bindDocView(ctx, tx, v, q.DocumentID, q.NamedOwner)
		if err != nil {
			return err
		}
		args := append(docArgs(v, q, q.DocumentID), seqs, revision)
		n := len(args)
		rows, err := tx.Query(ctx, `SELECT c.seq, c.content, c.flags, c.heading_path, c.page, c.table_name, c.slide, c.sheet, c.cell_range, c.cut_after`+docChunkFrom+
			` WHERE d.id = $5 AND c.seq = ANY($`+itoaSQL(n-1)+`::int[]) AND d.revision = $`+itoaSQL(n)+visibleDocSQL(q.AsOf != nil), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c DocChunk
			var path, table, sheet, cells string
			var page, slide *int
			if err := rows.Scan(&c.Seq, &c.Content, &c.Flags, &path, &page, &table, &slide, &sheet, &cells, &c.CutAfter); err != nil {
				return err
			}
			if len(c.Flags) == 0 {
				c.Flags = nil
			}
			c.Locator = locatorOf(path, page, table, slide, sheet, cells)
			out[c.Seq] = c
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		if len(out) < len(seqs) {
			var now int
			err := tx.QueryRow(ctx, `SELECT d.revision`+` FROM knowledge_documents d JOIN sources s ON s.id = d.source_id, LATERAL (SELECT d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles) x WHERE d.id = $5`+visibleDocSQL(q.AsOf != nil), docArgs(v, q, q.DocumentID)...).Scan(&now)
			if err == nil && now != revision {
				return fail("CONFLICT", "the document changed while it was being read; read it again")
			}
		}
		return nil
	})
	return out, err
}

func itoaSQL(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// AuditDocumentRead records that the caller read or found in a document of another member's personal base (D53): once, by the
// resolver, after the read succeeded; the caller named the member (D78).
func (s *Store) AuditDocumentRead(ctx context.Context, tenant string, v Visibility, baseName, docID, via string) error {
	return s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		return auditPersonalRead(ctx, tx, tenant, v, baseName, docID, via)
	})
}
