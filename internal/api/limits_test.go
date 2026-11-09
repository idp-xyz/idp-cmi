package api

import (
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
