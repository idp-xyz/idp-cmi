package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"idp-cmi/internal/cmi"
)

// DocQuery names the document a read or a find is about (D72). Via says which of the two it is, for the audit.
type DocQuery struct {
	DocumentID string
	AsOf       *cmi.AsOf // the view of D25/D52; nil = now
	Via        string    // read | find
}

// DocChunk is one chunk of the document with where it sits.
type DocChunk struct {
	Seq     int
	Content string
	Flags   []string
	Locator *cmi.Locator
}

// DocView is the version of a document the caller may read, with all its chunks in document order.
type DocView struct {
	Document cmi.Document
	Trust    string
	Chunks   []DocChunk
}

// visibleDocSQL is the predicate of a document the caller may read now or as of a view, the one resolve uses for knowledge
// (D7, D52, D53): the caller's ceiling and ACL, the excluded sources, the bases of the caller's scope, the version that is
// current. A version that is still waiting for its text is not current in either form (knowledgeCurrentSQL takes only active and
// superseded; knowledgeAsOfSQL leaves out pending ones, as the newer version that would replace another). Placeholders: $1 subject, $2
// roles, $3 ceiling rank, $4 excluded sources; a view adds $6 (valid) and $7 (recorded).
func visibleDocSQL(asOf bool) string {
	if asOf {
		return kbScopeSQL + excludeSQL("$4") + knowledgeAsOfSQL("$6", "$7")
	}
	return aclSQL + kbScopeSQL + excludeSQL("$4") + knowledgeCurrentSQL
}

const docChunkFrom = ` FROM knowledge_chunks c JOIN knowledge_documents d ON d.id = c.document_id JOIN sources s ON s.id = d.source_id, LATERAL (SELECT d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles) x`

// DocumentChunks finds the version of a document that is current for the caller (at q.AsOf, D52) and returns its chunks.
// The document id names a document by one of its versions: the version read is the one current for that base and uri,
// which is the id's own when no newer one is in force. A version that was erased or withdrawn names nothing. Every statement carries the visibility predicate of resolve, so a
// document the caller cannot see - another tenant's, another member's, above the ceiling, outside the ACL, erased, revoked,
// expired, not yet in force, still pending - is the same NOT_FOUND as one that does not exist. Reading a document of
// another member's personal base is audited (D53).
func (s *Store) DocumentChunks(ctx context.Context, tenant string, v Visibility, q DocQuery) (*DocView, error) {
	if !IsUUID(q.DocumentID) {
		return nil, fail("INVALID", "documentId must be a document id")
	}
	var out *DocView
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		out = nil
		// the personal base of another member is visible to a caller who may name it (the writing service, an
		// administrator): the document id names it, as a subject scope does in a resolve
		var owner string
		err := tx.QueryRow(ctx, `SELECT kb.scope_id FROM knowledge_documents d JOIN knowledge_bases kb ON kb.id = d.knowledge_base_id WHERE d.id = $1 AND kb.scope_kind = 'subject'`, q.DocumentID).Scan(&owner)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && owner != v.Subject && v.MayNameMembers {
			v.NamedSubjects = append(append([]string{}, v.NamedSubjects...), owner)
		}
		if err := bindView(ctx, tx, v); err != nil {
			return err
		}
		pred := visibleDocSQL(q.AsOf != nil)
		args := []any{v.Subject, rolesOrEmpty(v.Roles), rank(v.Ceiling), excluded(v), q.DocumentID}
		if q.AsOf != nil {
			args = append(args, q.AsOf.Valid, q.AsOf.RecordedArg())
		}
		var view DocView
		var x docScan
		row := tx.QueryRow(ctx, `SELECT `+docCols+`, s.trust FROM knowledge_documents d JOIN sources s ON s.id = d.source_id, LATERAL (SELECT d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles) x
			WHERE (d.knowledge_base_id, d.uri) = (SELECT n.knowledge_base_id, n.uri FROM knowledge_documents n WHERE n.id = $5 AND n.erasure_id IS NULL AND n.status <> 'revoked')`+pred+` ORDER BY d.version DESC LIMIT 1`, args...)
		if err := row.Scan(append(x.dest(&view.Document), &view.Trust)...); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fail("NOT_FOUND", "document not found")
			}
			return err
		}
		x.finish(&view.Document)
		// the chunks: the predicate is applied again, so a document withdrawn since the statement above leaves nothing
		args[4] = view.Document.ID
		rows, err := tx.Query(ctx, `SELECT c.seq, c.content, c.flags, c.heading_path, c.page, c.table_name, c.slide, c.sheet, c.cell_range`+docChunkFrom+`
			WHERE d.id = $5`+pred+` ORDER BY c.seq`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c DocChunk
			var path, table, sheet, cells string
			var page, slide *int
			if err := rows.Scan(&c.Seq, &c.Content, &c.Flags, &path, &page, &table, &slide, &sheet, &cells); err != nil {
				return err
			}
			if len(c.Flags) == 0 {
				c.Flags = nil
			}
			if path != "" || page != nil || table != "" || slide != nil || sheet != "" {
				c.Locator = &cmi.Locator{Table: table, Sheet: sheet, Range: cells}
				if slide != nil {
					c.Locator.Slide = *slide
				}
				if path != "" {
					c.Locator.Headings = cmi.SplitHeadingPath(path)
				}
				if page != nil {
					c.Locator.Page = *page
				}
			}
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
		out = &view
		return nil
	})
	return out, err
}
