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
}

// DocChunk is one chunk of the document with where it sits. Content is empty when only the places were asked for.
type DocChunk struct {
	Seq     int
	Content string
	Flags   []string
	Locator *cmi.Locator
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
// ceiling rank, $4 excluded sources; a view adds $6 (valid) and $7 (recorded).
func visibleDocSQL(asOf bool) string {
	if asOf {
		return kbScopeSQL + excludeSQL("$4") + knowledgeAsOfSQL("$6", "$7")
	}
	return aclSQL + kbScopeSQL + excludeSQL("$4") + knowledgeCurrentSQL
}

const docChunkFrom = ` FROM knowledge_chunks c JOIN knowledge_documents d ON d.id = c.document_id JOIN sources s ON s.id = d.source_id, LATERAL (SELECT d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles) x`

func docArgs(v Visibility, q DocQuery, id string) []any {
	args := []any{v.Subject, rolesOrEmpty(v.Roles), rank(v.Ceiling), excluded(v), id}
	if q.AsOf != nil {
		args = append(args, q.AsOf.Valid, q.AsOf.RecordedArg())
	}
	return args
}

// bindDocView makes the view of the caller for the document the id names: the personal base of another member is visible to a
// caller who may name it (the writing service, an administrator): the document id names it, as a subject scope does in a
// resolve (D53).
func bindDocView(ctx context.Context, tx pgx.Tx, v Visibility, id string) (Visibility, error) {
	var owner string
	err := tx.QueryRow(ctx, `SELECT kb.scope_id FROM knowledge_documents d JOIN knowledge_bases kb ON kb.id = d.knowledge_base_id WHERE d.id = $1 AND kb.scope_kind = 'subject'`, id).Scan(&owner)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return v, err
	}
	if err == nil && owner != v.Subject && v.MayNameMembers {
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
		bv, err := bindDocView(ctx, tx, v, q.DocumentID)
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
// audited (D53).
func (s *Store) DocumentChunks(ctx context.Context, tenant string, v Visibility, q DocQuery, content bool) (*DocView, error) {
	if !IsUUID(q.DocumentID) {
		return nil, fail("INVALID", "documentId must be a document id")
	}
	var out *DocView
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		out = nil
		v, err := bindDocView(ctx, tx, v, q.DocumentID)
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
		rows, err := tx.Query(ctx, `SELECT c.seq, c.flags, c.heading_path, c.page, c.table_name, c.slide, c.sheet, c.cell_range`+cols+docChunkFrom+`
			WHERE d.id = $5`+visibleDocSQL(q.AsOf != nil)+` ORDER BY c.seq`, docArgs(v, q, view.Document.ID)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c DocChunk
			var path, table, sheet, cells string
			var page, slide *int
			dest := []any{&c.Seq, &c.Flags, &path, &page, &table, &slide, &sheet, &cells}
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
		if err := auditPersonalRead(ctx, tx, tenant, v, view.Document.KnowledgeBase, view.Document.ID, q.Via); err != nil {
			return err
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

// ChunkContents returns the text of some chunks of the version q.DocumentID names (the id DocumentChunks resolved, not a
// sibling): the visibility predicate is applied again. A chunk that is no longer readable is missing from the result.
func (s *Store) ChunkContents(ctx context.Context, tenant string, v Visibility, q DocQuery, seqs []int) (map[int]string, error) {
	out := map[int]string{}
	if len(seqs) == 0 {
		return out, nil
	}
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		v, err := bindDocView(ctx, tx, v, q.DocumentID)
		if err != nil {
			return err
		}
		args := append(docArgs(v, q, q.DocumentID), seqs)
		rows, err := tx.Query(ctx, `SELECT c.seq, c.content`+docChunkFrom+` WHERE d.id = $5 AND c.seq = ANY($`+itoaSQL(len(args))+`::int[])`+visibleDocSQL(q.AsOf != nil), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var seq int
			var content string
			if err := rows.Scan(&seq, &content); err != nil {
				return err
			}
			out[seq] = content
		}
		return rows.Err()
	})
	return out, err
}

func itoaSQL(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}
