package api

import (
	"strings"
	"testing"
	"time"
)

func TestRateLimiterWindow(t *testing.T) {
	var l rateLimiter
	t0 := time.Unix(1000, 0)
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("a", 3, t0); !ok {
			t.Fatalf("write %d refused", i)
		}
	}
	ok, retry := l.allow("a", 3, t0.Add(20*time.Second))
	if ok || retry != 40 {
		t.Fatalf("4th write: ok=%v retry=%d", ok, retry)
	}
	if ok, _ := l.allow("b", 3, t0); !ok {
		t.Fatal("another principal has its own window")
	}
	if ok, _ := l.allow("a", 3, t0.Add(time.Minute)); !ok {
		t.Fatal("a new window starts after a minute")
	}
	if ok, _ := l.allow("a", 0, t0); !ok {
		t.Fatal("0 means unlimited")
	}
}

func TestUploadGateBoundsConcurrentUploadsPerCaller(t *testing.T) {
	var g uploadGate
	r1, ok1 := g.acquire("t/bob", 2)
	r2, ok2 := g.acquire("t/bob", 2)
	if !ok1 || !ok2 {
		t.Fatal("the first two are admitted")
	}
	if _, ok := g.acquire("t/bob", 2); ok {
		t.Fatal("a third concurrent upload by one caller is admitted")
	}
	if r, ok := g.acquire("t/carol", 2); !ok {
		t.Fatal("another caller is held back by bob's uploads")
	} else {
		r()
	}
	r1()
	r3, ok := g.acquire("t/bob", 2)
	if !ok {
		t.Fatal("a place is not given back")
	}
	r2()
	r3()
	if len(g.in) != 0 {
		t.Fatalf("places leak: %v", g.in)
	}
}

func TestValidateUploadBudget(t *testing.T) {
	if ValidateUploadBudget(0) != nil || ValidateUploadBudget(256<<20) != nil || ValidateUploadBudget(50<<20) != nil {
		t.Fatal("a sufficient budget is refused")
	}
	if err := ValidateUploadBudget(30 << 20); err == nil || !strings.Contains(err.Error(), "maxInFlightUploadBytes") {
		t.Fatalf("a budget smaller than the largest original: %v", err)
	}
}
