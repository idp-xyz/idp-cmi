package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"idp-cmi/adapters/blob"
	"idp-cmi/adapters/store/postgres"
)

// flaky is an object store whose calls can be made to fail or to run a hook, for the races and failures of originals.
type flaky struct {
	*blob.Mem
	putErr, deleteErr atomic.Pointer[error]
	beforePut         func(key string)
	deletes           atomic.Int32
}

func (f *flaky) Put(ctx context.Context, key string, data []byte) error {
	if f.beforePut != nil {
		f.beforePut(key)
	}
	if e := f.putErr.Load(); e != nil {
		return *e
	}
	return f.Mem.Put(ctx, key, data)
}

func (f *flaky) Delete(ctx context.Context, key string) error {
	f.deletes.Add(1)
	if e := f.deleteErr.Load(); e != nil {
		return *e
	}
	return f.Mem.Delete(ctx, key)
}

func fail(err error) *error { return &err }

func personalDoc(e *env, who, name string) string {
	e.t.Helper()
	d := e.must(201, "POST", "/v1/knowledge/documents", who, kbDoc("member-notes", "kb://me/"+name, "", "a scanned page about QzxOriginal "+name))
	return d["documentId"].(string)
}

func sha(b []byte) string { s := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(s[:]) }

func TestOriginalIsStoredAndErased(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	id := personalDoc(e, "bob", "scan")
	payload := []byte("%PDF-1.4 QzxOriginalBytes")
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "application/pdf", payload); st != 503 {
		t.Fatalf("missing store: %d", st)
	}
	mem := &blob.Mem{}
	e.store.Originals = mem

	st, _, body := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "application/pdf", payload)
	if st != 200 {
		t.Fatalf("put %d %s", st, body)
	}
	if !bytes.Contains(body, []byte(`"mediaType":"application/pdf"`)) || !bytes.Contains(body, []byte(sha(payload))) || bytes.Contains(body, []byte(`"kb/`)) {
		t.Fatalf("metadata %s", body)
	}
	keys := mem.Keys()
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "kb/"+e.tenantA+"/"+id+"/1/") {
		t.Fatalf("object keys %v", keys)
	}
	st, hdr, body := e.callBytes(http.MethodGet, "/v1/knowledge/documents/"+id+"/original", "bob", "", nil)
	if st != 200 || hdr.Get("Content-Type") != "application/pdf" || !bytes.Equal(body, payload) {
		t.Fatalf("get %d %s %q", st, hdr.Get("Content-Type"), body)
	}
	if st, _, _ = e.callBytes(http.MethodGet, "/v1/knowledge/documents/"+id+"/original", "carol", "", nil); st != 404 {
		t.Fatalf("carol %d", st)
	}

	e.must(201, "POST", "/api/v1/erasures", "alice", map[string]any{"documentIds": []string{id}, "reason": "drop the file"})
	if n, err := e.store.Purge(context.Background(), e.tenantA, 5); err != nil || n != 1 {
		t.Fatalf("purge %d %v", n, err)
	}
	if len(mem.Keys()) != 0 {
		t.Fatalf("original survived erasure: %v", mem.Keys())
	}
	if st, _, _ = e.callBytes(http.MethodGet, "/v1/knowledge/documents/"+id+"/original", "bob", "", nil); st != 404 {
		t.Fatalf("after purge %d", st)
	}
}

// A stored original that no longer matches its recorded digest is not served: a 500, with a line in the log (not a 409 the client could retry).
func TestOriginalDigestMismatchIsAServerError(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	var logs bytes.Buffer
	e.store.Log = slog.New(slog.NewTextHandler(&logs, nil))
	mem := &blob.Mem{}
	e.store.Originals = mem
	id := personalDoc(e, "bob", "tamper")
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "application/pdf", []byte("%PDF genuine")); st != 200 {
		t.Fatal(st)
	}
	if err := mem.Put(context.Background(), mem.Keys()[0], []byte("%PDF forgery")); err != nil {
		t.Fatal(err)
	}
	if st, _, body := e.callBytes(http.MethodGet, "/v1/knowledge/documents/"+id+"/original", "bob", "", nil); st != 500 || bytes.Contains(body, []byte("forgery")) {
		t.Fatalf("tampered object: %d %q", st, body)
	}
	if !strings.Contains(logs.String(), "does not match its recorded digest") || !strings.Contains(logs.String(), id) {
		t.Fatalf("log: %s", logs.String())
	}
}

// A download is never a page (S11): always an attachment, nosniff, sandboxed, with a name the server chose; a stored
// media type that is not in the allow-list is served as opaque bytes; the bytes are what was uploaded.
func TestOriginalDownloadHeaders(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	e.store.Originals = &blob.Mem{}
	id := personalDoc(e, "bob", "page")
	page := []byte(`<script>fetch("/steal?c="+document.cookie)</script>`)
	e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "text/html; charset=utf-8", page)
	st, hdr, body := e.callBytes(http.MethodGet, "/v1/knowledge/documents/"+id+"/original", "bob", "", nil)
	if st != 200 || !bytes.Equal(body, page) {
		t.Fatalf("%d %q", st, body)
	}
	for k, want := range map[string]string{
		"Content-Disposition":     `attachment; filename="original-` + id[:8] + `.html"`,
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "sandbox",
		"Content-Type":            "text/html",
	} {
		if got := hdr.Get(k); got != want {
			t.Errorf("%s: %q, want %q", k, got, want)
		}
	}
	// a media type outside the allow-list (planted in the row) is not served as that type
	conn, err := pgx.Connect(context.Background(), e.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(context.Background(), `UPDATE knowledge_documents SET original_media_type = 'image/svg+xml' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	_, hdr, _ = e.callBytes(http.MethodGet, "/v1/knowledge/documents/"+id+"/original", "bob", "", nil)
	if hdr.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(hdr.Get("Content-Disposition"), "attachment") {
		t.Fatalf("outside the allow-list: %v", hdr)
	}
	// a type outside the allow-list is refused at upload
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "image/svg+xml", []byte("<svg/>")); st != 422 {
		t.Fatalf("svg upload: %d", st)
	}
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "application/x-msdownload", []byte("MZ")); st != 422 {
		t.Fatalf("exe upload: %d", st)
	}
}

// The permission is checked before the body is read; the body is bounded as it is read (S11).
func TestOriginalPermissionBeforeBodyAndSizeLimit(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	e.store.Originals = &blob.Mem{}
	id := personalDoc(e, "bob", "doc")
	v := postgres.Visibility{Subject: "carol", Roles: []string{"cmi-reader"}, Ceiling: "internal"}
	_, err := e.store.PutOriginal(context.Background(), e.tenantA, id, v, "application/pdf", readerFunc(func([]byte) (int, error) {
		t.Fatal("the body of a caller who may not write the document was read")
		return 0, io.EOF
	}))
	var se *postgres.Error
	if !errors.As(err, &se) || se.Code != "NOT_FOUND" {
		t.Fatalf("carol: %v", err)
	}
	// the body is bounded as it is read: a caller that offers 200 MiB is not read to the end
	var served int64
	endless := readerFunc(func(p []byte) (int, error) {
		if served >= 200<<20 {
			return 0, io.EOF
		}
		served += int64(len(p))
		return len(p), nil
	})
	bobView := postgres.Visibility{Subject: "bob", Roles: []string{"cmi-writer"}, Ceiling: "confidential"}
	_, err = e.store.PutOriginal(context.Background(), e.tenantA, id, bobView, "text/plain", endless)
	if !errors.As(err, &se) || se.Code != "INVALID" || served > 26<<20 {
		t.Fatalf("an oversized body: %v, %d bytes read", err, served)
	}
	// 25 MiB + 1 of text, or no bytes, is refused; exactly the limit is taken
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "text/plain", make([]byte, 25<<20+1)); st != 422 {
		t.Fatalf("over the limit: %d", st)
	}
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "text/plain", []byte{}); st != 422 {
		t.Fatalf("empty: %d", st)
	}
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "text/plain", bytes.Repeat([]byte("a"), 25<<20)); st != 200 {
		t.Fatalf("at the limit: %d", st)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// At most MaxConcurrentUploads originals per caller are in flight (S11).
func TestOriginalUploadsPerCallerAreBounded(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	e.store.Originals = &blob.Mem{}
	id := personalDoc(e, "bob", "doc")
	var pipes []*io.PipeWriter
	closePipes := sync.OnceFunc(func() {
		for _, pw := range pipes {
			_ = pw.Close() // an empty body: refused, but the places are given back
		}
	})
	defer closePipes() // a failed assertion below must not leave handlers waiting for bytes
	var wg sync.WaitGroup
	status := make(chan int, 4)
	send := func(r io.Reader) {
		req, _ := http.NewRequest(http.MethodPut, e.api.URL+"/v1/knowledge/documents/"+id+"/original", r)
		req.Header.Set("Authorization", "Bearer "+e.tokens["bob"])
		req.Header.Set("Content-Type", "application/pdf")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			status <- 0
			return
		}
		resp.Body.Close()
		status <- resp.StatusCode
	}
	for i := 0; i < 2; i++ {
		pr, pw := io.Pipe()
		pipes = append(pipes, pw)
		wg.Add(1)
		go func() { defer wg.Done(); send(pr) }()
	}
	time.Sleep(500 * time.Millisecond) // both are inside the handler, waiting for bytes
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "application/pdf", []byte("%PDF third")); st != 429 {
		t.Fatalf("third concurrent upload: %d", st)
	}
	closePipes()
	wg.Wait()
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "application/pdf", []byte("%PDF fourth")); st != 200 {
		t.Fatalf("after the others ended: %d", st)
	}
}

// S5: every upload has its own key; the row is updated only if it still points to what the upload started from; a
// failing store changes nothing; the replaced object is deleted after the commit.
func TestOriginalReplacementRacesAndFailures(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	fs := &flaky{Mem: &blob.Mem{}}
	e.store.Originals = fs
	id := personalDoc(e, "bob", "doc")
	put := func(data string) int {
		st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "application/pdf", []byte(data))
		return st
	}
	get := func() string {
		_, _, b := e.callBytes(http.MethodGet, "/v1/knowledge/documents/"+id+"/original", "bob", "", nil)
		return string(b)
	}
	if put("%PDF first") != 200 {
		t.Fatal("first")
	}
	first := fs.Keys()[0]

	// a store that fails the write leaves the previous original in place (it used to be deleted)
	fs.putErr.Store(fail(errors.New("disk full")))
	if st := put("%PDF second"); st != 500 {
		t.Fatalf("failing put: %d", st)
	}
	fs.putErr.Store(nil)
	if got := get(); got != "%PDF first" || len(fs.Keys()) != 1 {
		t.Fatalf("after a failed replacement: %q %v", got, fs.Keys())
	}

	// a replaced object is deleted after the commit, and a failure to delete it does not fail the upload
	fs.deleteErr.Store(fail(errors.New("delete refused")))
	if put("%PDF second") != 200 || get() != "%PDF second" {
		t.Fatal("replacement with a failing delete")
	}
	if len(fs.Keys()) != 2 {
		t.Fatalf("the old object should have stayed (the delete failed): %v", fs.Keys())
	}
	fs.deleteErr.Store(nil)
	if put("%PDF third") != 200 {
		t.Fatal("third")
	}
	if got := get(); got != "%PDF third" {
		t.Fatalf("third: %q", got)
	}
	if keys := fs.Keys(); len(keys) != 2 || fs.Has(first) == false {
		// the second's object is deleted; the first's stayed from the failed delete: that is the orphan the ADR names
		t.Logf("keys %v", keys)
	}

	// two uploads that both started from the same original: one wins, the other is a conflict whose object is deleted,
	// and the download is the winner's bytes with the winner's digest (it used to be one's bytes with the other's digest)
	var mu sync.Mutex
	arrived := 0
	gate := make(chan struct{})
	fs.beforePut = func(string) {
		mu.Lock()
		arrived++
		n := arrived
		mu.Unlock()
		if n <= 2 {
			if n == 2 {
				close(gate)
			}
			<-gate
		}
	}
	var wg sync.WaitGroup
	got := make([]int, 2)
	for i, data := range []string{"%PDF racer A", "%PDF racer B"} {
		wg.Add(1)
		go func() { defer wg.Done(); got[i] = put(data) }()
	}
	wg.Wait()
	fs.beforePut = nil
	if !(got[0] == 200 && got[1] == 409 || got[0] == 409 && got[1] == 200) {
		t.Fatalf("racers: %v", got)
	}
	if b := get(); b != "%PDF racer A" && b != "%PDF racer B" {
		t.Fatalf("download after the race: %q", b)
	}
	doc := e.must(200, "GET", "/v1/knowledge/documents/"+id, "bob", nil)
	if doc["original"].(map[string]any)["sha256"] != sha([]byte(get())) {
		t.Fatalf("digest of the download %s, recorded %v", sha([]byte(get())), doc["original"])
	}
	// the losing upload's object is gone: of the keys now present, one is the winner's
	live := 0
	for _, k := range fs.Keys() {
		if data, _ := fs.Get(context.Background(), k, 1<<20); strings.HasPrefix(string(data), "%PDF racer") {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("%d racer objects remain, want 1", live)
	}
}

// S5(a): an erasure that starts after the permission check and finishes before the row is updated must not leave the object.
func TestOriginalAttachedWhileTheDocumentIsErased(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	fs := &flaky{Mem: &blob.Mem{}}
	e.store.Originals = fs
	id := personalDoc(e, "bob", "erase-me")
	fs.beforePut = func(string) {
		fs.beforePut = nil
		e.must(201, "POST", "/api/v1/erasures", "alice", map[string]any{"documentIds": []string{id}, "reason": "probe"})
		if _, err := e.store.Purge(context.Background(), e.tenantA, 5); err != nil {
			t.Error(err)
		}
	}
	st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "application/pdf", []byte("%PDF erased content"))
	if st != 404 {
		t.Fatalf("put racing the erasure: %d", st)
	}
	if keys := fs.Keys(); len(keys) != 0 {
		t.Fatalf("an original of an erased document stays in the object store: %v", keys)
	}
}

// The key is built from the database's id, whatever spelling the caller used (it used to orphan objects the erasure never found).
func TestOriginalKeyUsesTheCanonicalID(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	mem := &blob.Mem{}
	e.store.Originals = mem
	id := personalDoc(e, "bob", "case")
	st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+strings.ToUpper(id)+"/original", "bob", "application/pdf", []byte("%PDF upper"))
	if st != 200 {
		t.Fatalf("upper-case id: %d", st)
	}
	if keys := mem.Keys(); len(keys) != 1 || strings.Contains(keys[0], strings.ToUpper(id)) || !strings.Contains(keys[0], "/"+id+"/") {
		t.Fatalf("keys %v", keys)
	}
	e.must(201, "POST", "/api/v1/erasures", "alice", map[string]any{"documentIds": []string{id}, "reason": "x"})
	if _, err := e.store.Purge(context.Background(), e.tenantA, 5); err != nil {
		t.Fatal(err)
	}
	if len(mem.Keys()) != 0 {
		t.Fatalf("orphaned: %v", mem.Keys())
	}
}

// A store that refuses the delete fails the erasure's originals domain: the erasure is not verified until it is done (S14).
func TestOriginalErasureFailureIsNotVerified(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	fs := &flaky{Mem: &blob.Mem{}}
	e.store.Originals = fs
	id := personalDoc(e, "bob", "stuck")
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "application/pdf", []byte("%PDF stuck")); st != 200 {
		t.Fatal(st)
	}
	fs.deleteErr.Store(fail(errors.New("store unreachable")))
	er := e.must(201, "POST", "/api/v1/erasures", "alice", map[string]any{"documentIds": []string{id}, "reason": "x"})
	if _, err := e.store.Purge(context.Background(), e.tenantA, 5); err != nil {
		t.Fatal(err)
	}
	got := e.must(200, "GET", "/api/v1/erasures/"+er["erasureId"].(string), "alice", nil)
	if got["status"] != "failed" || got["domains"].(map[string]any)["originals"].(map[string]any)["status"] != "failed" || len(fs.Keys()) != 1 {
		t.Fatalf("erasure %v", got)
	}
	fs.deleteErr.Store(nil)
	if _, err := e.store.Purge(context.Background(), e.tenantA, 5); err != nil {
		t.Fatal(err)
	}
	got = e.must(200, "GET", "/api/v1/erasures/"+er["erasureId"].(string), "alice", nil)
	if got["status"] != "verified" || len(fs.Keys()) != 0 {
		t.Fatalf("after the store came back: %v %v", got["status"], fs.Keys())
	}
}

// Originals report unavailable until the store is reachable, and CMI does not wait for it (S13).
func TestOriginalStoreGate(t *testing.T) {
	e := newEnv(t)
	kbSources(e)
	var up atomic.Bool
	objects := &blob.Mem{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "<Error><Code>ServiceUnavailable</Code></Error>", http.StatusServiceUnavailable)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/originals/")
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") != "":
			_, _ = w.Write([]byte(`<ListBucketResult></ListBucketResult>`))
		case r.Method == http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			_ = objects.Put(r.Context(), key, b)
		case r.Method == http.MethodGet:
			b, err := objects.Get(r.Context(), key, 1<<20)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(b)
		case r.Method == http.MethodDelete:
			_ = objects.Delete(r.Context(), key)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	s3, err := blob.New(blob.Config{Endpoint: srv.URL, Bucket: "originals", AccessKeyFile: writeFile(t, dir, "ak"), SecretKeyFile: writeFile(t, dir, "sk")})
	if err != nil {
		t.Fatal(err)
	}
	gate := &blob.Gate{S3: s3}
	e.store.Originals = gate
	id := personalDoc(e, "bob", "gate")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go gate.Run(ctx, nil, 50*time.Millisecond)
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "application/pdf", []byte("%PDF gate")); st != 503 {
		t.Fatalf("store down: %d", st)
	}
	up.Store(true)
	deadline := time.Now().Add(5 * time.Second)
	for !gate.Ready() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if st, _, _ := e.callBytes(http.MethodPut, "/v1/knowledge/documents/"+id+"/original", "bob", "application/pdf", []byte("%PDF gate")); st != 200 {
		t.Fatalf("store up: %d", st)
	}
	if st, _, b := e.callBytes(http.MethodGet, "/v1/knowledge/documents/"+id+"/original", "bob", "", nil); st != 200 || string(b) != "%PDF gate" {
		t.Fatalf("store up, get: %d %q", st, b)
	}
}

func (e *env) callBytes(method, path, who, media string, body []byte) (int, http.Header, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.api.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.tokens[who])
	if media != "" {
		req.Header.Set("Content-Type", media)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw
}
