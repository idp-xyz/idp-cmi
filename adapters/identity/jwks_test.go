package identity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func jwksServer(t *testing.T, up *bool) *httptest.Server {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	b := func(n []byte) string { return base64.RawURLEncoding.EncodeToString(n) }
	body, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"kty": "EC", "kid": "k1", "use": "sig", "crv": "P-256",
		"x": b(k.X.FillBytes(make([]byte, 32))), "y": b(k.Y.FillBytes(make([]byte, 32)))}}})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !*up {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(body)
	}))
}

// Keys are used while the issuer cannot be reached, but not for ever: a key the issuer withdrew must not stay trusted as long as
// its endpoint stays down.
func TestJWKSKeysGoStaleWhenTheIssuerStaysDown(t *testing.T) {
	up := true
	srv := jwksServer(t, &up)
	defer srv.Close()
	j := &JWKS{URL: srv.URL, HTTP: srv.Client(), TTL: time.Minute, MinRefresh: time.Millisecond, MaxStale: time.Hour}
	if _, err := j.Key(context.Background(), "k1"); err != nil {
		t.Fatal(err)
	}
	up = false
	j.mu.Lock()
	j.fetchedAt = time.Now().Add(-2 * time.Minute) // past the TTL: a refresh is tried and fails
	j.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	if _, err := j.Key(context.Background(), "k1"); err != nil {
		t.Fatalf("within MaxStale the cached key still serves: %v", err)
	}
	j.mu.Lock()
	j.fetchedAt = time.Now().Add(-2 * time.Hour)
	j.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	if _, err := j.Key(context.Background(), "k1"); err == nil {
		t.Fatal("past MaxStale with the issuer down, the key must no longer be trusted")
	}
	up = true
	time.Sleep(5 * time.Millisecond)
	if _, err := j.Key(context.Background(), "k1"); err != nil {
		t.Fatalf("when the issuer is back the keys are fetched again: %v", err)
	}
}
