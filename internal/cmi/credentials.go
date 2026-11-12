package cmi

import (
	"regexp"
	"strings"
)

// A connector's headers are stored in the database and shown to administrators, so they may not carry credentials
// (ADR-0001 D45): a credential belongs in a reverse proxy in front of the document store. Names that usually carry one
// (Authorization, Cookie, X-Api-Key, X-Auth-Token, ...) and values in an authorization scheme are refused. The same
// rule is in migrations/0012_thread_acl.sql, which redacted the headers stored before it.
var (
	credentialNameRE  = regexp.MustCompile(`(?i)(auth|token|secret|key|cookie|session|pass|credential|signature|csrf|xsrf|jwt|bearer)`)
	credentialValueRE = regexp.MustCompile(`(?i)^(bearer|basic|digest|token|negotiate)\s`)
)

// CredentialHeader reports whether a connector header ("Name: value") looks like it carries a credential.
func CredentialHeader(h string) bool {
	name, value, _ := strings.Cut(h, ":")
	return credentialNameRE.MatchString(strings.TrimSpace(name)) || credentialValueRE.MatchString(strings.TrimSpace(value))
}
