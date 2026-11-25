package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5"

	"idp-cmi/adapters/blob"
	"idp-cmi/internal/cmi"
)

// OriginalStore keeps the bytes of a document's original. The database holds only the key and the digest.
type OriginalStore interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string, max int64) ([]byte, error)
	Delete(ctx context.Context, key string) error
	// ListKeys lists every key under prefix: the erasure sweeps the whole prefix of a document, so an object whose delete
	// once failed, or whose upload was never recorded, does not outlive the document (D54).
	ListKeys(ctx context.Context, prefix string) ([]string, error)
}

// originalOpener is the optional streaming read of a store (the S3 client has it).
type originalOpener interface {
	Open(ctx context.Context, key string, max int64) (io.ReadCloser, error)
}

// documentPrefix is where every object of a document is: kb/<tenant>/<document id>/.
func documentPrefix(tenant, docID string) string { return fmt.Sprintf("kb/%s/%s/", tenant, docID) }

// OriginalType is an accepted media type of an original, the size it may have and the extension a download is named with.
type OriginalType struct {
	Max int64
	Ext string
}

// originalTypes is the upload allow-list (plan section 2.2): scans, decks and PDF may be 50 MiB, the other
// allowed types 25 MiB. The stored media type is checked against it again on download (D54).
var originalTypes = map[string]OriginalType{
	"application/pdf": {50 << 20, "pdf"},
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": {50 << 20, "pptx"},
	"image/png":  {50 << 20, "png"},
	"image/jpeg": {50 << 20, "jpg"},
	"image/tiff": {50 << 20, "tiff"},
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": {25 << 20, "docx"},
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       {25 << 20, "xlsx"},
	"text/plain":    {25 << 20, "txt"},
	"text/markdown": {25 << 20, "md"},
	"text/html":     {25 << 20, "html"},
}

// LargestOriginal is the largest size any accepted original type may have.
func LargestOriginal() int64 {
	var m int64
	for _, t := range originalTypes {
		m = max(m, t.Max)
	}
	return m
}

// OriginalLimit is the largest accepted original for a media type.
func OriginalLimit(media string) (int64, bool) {
	t, ok := originalTypes[media]
	return t.Max, ok
}

// OriginalFile is a stored original as a download.
type OriginalFile struct {
	Body      io.ReadCloser // verified against the recorded digest; an unlinked temporary file
	Size      int64
	MediaType string // from the allow-list; application/octet-stream when the stored one no longer is
	Name      string // a safe file name: original-<first eight of the document id>.<extension>
}

// originalObjectKey is the key of one upload: kb/<tenant>/<document id>/<version>/<upload id>. The id is the
// database's canonical spelling, the upload id is new for every upload (D54).
func originalObjectKey(tenant, docID string, version int, upload string) string {
	return fmt.Sprintf("kb/%s/%s/%d/%s", tenant, docID, version, upload)
}

func setOriginal(d *cmi.Document, media string, size int64, sum string) {
	if sum == "" {
		return
	}
	d.Original = &cmi.Original{MediaType: media, ByteSize: size, SHA256: sum}
}

// originalTarget is what a permission check finds before an original is read or written.
type originalTarget struct {
	tenant   string // the database's spelling of the tenant id (the token may spell it in upper case)
	id       string // the database's spelling
	version  int
	base     string
	key      string // the object the row points to now ("" for none)
	sum      string
	size     int64
	media    string
	baseName string
}

// findForOriginal finds the document the caller may read (D53, D7, D23) and, when write, may attach an original to. It
// runs before any body is read. A document the caller cannot read, or whose base the caller may not write, is not found.
func findForOriginal(ctx context.Context, tx pgx.Tx, tenant string, v Visibility, id string, write bool) (*originalTarget, error) {
	if !IsUUID(id) {
		return nil, fail("NOT_FOUND", "document not found")
	}
	var t originalTarget
	err := tx.QueryRow(ctx, `SELECT current_tenant()::text, d.id::text, d.version, kb.name, coalesce(d.original_key, ''), coalesce(d.original_sha256, ''), coalesce(d.original_byte_size, 0), coalesce(d.original_media_type, '')
		FROM knowledge_documents d JOIN sources s ON s.id = d.source_id JOIN knowledge_bases kb ON kb.id = d.knowledge_base_id, LATERAL (SELECT d.status, d.valid_until, d.sensitivity, d.acl_subjects, d.acl_roles) x
		WHERE d.id = $5`+memberDocSQL(v), v.Subject, rolesOrEmpty(v.Roles), rank(v.Ceiling), excluded(v), id).
		Scan(&t.tenant, &t.id, &t.version, &t.baseName, &t.key, &t.sum, &t.size, &t.media)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail("NOT_FOUND", "document not found")
	}
	if err != nil {
		return nil, err
	}
	if write {
		b, err := loadBase(ctx, tx, t.baseName, false)
		if err != nil {
			return nil, err
		}
		if err := b.canWrite(ctx, tx, v, true); err != nil {
			if se, ok := err.(*Error); ok && (se.Code == "FORBIDDEN" || se.Code == "NOT_FOUND") {
				return nil, fail("NOT_FOUND", "document not found")
			}
			return nil, err
		}
	} else if err := auditPersonalRead(ctx, tx, tenant, v, t.baseName, t.id, "original"); err != nil {
		return nil, err
	}
	return &t, nil
}

// unavailable maps the object store's "not reachable yet" to 503.
func unavailable(err error) error {
	if errors.Is(err, blob.ErrUnavailable) {
		return fail("NOT_CONFIGURED", "the original store is not reachable")
	}
	return err
}

// CheckOriginalWrite is the permission check of a PUT of an original, run before the body is read: the store is
// there, the media type is allowed, and the caller may attach an original to the document.
func (s *Store) CheckOriginalWrite(ctx context.Context, tenant, id string, v Visibility, media string) error {
	if s.Originals == nil {
		return fail("NOT_CONFIGURED", "no original store is configured")
	}
	if _, ok := originalTypes[media]; !ok {
		return fail("INVALID", "media type %s is not accepted", media)
	}
	return s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := bindView(ctx, tx, v); err != nil {
			return err
		}
		_, err := findForOriginal(ctx, tx, tenant, v, id, true)
		return err
	})
}

// PutOriginal stores the file for one document version and records its digest (D54). The permission is checked
// first; the body is then read once, hashed while it is read, and refused above the limit of its type. Every
// upload has its own object key; the row is updated only if it still points to the object this upload started
// from and the document is not erased; the object it replaced is deleted after the commit. A failure leaves the
// previous original in place.
func (s *Store) PutOriginal(ctx context.Context, tenant, id string, v Visibility, media string, body io.Reader) (*cmi.Document, error) {
	if s.Originals == nil {
		return nil, fail("NOT_CONFIGURED", "no original store is configured")
	}
	typ, ok := originalTypes[media]
	if !ok {
		return nil, fail("INVALID", "media type %s is not accepted", media)
	}
	var target *originalTarget
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		if err := bindView(ctx, tx, v); err != nil {
			return err
		}
		target, err = findForOriginal(ctx, tx, tenant, v, id, true)
		return err
	})
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	var buf bytes.Buffer
	n, err := io.Copy(io.MultiWriter(&buf, h), io.LimitReader(body, typ.Max+1))
	if err != nil {
		return nil, fail("INVALID", "the body could not be read: %v", err)
	}
	if n == 0 || n > typ.Max {
		return nil, fail("INVALID", "original must be 1 byte to %d bytes", typ.Max)
	}
	sum := "sha256:" + hex.EncodeToString(h.Sum(nil))
	key := originalObjectKey(target.tenant, target.id, target.version, NewUUID())
	if err := s.Originals.Put(ctx, key, buf.Bytes()); err != nil {
		return nil, unavailable(err)
	}
	var out *cmi.Document
	err = s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		var baseID, current string
		var erased bool
		err := tx.QueryRow(ctx, `SELECT d.knowledge_base_id::text, coalesce(d.original_key, ''), d.erasure_id IS NOT NULL FROM knowledge_documents d WHERE d.id = $1`, target.id).Scan(&baseID, &current, &erased)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail("NOT_FOUND", "document not found")
		}
		if err != nil {
			return err
		}
		// the base row lock serializes the quota check with the uploads and replacements of the base
		b, err := scanBaseRow(tx.QueryRow(ctx, `SELECT `+baseRowCols+` FROM knowledge_bases WHERE id = $1 FOR UPDATE`, baseID))
		if err != nil {
			return err
		}
		// the row is re-read under the lock: an erasure that began after the permission check is seen here
		if err := tx.QueryRow(ctx, `SELECT coalesce(d.original_key, ''), d.erasure_id IS NOT NULL FROM knowledge_documents d WHERE d.id = $1 FOR UPDATE`, target.id).Scan(&current, &erased); err != nil {
			return err
		}
		if erased {
			return fail("NOT_FOUND", "document not found")
		}
		// the permission is decided again under the locks: the base may have been archived, or the caller may have
		// stopped managing it, since the first check
		if err := b.canWrite(ctx, tx, v, true); err != nil {
			if se, ok := err.(*Error); ok && (se.Code == "FORBIDDEN" || se.Code == "NOT_FOUND") {
				return fail("NOT_FOUND", "document not found")
			}
			return err
		}
		if current != target.key {
			return fail("CONFLICT", "the original of document %s changed while this one was being uploaded; send it again", target.id)
		}
		if err := s.checkOriginalQuota(ctx, tx, b, target.id, n); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE knowledge_documents SET original_key = $2, original_sha256 = $3, original_byte_size = $4, original_media_type = $5 WHERE id = $1`,
			target.id, key, sum, n, media); err != nil {
			return err
		}
		if err := Audit(ctx, tx, tenant, v.Subject, "knowledge.original", target.id, map[string]any{"bytes": n, "mediaType": media}); err != nil {
			return err
		}
		out, err = scanDoc(tx.QueryRow(ctx, `SELECT `+docCols+` FROM knowledge_documents d JOIN sources s ON s.id = d.source_id WHERE d.id = $1`, target.id))
		return err
	})
	if err != nil {
		if derr := s.Originals.Delete(context.WithoutCancel(ctx), key); derr != nil && s.Log != nil {
			s.Log.Error("an uploaded original that was not recorded could not be deleted", "key", key, "error", derr)
		}
		return nil, err
	}
	if target.key != "" { // after the commit: the row no longer points to it
		if derr := s.Originals.Delete(context.WithoutCancel(ctx), target.key); derr != nil && s.Log != nil {
			s.Log.Error("a replaced original could not be deleted", "key", target.key, "error", derr)
		}
	}
	return out, nil
}

// GetOriginal returns the stored file after checking that the caller can read the document and the digest. The object is
// streamed from the store into a temporary file while it is hashed (memory stays bounded however large it is), the digest
// and size are checked, and only then is the file handed to the caller to send. The temporary file is created in
// Store.TempDir (the operator's writable scratch space: the service's root file system is read-only) and unlinked at
// once, keeping the descriptor, so a crash or a forgotten Close leaves nothing in the directory.
func (s *Store) GetOriginal(ctx context.Context, tenant, id string, v Visibility) (*OriginalFile, error) {
	if s.Originals == nil {
		return nil, fail("NOT_CONFIGURED", "no original store is configured")
	}
	var t *originalTarget
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) (err error) {
		if err := bindView(ctx, tx, v); err != nil {
			return err
		}
		t, err = findForOriginal(ctx, tx, tenant, v, id, false)
		return err
	})
	if err != nil {
		return nil, err
	}
	if t.key == "" {
		return nil, fail("NOT_FOUND", "document has no original")
	}
	var src io.ReadCloser
	if o, ok := s.Originals.(originalOpener); ok {
		src, err = o.Open(ctx, t.key, t.size)
	} else {
		var data []byte
		if data, err = s.Originals.Get(ctx, t.key, t.size); err == nil {
			src = io.NopCloser(bytes.NewReader(data))
		}
	}
	if err != nil {
		return nil, unavailable(err)
	}
	defer src.Close()
	tmp, err := os.CreateTemp(s.TempDir, "cmi-original-*")
	if err != nil {
		if s.Log != nil {
			s.Log.Error("the temporary directory for downloads is not writable (set IDP_CMI_TEMP_DIR to a writable one)", "dir", s.TempDir, "error", err)
		}
		return nil, fmt.Errorf("a download needs a writable temporary directory: %w", err)
	}
	_ = os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(src, t.size+1))
	if err != nil {
		_ = tmp.Close()
		return nil, unavailable(err)
	}
	if n != t.size || "sha256:"+hex.EncodeToString(h.Sum(nil)) != t.sum {
		_ = tmp.Close()
		if s.Log != nil {
			s.Log.Error("a stored original does not match its recorded digest", "document", t.id, "recordedBytes", t.size, "foundBytes", n)
		}
		return nil, fmt.Errorf("stored original of document %s does not match its recorded digest", t.id)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	f := &OriginalFile{Body: tmp, Size: n, MediaType: "application/octet-stream", Name: "original-" + t.id[:8]}
	if typ, ok := originalTypes[t.media]; ok {
		f.MediaType = t.media
		f.Name += "." + typ.Ext
	}
	return f, nil
}

// deleteOriginals removes every stored object of the documents and clears the columns (the erasure domain originals,
// D20). It lists and deletes the whole prefix of each document, not only the key the row points to: an object whose
// delete failed after a replacement, or whose upload was never recorded, is found too (D54). The barrier of the erasure
// (erasure_id) keeps PutOriginal from recording a new key after it was set. Afterwards the prefixes are listed again and
// the columns checked; anything left fails the domain.
func (s *Store) deleteOriginals(ctx context.Context, tenant string, documentIDs []string) (int, error) {
	if len(documentIDs) == 0 {
		return 0, nil
	}
	var keys []string
	err := s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT current_tenant()::text`).Scan(&tenant); err != nil { // the prefixes carry the database's spelling
			return err
		}
		rows, err := tx.Query(ctx, `SELECT original_key FROM knowledge_documents WHERE id = ANY($1::uuid[]) AND original_key IS NOT NULL`, documentIDs)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				return err
			}
			keys = append(keys, key)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, err
	}
	if s.Originals == nil {
		if len(keys) > 0 {
			return 0, fail("NOT_CONFIGURED", "documents have originals but no original store is configured")
		}
		return 0, nil
	}
	seen := map[string]bool{}
	var all []string
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			all = append(all, k)
		}
	}
	for _, id := range documentIDs {
		found, err := s.Originals.ListKeys(ctx, documentPrefix(tenant, id))
		if err != nil {
			return 0, err
		}
		for _, k := range found {
			if !seen[k] {
				seen[k] = true
				all = append(all, k)
			}
		}
	}
	for _, key := range all {
		if err := s.Originals.Delete(ctx, key); err != nil {
			return 0, err
		}
	}
	for _, id := range documentIDs {
		left, err := s.Originals.ListKeys(ctx, documentPrefix(tenant, id))
		if err != nil {
			return 0, err
		}
		if len(left) > 0 {
			return 0, fmt.Errorf("%d objects of document %s are still in the store after the erasure deleted them", len(left), id)
		}
	}
	n := 0
	err = s.InTenant(ctx, tenant, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE knowledge_documents SET original_key = NULL, original_sha256 = NULL, original_byte_size = NULL, original_media_type = NULL WHERE id = ANY($1::uuid[]) AND original_key IS NOT NULL`, documentIDs)
		if err != nil {
			return err
		}
		n = int(tag.RowsAffected())
		var left int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM knowledge_documents WHERE id = ANY($1::uuid[]) AND original_key IS NOT NULL`, documentIDs).Scan(&left); err != nil {
			return err
		}
		if left > 0 {
			return fmt.Errorf("%d documents still point to an original after the erasure deleted theirs", left)
		}
		return nil
	})
	return n, err
}
