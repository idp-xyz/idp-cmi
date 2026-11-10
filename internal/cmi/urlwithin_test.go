package cmi

import "testing"

func TestURLWithin(t *testing.T) {
	base := "https://docs.example.com/handbook"
	for raw, want := range map[string]bool{
		"https://docs.example.com/handbook":                true,
		"https://docs.example.com/handbook/a/b.md":         true,
		"https://DOCS.example.com:443/handbook/a.md":       true,
		"https://docs.example.com/handbook/./x/../y.md":    true,
		"https://docs.example.com/handbook/../secret":      false, // leaves the base once resolved
		"https://docs.example.com/handbook/%2e%2e/secret":  false,
		"https://docs.example.com/handbook-private/a.md":   false, // not a segment boundary
		"https://docs.example.com.evil.example/handbook/a": false, // a string prefix would accept it
		"https://docs.example.com@evil.example/handbook/a": false, // the host is evil.example
		"https://user:pw@docs.example.com/handbook/a":      false,
		"http://docs.example.com/handbook/a":               false, // another scheme
		"https://docs.example.com:8443/handbook/a":         false, // another port
		"https://docs.example.com/handbook/a#frag":         false,
		"file:///etc/passwd":                               false,
		"https://docs.example.com":                         false,
		"https://other.example.com/handbook/a":             false,
	} {
		got, ok := URLWithin(base, raw)
		if ok != want {
			t.Errorf("%s: within=%v (%q), want %v", raw, ok, got, want)
		}
	}
	if got, _ := URLWithin(base, "https://DOCS.example.com:443/handbook/./x/../y.md"); got != "https://docs.example.com:443/handbook/y.md" {
		t.Errorf("normal form: %q", got)
	}
	if _, ok := URLWithin("https://docs.example.com/", "https://docs.example.com/anything"); !ok {
		t.Error("a base of the whole host contains its paths")
	}
	if _, ok := URLWithin("ftp://x/", "ftp://x/a"); ok {
		t.Error("only http and https")
	}
}
