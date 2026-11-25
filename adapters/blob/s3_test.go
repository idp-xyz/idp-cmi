package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// The examples of AWS's "Signature Calculations for the Authorization Header" (Amazon S3, signature version 4).
func TestSignatureMatchesTheAWSExamples(t *testing.T) {
	at := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	ak, sk := "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	sig := func(req *http.Request) string {
		sign(req, emptySHA, ak, sk, "us-east-1", at)
		a := req.Header.Get("Authorization")
		return a[strings.LastIndex(a, "Signature=")+len("Signature="):]
	}
	get, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	get.Header.Set("Range", "bytes=0-9")
	if got := sig(get); got != "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41" {
		t.Errorf("GET object: %s", got)
	}
	if !strings.Contains(get.Header.Get("Authorization"), "Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date,") {
		t.Errorf("%s", get.Header.Get("Authorization"))
	}
	list, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/?"+canonicalQuery(url.Values{"prefix": {"J"}, "max-keys": {"2"}}), nil)
	if got := sig(list); got != "34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7" {
		t.Errorf("list objects: %s", got)
	}
}

func keys(t *testing.T) (string, string) {
	t.Helper()
	d := t.TempDir()
	ak, sk := filepath.Join(d, "ak"), filepath.Join(d, "sk")
	_ = os.WriteFile(ak, []byte("access\n"), 0o600)
	_ = os.WriteFile(sk, []byte("secret"), 0o600)
	return ak, sk
}

func TestClientRequests(t *testing.T) {
	type seen struct{ method, path, query, sse, auth, body string }
	var got []seen
	objects := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, seen{r.Method, r.URL.EscapedPath(), r.URL.RawQuery, r.Header.Get("X-Amz-Server-Side-Encryption"), r.Header.Get("Authorization"), string(b)})
		key := strings.TrimPrefix(r.URL.Path, "/files/")
		switch {
		case r.Method == http.MethodPut:
			objects[key] = b
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			_, _ = io.WriteString(w, `<ListBucketResult><Contents><Key>a/1</Key><Size>3</Size><LastModified>2026-11-13T01:02:03.000Z</LastModified></Contents><IsTruncated>true</IsTruncated><NextContinuationToken>n2</NextContinuationToken></ListBucketResult>`)
		case r.Method == http.MethodGet:
			if d, ok := objects[key]; ok {
				_, _ = w.Write(d)
				return
			}
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>secret detail</Message></Error>`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(204)
		}
	}))
	defer srv.Close()
	ak, sk := keys(t)
	s, err := New(Config{Endpoint: srv.URL, Bucket: "files", AccessKeyFile: ak, SecretKeyFile: sk, SSE: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Put(ctx, "t1/f 1", []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if d, err := s.Get(ctx, "t1/f 1", 3); err != nil || string(d) != "abc" {
		t.Fatalf("%q %v", d, err)
	}
	if _, err := s.Get(ctx, "t1/f 1", 2); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
	if _, err := s.Get(ctx, "missing", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	objs, next, err := s.List(ctx, "a/", "")
	if err != nil || len(objs) != 1 || objs[0].Key != "a/1" || objs[0].Size != 3 || objs[0].LastModified.Year() != 2026 || next != "n2" {
		t.Fatalf("%+v %q %v", objs, next, err)
	}
	if err := s.Delete(ctx, "t1/f 1"); err != nil {
		t.Fatal(err)
	}
	put := got[0]
	if put.method != "PUT" || put.path != "/files/t1/f%201" || put.sse != "AES256" || put.body != "abc" || !strings.HasPrefix(put.auth, "AWS4-HMAC-SHA256 Credential=access/") ||
		!strings.Contains(put.auth, "x-amz-server-side-encryption") {
		t.Fatalf("%+v", put)
	}
	if got[4].query != "list-type=2&max-keys=1000&prefix=a%2F" {
		t.Fatalf("list query %q", got[4].query)
	}
}

func TestConfiguration(t *testing.T) {
	ak, sk := keys(t)
	for name, c := range map[string]Config{
		"no scheme":     {Endpoint: "files-s3:9000", Bucket: "files"},
		"path":          {Endpoint: "http://h:9000/x", Bucket: "files"},
		"bucket":        {Endpoint: "http://h:9000", Bucket: "Files_1"},
		"no key file":   {Endpoint: "http://h:9000", Bucket: "files", AccessKeyFile: "/nonexistent", SecretKeyFile: sk},
		"empty ca file": {Endpoint: "https://h", Bucket: "files", AccessKeyFile: ak, SecretKeyFile: sk, CAFile: ak},
	} {
		if c.AccessKeyFile == "" {
			c.AccessKeyFile, c.SecretKeyFile = ak, sk
		}
		if _, err := New(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `<Error><Code>SignatureDoesNotMatch</Code><Message>the secret was x</Message></Error>`)
	}))
	defer srv.Close()
	s, _ := New(Config{Endpoint: srv.URL, Bucket: "files", AccessKeyFile: ak, SecretKeyFile: sk})
	err := s.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") || strings.Contains(err.Error(), "secret was") {
		t.Fatalf("%v", err)
	}
}

// A check the store refuses (403) or fails is not a missing bucket: only a 404 creates one (S13).
func TestEnsureBucketCreatesOnlyAMissingBucket(t *testing.T) {
	ak, sk := keys(t)
	for _, tc := range []struct {
		name       string
		checkCode  int
		wantCreate bool
		wantErr    bool
	}{
		{"missing", 404, true, false},
		{"refused", 403, false, true},
		{"server error", 500, false, true},
		{"present", 200, false, false},
	} {
		var creates int
		created := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPut {
				creates++
				created = true
				return
			}
			code := tc.checkCode
			if created {
				code = 200
			}
			w.WriteHeader(code)
			if code != 200 {
				_, _ = io.WriteString(w, `<Error><Code>X</Code></Error>`)
			} else {
				_, _ = io.WriteString(w, `<ListBucketResult></ListBucketResult>`)
			}
		}))
		s, _ := New(Config{Endpoint: srv.URL, Bucket: "files", AccessKeyFile: ak, SecretKeyFile: sk})
		err := s.EnsureBucket(context.Background())
		srv.Close()
		if (err != nil) != tc.wantErr || (creates > 0) != tc.wantCreate {
			t.Errorf("%s: err %v, bucket creations %d", tc.name, err, creates)
		}
		var se *StatusError
		if tc.wantErr && (!errors.As(err, &se) || se.Status != tc.checkCode) {
			t.Errorf("%s: the status is not in the error: %v", tc.name, err)
		}
	}
}

// The gate answers ErrUnavailable until the store has been reached, with the 20 s check bounded per attempt, and
// does not block its caller (S13).
func TestGateAnswersUnavailableUntilTheStoreIsReached(t *testing.T) {
	ak, sk := keys(t)
	var up atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			w.WriteHeader(503)
			return
		}
		_, _ = io.WriteString(w, `<ListBucketResult></ListBucketResult>`)
	}))
	defer srv.Close()
	s, _ := New(Config{Endpoint: srv.URL, Bucket: "files", AccessKeyFile: ak, SecretKeyFile: sk})
	g := &Gate{S3: s}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { g.Run(ctx, nil, 10*time.Millisecond); close(done) }()
	if err := g.Put(ctx, "k", []byte("x")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("put before the store is up: %v", err)
	}
	if _, err := g.Get(ctx, "k", 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("get before: %v", err)
	}
	if err := g.Delete(ctx, "k"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("delete before: %v", err)
	}
	up.Store(true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the gate did not notice the store")
	}
	if !g.Ready() {
		t.Fatal("not ready")
	}
}

func TestTransientErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"gate":     {ErrUnavailable, true},
		"503":      {fmt.Errorf("x: %w", &StatusError{Status: 503}), true},
		"429":      {&StatusError{Status: 429}, true},
		"403":      {&StatusError{Status: 403}, false},
		"404":      {&StatusError{Status: 404}, false},
		"timeout":  {context.DeadlineExceeded, true},
		"no route": {&net.OpError{Op: "dial", Err: errors.New("refused")}, true},
		"other":    {errors.New("digest"), false},
	} {
		if got := Transient(tc.err); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// ListKeys follows the continuation tokens; Open streams and refuses an object that says it is too large.
func TestListKeysAndOpen(t *testing.T) {
	ak, sk := keys(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" && r.URL.Query().Get("continuation-token") == "":
			_, _ = io.WriteString(w, `<ListBucketResult><Contents><Key>p/1</Key></Contents><IsTruncated>true</IsTruncated><NextContinuationToken>t2</NextContinuationToken></ListBucketResult>`)
		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			_, _ = io.WriteString(w, `<ListBucketResult><Contents><Key>p/2</Key></Contents><IsTruncated>false</IsTruncated></ListBucketResult>`)
		case strings.HasSuffix(r.URL.Path, "/big"):
			_, _ = w.Write(make([]byte, 100))
		case strings.HasSuffix(r.URL.Path, "/small"):
			_, _ = io.WriteString(w, "hello")
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	s, _ := New(Config{Endpoint: srv.URL, Bucket: "files", AccessKeyFile: ak, SecretKeyFile: sk})
	ctx := context.Background()
	got, err := s.ListKeys(ctx, "p/")
	if err != nil || strings.Join(got, ",") != "p/1,p/2" {
		t.Fatalf("%v %v", got, err)
	}
	rc, err := s.Open(ctx, "small", 10)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Fatalf("%q", b)
	}
	if _, err := s.Open(ctx, "big", 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("big: %v", err)
	}
	if _, err := s.Open(ctx, "missing", 10); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}
