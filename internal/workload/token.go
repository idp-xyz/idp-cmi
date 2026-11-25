// Package workload holds CMI's own identity toward the platform: an OAuth 2.0 client_credentials grant (RFC 6749 4.4) for the
// ingest workload (ADR-0001 D62). The token endpoint, client id and the file with the secret are settings; the token is
// kept and renewed before it expires. Keycloak is only what answers today: this is the standard grant, and any OIDC
// provider that offers it serves.
package workload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// TokenSource fetches and keeps an access token.
type TokenSource struct {
	TokenURL   string
	ClientID   string
	SecretFile string // read at every renewal, so a rotated secret is picked up
	Scope      string // optional
	HTTP       *http.Client
	Now        func() time.Time

	mu     sync.Mutex
	token  string
	expiry time.Time
	renew  time.Time
}

// NewHTTPClient is the client for the token endpoint: no environment proxy, no redirects, a bounded wait.
func NewHTTPClient(timeout time.Duration, tr http.RoundTripper) *http.Client {
	if tr == nil {
		tr = &http.Transport{Proxy: nil}
	}
	return &http.Client{Timeout: timeout, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (t *TokenSource) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// Token returns a token that is not about to expire.
func (t *TokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && t.now().Before(t.renew) {
		return t.token, nil
	}
	return t.fetch(ctx)
}

// Invalidate drops the kept token (the gateway refused it): the next Token fetches a new one.
func (t *TokenSource) Invalidate(bad string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token == bad {
		t.token, t.renew = "", time.Time{}
	}
}

func (t *TokenSource) fetch(ctx context.Context) (string, error) {
	secret, err := os.ReadFile(t.SecretFile)
	if err != nil {
		return "", fmt.Errorf("the client secret file cannot be read: %w", err)
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	if t.Scope != "" {
		form.Set("scope", t.Scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(url.QueryEscape(t.ClientID), url.QueryEscape(strings.TrimSpace(string(secret))))
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("the token endpoint did not answer: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("the token endpoint's answer could not be read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// the body of an error may name the client; it is never the secret, but it is not logged either: the status is enough
		return "", &Error{Status: resp.StatusCode}
	}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" || (out.TokenType != "" && !strings.EqualFold(out.TokenType, "bearer")) {
		return "", errors.New("the token endpoint's answer has no bearer access token")
	}
	life := time.Duration(out.ExpiresIn) * time.Second
	if life <= 0 {
		life = 5 * time.Minute
	}
	now := t.now()
	// renewed before it expires: a tenth of its life ahead, at least 30 s (a token that lives less than that is renewed each time)
	lead := max(life/10, 30*time.Second)
	if lead >= life {
		lead = life / 2
	}
	t.token, t.expiry, t.renew = out.AccessToken, now.Add(life), now.Add(life-lead)
	return t.token, nil
}

// Error is a refusal of the token endpoint. A 5xx or 429 is the endpoint's trouble; anything else is the client's credentials.
type Error struct{ Status int }

func (e *Error) Error() string { return fmt.Sprintf("the token endpoint answered HTTP %d", e.Status) }

// Transient reports whether trying again later may help.
func (e *Error) Transient() bool { return e.Status >= 500 || e.Status == http.StatusTooManyRequests }
