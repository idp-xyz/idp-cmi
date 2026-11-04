package cmi

import (
	"strings"
	"testing"
	"time"
)

func TestChunkAndBigrams(t *testing.T) {
	text := strings.Repeat("alpha beta gamma. ", 40) + "\n\n" + strings.Repeat("delta ", 300) + "\n\nlast paragraph"
	chunks := Chunk(text, 500)
	if len(chunks) < 4 {
		t.Fatalf("%d chunks", len(chunks))
	}
	for _, c := range chunks {
		if len(c) > 520 {
			t.Fatalf("chunk too long: %d", len(c))
		}
	}
	if strings.Join(Chunk("a\n\nb", 100), "|") != "a|b" && strings.Join(Chunk("a\n\nb", 100), "|") != "a\n\nb" {
		t.Fatalf("%q", Chunk("a\n\nb", 100))
	}
	if got := Bigrams("关税税率 HS 0101 編碼"); got != "关税 税税 税率 編碼" {
		t.Fatalf("%q", got)
	}
	if Bigrams("plain ascii") != "" || Bigrams("税") != "税" {
		t.Fatal("bigrams of non-CJK text must be empty; a lone character stands alone")
	}
}

func TestValidationAndSealing(t *testing.T) {
	s := Source{Name: "orders", DisplayName: "Orders", Kind: SourceAgent, Trust: TrustAuthoritative}
	if err := s.Validate(); err == nil {
		t.Fatal("an authoritative agent source was accepted")
	}
	s.Kind = SourceDomainSystem
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	m := MemoryRecord{Scope: Scope{Kind: ScopeSubject, ID: "alice"}, Kind: KindSemantic, Content: "prefers morning pickups", Provenance: Provenance{Source: "orders", Ref: "order:1", ObservedAt: now, RecordedBy: "run_1"},
		Validity: Validity{ValidFrom: now}, Sensitivity: SensitivityInternal}
	m.Normalize()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]func(x *MemoryRecord){
		"no ref":       func(x *MemoryRecord) { x.Provenance.Ref = "" },
		"bad kind":     func(x *MemoryRecord) { x.Kind = "fact" },
		"tenant id":    func(x *MemoryRecord) { x.Scope = Scope{Kind: ScopeTenant, ID: "x"} },
		"until before": func(x *MemoryRecord) { x.Validity.ValidUntil = now.Add(-time.Hour) },
		"bad level":    func(x *MemoryRecord) { x.Sensitivity = "secret" },
		"bad tag":      func(x *MemoryRecord) { x.Tags = []string{"Bad Tag"} },
	} {
		x := m
		bad(&x)
		if err := x.Validate(); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if !Within(SensitivityInternal, SensitivityConfidential) || Within(SensitivityRestricted, SensitivityInternal) {
		t.Fatal("sensitivity order")
	}
	p := &Package{ID: "p1", TenantID: "t", RequestedBy: "alice", Query: "q", Budget: Budget{MaxTokens: 10, MaxItems: 5}, AssembledAt: now.UTC().Truncate(time.Second), ExpiresAt: now.Add(time.Hour).UTC().Truncate(time.Second)}
	env, err := Seal(p)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Open(env)
	if err != nil || back.ID != "p1" || back.SchemaVersion != SchemaVersion {
		t.Fatalf("%v %+v", err, back)
	}
	env.Digest = "sha256:" + strings.Repeat("0", 64)
	if _, err := Open(env); err == nil {
		t.Fatal("tampered digest accepted")
	}
}
