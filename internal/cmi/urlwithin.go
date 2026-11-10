package cmi

import (
	"net/url"
	"path"
	"strings"
)

// URLWithin reports whether raw lies under base, and returns raw in normal form (its path cleaned, so a ".." cannot lead out
// of base once the server resolves it). The scheme (http or https), host and port must be equal, neither may carry
// credentials ("https://docs.example.com@evil.example/" is a request to evil.example), and the path must be base's or lie
// below it at a segment boundary ("/handbook" does not contain "/handbook-private"). A plain string prefix test passes
// "https://docs.example.com.evil.example/..." for the base "https://docs.example.com".
func URLWithin(base, raw string) (string, bool) {
	b, err1 := url.Parse(base)
	u, err2 := url.Parse(raw)
	if err1 != nil || err2 != nil || (b.Scheme != "http" && b.Scheme != "https") || b.Scheme != u.Scheme || b.User != nil || u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return "", false
	}
	if b.Hostname() == "" || !strings.EqualFold(b.Hostname(), u.Hostname()) || portOf(b) != portOf(u) {
		return "", false
	}
	bp, up := path.Clean("/"+b.Path), path.Clean("/"+u.Path)
	if up != bp && !strings.HasPrefix(up, strings.TrimSuffix(bp, "/")+"/") {
		return "", false
	}
	u.Host = strings.ToLower(u.Host)
	u.Path, u.RawPath = up, ""
	return u.String(), true
}

func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}
