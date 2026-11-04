package ingest

import (
	"strings"
	"testing"
)

func TestToText(t *testing.T) {
	out, err := ToText([]byte(`<html><head><title>x</title><style>p{}</style></head><body><h1>Returns</h1><p>Accepted within <b>30</b> days.</p><script>alert(1)</script><ul><li>a &amp; b</li></ul></body></html>`), "text/html")
	if err != nil || !strings.Contains(out, "Returns\nAccepted within 30 days.") || strings.Contains(out, "alert") || !strings.Contains(out, "a & b") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := ToText([]byte("x"), "application/pdf"); err == nil {
		t.Fatal("pdf accepted")
	}
	if _, err := ToText([]byte("{"), "application/json"); err == nil {
		t.Fatal("invalid json accepted")
	}
	if _, err := ToText(make([]byte, MaxFetch+1), "text/plain"); err == nil {
		t.Fatal("oversize accepted")
	}
	if mediaTypeOf("text/html; charset=utf-8", "x") != "text/html" || mediaTypeOf("", "a/b.MD") != "text/markdown" || mediaTypeOf("", "a") != "text/plain" {
		t.Fatal("media type detection")
	}
}
