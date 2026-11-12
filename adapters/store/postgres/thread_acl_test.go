package postgres

import (
	"testing"

	"idp-cmi/internal/authorization"
)

// The store names the administrator role without importing the role model; the two must not drift (D44).
func TestThreadAdminRoleIsTheAdministratorRole(t *testing.T) {
	if ThreadAdminRole != authorization.RoleAdmin {
		t.Fatalf("ThreadAdminRole %q, administrators are %q", ThreadAdminRole, authorization.RoleAdmin)
	}
}
