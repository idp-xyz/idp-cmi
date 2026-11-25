package workload

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type fixture struct {
	srv   *httptest.Server
	hits  atomic.Int32
	life  atomic.Int32
	state atomic.Int32 // the HTTP status to answer (0 = 200)
	ts    *TokenSource
	now   atomic.Int64
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{}
	f.life.Store(300)
	f.now.Store(time.Now().Unix())
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte("s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, pw, ok := r.BasicAuth()
		_ = r.ParseForm()
		if r.Method != http.MethodPost || !ok || id != "cmi-ingest" || pw != "s3cr3t" || r.PostForm.Get("grant_type") != "client_credentials" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if s := f.state.Load(); s != 0 {
			http.Error(w, "boom", int(s))
			return
		}
		n := f.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-" + string(rune('0'+n)), "token_type": "Bearer", "expires_in": f.life.Load()})
	}))
	t.Cleanup(f.srv.Close)
	f.ts = &TokenSource{TokenURL: f.srv.URL, ClientID: "cmi-ingest", SecretFile: secret, HTTP: NewHTTPClient(5*time.Second, nil),
		Now: func() time.Time { return time.Unix(f.now.Load(), 0) }}
	return f
}

func TestTheTokenIsKeptAndRenewedBeforeItExpires(t *testing.T) {
	f := newFixture(t)
	a, err := f.ts.Token(context.Background())
	if err != nil || a != "tok-1" {
		t.Fatalf("%q %v", a, err)
	}
	f.now.Add(200) // 300 s life: renewed from 270 s on
	if b, _ := f.ts.Token(context.Background()); b != "tok-1" || f.hits.Load() != 1 {
		t.Fatalf("kept token %q after %d fetches", b, f.hits.Load())
	}
	f.now.Add(71)
	if c, _ := f.ts.Token(context.Background()); c != "tok-2" || f.hits.Load() != 2 {
		t.Fatalf("renewed token %q after %d fetches", c, f.hits.Load())
	}
}

func TestAShortLivedTokenIsStillUsedOnce(t *testing.T) {
	f := newFixture(t)
	f.life.Store(20)
	if _, err := f.ts.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.now.Add(5)
	if _, err := f.ts.Token(context.Background()); err != nil || f.hits.Load() != 1 {
		t.Fatalf("a 20 s token is renewed after 10 s, not at once (%d fetches, %v)", f.hits.Load(), err)
	}
	f.now.Add(6)
	if _, err := f.ts.Token(context.Background()); err != nil || f.hits.Load() != 2 {
		t.Fatalf("%d fetches %v", f.hits.Load(), err)
	}
}

func TestInvalidateDropsOnlyTheTokenThatWasRefused(t *testing.T) {
	f := newFixture(t)
	a, _ := f.ts.Token(context.Background())
	f.ts.Invalidate("some-other-token")
	if b, _ := f.ts.Token(context.Background()); b != a {
		t.Fatal("another token's refusal dropped this one")
	}
	f.ts.Invalidate(a)
	if b, _ := f.ts.Token(context.Background()); b == a {
		t.Fatal("a refused token was kept")
	}
}

func TestRefusalsOfTheTokenEndpointAreErrorsWithoutTheSecret(t *testing.T) {
	f := newFixture(t)
	f.state.Store(http.StatusServiceUnavailable)
	_, err := f.ts.Token(context.Background())
	e, ok := err.(*Error)
	if !ok || !e.Transient() || e.Status != 503 {
		t.Fatalf("%v", err)
	}
	f.state.Store(http.StatusBadRequest)
	_, err = f.ts.Token(context.Background())
	if e, ok := err.(*Error); !ok || e.Transient() {
		t.Fatalf("%v", err)
	}
	f.ts.SecretFile = filepath.Join(t.TempDir(), "missing")
	if _, err = f.ts.Token(context.Background()); err == nil {
		t.Fatal("no secret file")
	}
}

func TestTheTokenEndpointIsNotFollowedToAnotherAddress(t *testing.T) {
	f := newFixture(t)
	redirect := httptest.NewServer(http.RedirectHandler(f.srv.URL, http.StatusTemporaryRedirect))
	defer redirect.Close()
	f.ts.TokenURL = redirect.URL
	if _, err := f.ts.Token(context.Background()); err == nil || f.hits.Load() != 0 {
		t.Fatalf("a redirect was followed: %v", err)
	}
}
