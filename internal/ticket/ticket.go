// Package ticket signs and verifies the tickets of CMI's internal file endpoint (ADR-0001 D62).
//
// idp-sandbox's sandbox_import_file and sandbox_export_file take a ticket as an argument and present it to the file endpoint
// (IDP_SANDBOX_FILES_URL). The sandbox reads three claims of it without verifying the signature - tenant_id, sub and kind - and
// refuses a ticket that is not its caller's; it then hands the whole ticket on. So the signature, the audience, the expiry, the
// kind and the single use are all checked here, by the endpoint that issued it: nothing else does.
//
// A ticket is a three-part, JWS-shaped token (the sandbox's Peek needs that shape) signed with HMAC-SHA256 under a key that only
// CMI holds. tenant_id and sub are the identity of the ingest workload (the caller the sandbox compares them with), not the
// document's tenant; the document's own tenant, id and version, the conversion job and the object it may read are claims of
// CMI's own.
package ticket

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Issuer is the iss claim of every ticket.
const Issuer = "idp-cmi"

// Kinds of a ticket: a read ticket fetches the original of its document; a write ticket delivers the conversion result.
const (
	Read  = "read"
	Write = "write"
)

// MaxLen is the longest ticket the sandbox passes on.
const MaxLen = 8192

// MinKeyBytes is the shortest accepted signing key.
const MinKeyBytes = 32

// Claims of a ticket.
type Claims struct {
	// the ingest workload, as the sandbox knows its caller
	Tenant  string `json:"tenant_id"`
	Subject string `json:"sub"`
	Kind    string `json:"kind"`
	// standard
	Issuer   string `json:"iss"`
	Audience string `json:"aud"`
	Expires  int64  `json:"exp"`
	IssuedAt int64  `json:"iat"`
	ID       string `json:"jti"`
	// CMI's own: the document the ticket is for, the conversion job, and what a read ticket may fetch
	DocTenant string `json:"dt"`
	Document  string `json:"doc"`
	Version   int    `json:"ver"`
	Job       string `json:"jid"`
	Attempt   int    `json:"att"` // the attempt of the job: a ticket of an attempt that has been given up is worth nothing
	Key       string `json:"key"`
	SHA256    string `json:"sha"`
}

// Signer signs tickets.
type Signer struct {
	Key      []byte
	Audience string // the file endpoint (the URL the sandbox is configured with)
	TTL      time.Duration
	Now      func() time.Time
}

// Verifier verifies tickets.
type Verifier struct {
	Key      []byte
	Audience string
	Now      func() time.Time
	// Skew is how far an iat in the future is tolerated (default 30 s).
	Skew time.Duration
}

var (
	// ErrInvalid covers every reason a ticket is not accepted; the endpoint answers all of them alike.
	ErrInvalid = errors.New("ticket is not valid")
	b64        = base64.RawURLEncoding
	header     = b64.EncodeToString([]byte(`{"alg":"HS256","typ":"idp-cmi-file+jwt"}`))
)

// CheckKey refuses a key that is too short to sign with.
func CheckKey(key []byte) error {
	if len(key) < MinKeyBytes {
		return fmt.Errorf("the ticket key must be at least %d bytes", MinKeyBytes)
	}
	return nil
}

func (s *Signer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Sign returns a ticket with c's claims, a fresh jti, the signer's audience, the issuer and the times (c.Kind must be read or
// write). It returns the ticket and its claims as signed.
func (s *Signer) Sign(c Claims) (string, Claims, error) {
	if err := CheckKey(s.Key); err != nil {
		return "", c, err
	}
	if c.Kind != Read && c.Kind != Write {
		return "", c, errors.New("a ticket is for reading or writing")
	}
	if c.Tenant == "" || c.Subject == "" || c.DocTenant == "" || c.Document == "" || c.Job == "" {
		return "", c, errors.New("a ticket names the workload, the tenant, the document and the job")
	}
	ttl := s.TTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	now := s.now()
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", c, err
	}
	c.Issuer, c.Audience, c.IssuedAt, c.Expires, c.ID = Issuer, s.Audience, now.Unix(), now.Add(ttl).Unix(), hex.EncodeToString(id)
	payload, err := json.Marshal(c)
	if err != nil {
		return "", c, err
	}
	signing := header + "." + b64.EncodeToString(payload)
	tok := signing + "." + b64.EncodeToString(mac(s.Key, signing))
	if len(tok) > MaxLen {
		return "", c, errors.New("the ticket is too long")
	}
	return tok, c, nil
}

func mac(key []byte, signing string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(signing))
	return m.Sum(nil)
}

// Verify checks a ticket's shape, algorithm, signature, issuer, audience, times and kind (want is read or write), and returns
// its claims. It does not check single use or what the ticket is for: the endpoint does, against the database.
func (v *Verifier) Verify(tok, want string) (Claims, error) {
	var c Claims
	if len(tok) == 0 || len(tok) > MaxLen || CheckKey(v.Key) != nil {
		return c, ErrInvalid
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return c, ErrInvalid
	}
	// the algorithm is fixed: the header must be exactly ours, whatever it says ("none", RS256, a different type)
	if subtle.ConstantTimeCompare([]byte(parts[0]), []byte(header)) != 1 {
		return c, ErrInvalid
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, mac(v.Key, parts[0]+"."+parts[1])) {
		return c, ErrInvalid
	}
	raw, err := b64.DecodeString(parts[1])
	if err != nil || json.Unmarshal(raw, &c) != nil {
		return Claims{}, ErrInvalid
	}
	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}
	skew := v.Skew
	if skew <= 0 {
		skew = 30 * time.Second
	}
	switch {
	case c.Issuer != Issuer, c.Audience != v.Audience, c.Audience == "":
	case c.Kind != want, want != Read && want != Write:
	case c.Expires <= now.Unix(), c.IssuedAt > now.Add(skew).Unix():
	case c.ID == "", c.DocTenant == "", c.Document == "", c.Job == "", c.Tenant == "", c.Subject == "":
	default:
		return c, nil
	}
	return Claims{}, ErrInvalid
}
