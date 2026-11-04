// Package authorization holds the authenticated caller and the CMI's role model. Decisions are
// made on tenant, subject and roles only; everything else on the principal is audit information.
package authorization

import "time"

// Roles (ADR-0001 D7). An administrator registers sources and documents and sets the policy; a
// writer records and revokes memory; a reader resolves context packages.
const (
	RoleAdmin  = "cmi-admin"
	RoleWriter = "cmi-writer"
	RoleReader = "cmi-reader"
)

// Principal is an authenticated caller.
type Principal struct {
	Subject string
	Tenant  string
	Roles   []string
	Method  string    // "jwt" or "dev"
	Expires time.Time // credential expiry
	// Client is the OAuth client the token was issued to (azp); Actor the party acting for the
	// subject in a delegated token (RFC 8693 act claim). Audit only.
	Client string
	Actor  string
}

// Has reports whether the principal holds role.
func (p Principal) Has(role string) bool {
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}
