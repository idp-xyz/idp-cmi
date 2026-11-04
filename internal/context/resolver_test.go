package context

import (
	"testing"
	"time"
)

func TestTSQueryAndFreshness(t *testing.T) {
	if q := TSQuery("Pickup window for 关税 order #42"); q != "'pickup' | 'window' | 'for' | '关税' | 'order' | '42'" {
		t.Fatalf("%q", q)
	}
	if q := TSQuery("a O'Brien"); q != "'brien'" { // one-letter words and the apostrophe split are dropped
		t.Fatalf("%q", q)
	}
	now := time.Now()
	if f := freshness(now, now); f != 1 {
		t.Fatal(f)
	}
	if f := freshness(now, now.Add(-30*24*time.Hour)); f < 0.49 || f > 0.51 {
		t.Fatal(f)
	}
	if excerpt("héllo wörld", 6) != "héllo…" {
		t.Fatalf("%q", excerpt("héllo wörld", 6))
	}
}
