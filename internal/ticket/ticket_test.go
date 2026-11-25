package ticket

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var key = []byte("0123456789abcdef0123456789abcdef")

func claims(kind string) Claims {
	return Claims{Tenant: "system", Subject: "cmi-ingest", Kind: kind, DocTenant: "t1", Document: "d1", Version: 2, Job: "j1", Key: "kb/t1/d1/2/u", SHA256: "sha256:ab"}
}

func pair(now *time.Time) (*Signer, *Verifier) {
	clock := func() time.Time { return *now }
	return &Signer{Key: key, Audience: "http://cmi-ingest:8553", TTL: 5 * time.Minute, Now: clock}, &Verifier{Key: key, Audience: "http://cmi-ingest:8553", Now: clock}
}

func TestATicketRoundTripsAndHasTheShapeTheSandboxReads(t *testing.T) {
	now := time.Now()
	s, v := pair(&now)
	tok, signed, err := s.Sign(claims(Read))
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Verify(tok, Read)
	if err != nil || got != signed || got.Document != "d1" || got.Version != 2 || got.Key != "kb/t1/d1/2/u" {
		t.Fatalf("%+v %v", got, err)
	}
	// the sandbox's Peek: three parts, a JSON middle with tenant_id, sub and kind
	parts := strings.Split(tok, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var peek struct{ Tenant, Subject, Kind string }
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	peek.Tenant, peek.Subject, peek.Kind = m["tenant_id"].(string), m["sub"].(string), m["kind"].(string)
	if len(parts) != 3 || peek.Tenant != "system" || peek.Subject != "cmi-ingest" || peek.Kind != "read" {
		t.Fatalf("shape %v %+v", parts, peek)
	}
	if t2, _, _ := s.Sign(claims(Read)); t2 == tok {
		t.Fatal("every ticket has its own jti")
	}
}

func TestForgedExpiredWrongKindAudienceAndKeyAreRefused(t *testing.T) {
	now := time.Now()
	s, v := pair(&now)
	tok, _, _ := s.Sign(claims(Read))
	parts := strings.Split(tok, ".")
	forge := func(mod func(map[string]any)) string {
		raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		mod(m)
		p, _ := json.Marshal(m)
		return parts[0] + "." + base64.RawURLEncoding.EncodeToString(p) + "." + parts[2]
	}
	other := &Signer{Key: []byte("ffffffffffffffffffffffffffffffff"), Audience: "http://cmi-ingest:8553", Now: func() time.Time { return now }}
	otherTok, _, _ := other.Sign(claims(Read))
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	cases := map[string]string{
		"a changed document":      forge(func(m map[string]any) { m["doc"] = "d2" }),
		"a changed tenant":        forge(func(m map[string]any) { m["dt"] = "t2" }),
		"another key":             otherTok,
		"alg none":                noneHeader + "." + parts[1] + ".",
		"alg none with signature": noneHeader + "." + parts[1] + "." + parts[2],
		"two parts":               parts[0] + "." + parts[1],
		"empty":                   "",
		"garbage":                 "a.b.c",
		"too long":                tok + strings.Repeat("a", MaxLen),
		"wrong kind":              mustSign(s, claims(Write)),
	}
	for name, bad := range cases {
		if _, err := v.Verify(bad, Read); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := v.Verify(mustSign(s, claims(Write)), Write); err != nil {
		t.Fatalf("a write ticket for write: %v", err)
	}
	wrongAud := &Verifier{Key: key, Audience: "http://elsewhere", Now: v.Now}
	if _, err := wrongAud.Verify(tok, Read); err == nil {
		t.Error("another audience was accepted")
	}
	later := now.Add(5*time.Minute + time.Second)
	expired := &Verifier{Key: key, Audience: v.Audience, Now: func() time.Time { return later }}
	if _, err := expired.Verify(tok, Read); err == nil {
		t.Error("an expired ticket was accepted")
	}
	earlier := &Verifier{Key: key, Audience: v.Audience, Now: func() time.Time { return now.Add(-time.Hour) }}
	if _, err := earlier.Verify(tok, Read); err == nil {
		t.Error("a ticket from the future was accepted")
	}
	if _, err := v.Verify(tok, "delete"); err == nil {
		t.Error("an unknown kind was accepted")
	}
}

func mustSign(s *Signer, c Claims) string {
	tok, _, err := s.Sign(c)
	if err != nil {
		panic(err)
	}
	return tok
}

func TestSigningRefusesAWeakKeyAndAnIncompleteTicket(t *testing.T) {
	now := time.Now()
	if _, _, err := (&Signer{Key: []byte("short"), Audience: "a", Now: func() time.Time { return now }}).Sign(claims(Read)); err == nil {
		t.Fatal("a short key signs")
	}
	s, _ := pair(&now)
	c := claims(Read)
	c.Job = ""
	if _, _, err := s.Sign(c); err == nil {
		t.Fatal("a ticket without a job")
	}
	c = claims("rw")
	if _, _, err := s.Sign(c); err == nil {
		t.Fatal("an unknown kind")
	}
	if _, err := (&Verifier{Key: []byte("short"), Audience: "a"}).Verify("a.b.c", Read); err == nil {
		t.Fatal("a short key verifies")
	}
}
