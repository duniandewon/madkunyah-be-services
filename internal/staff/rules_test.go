package staff

import (
	"testing"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

func access(id string, global []string, outlets map[string][]string) *domain.StaffAccess {
	perms := map[string][]string{
		domain.RoleCashier:    {"order.create"},
		domain.RoleSupervisor: {"order.create", "order.void.paid"},
		domain.RoleManager:    {"order.create", domain.PermStaffManage},
		domain.RoleOwner:      {"order.create", domain.PermStaffManage, "catalog.manage"},
	}
	scope := func(roles []string) domain.AccessScope {
		s := domain.AccessScope{Roles: roles}
		for _, r := range roles {
			s.Permissions = append(s.Permissions, perms[r]...)
		}
		return s
	}
	a := &domain.StaffAccess{StaffID: id, Active: true, Global: scope(global), Outlets: map[string]domain.AccessScope{}}
	for outlet, roles := range outlets {
		a.Outlets[outlet] = scope(roles)
	}
	return a
}

func code(err error) domain.ErrorCode {
	if err == nil {
		return ""
	}
	return domain.AsError(err).Code
}

// AC-29: managers cannot grant manager or owner.
func TestCanAssign(t *testing.T) {
	manager := access("m", nil, map[string][]string{"A": {domain.RoleManager}})
	outletOwner := access("oo", nil, map[string][]string{"A": {domain.RoleOwner}})
	globalOwner := access("go", []string{domain.RoleOwner}, nil)

	tests := []struct {
		name   string
		actor  *domain.StaffAccess
		outlet string
		role   string
		want   domain.ErrorCode
	}{
		{"manager grants cashier at own outlet", manager, "A", domain.RoleCashier, ""},
		{"manager grants supervisor at own outlet", manager, "A", domain.RoleSupervisor, ""},
		{"manager grants manager", manager, "A", domain.RoleManager, domain.CodeForbidden},
		{"manager grants owner", manager, "A", domain.RoleOwner, domain.CodeForbidden},
		{"manager grants cashier elsewhere", manager, "B", domain.RoleCashier, domain.CodeForbidden},
		{"outlet owner grants manager at own outlet", outletOwner, "A", domain.RoleManager, ""},
		{"outlet owner grants manager elsewhere", outletOwner, "B", domain.RoleManager, domain.CodeForbidden},
		{"global owner grants owner anywhere", globalOwner, "Z", domain.RoleOwner, ""},
		{"unknown role", globalOwner, "A", "admin", domain.CodeForbidden},
	}
	for _, tt := range tests {
		if got := code(canAssign(tt.actor, tt.outlet, tt.role)); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestCanManage(t *testing.T) {
	manager := access("m", nil, map[string][]string{"A": {domain.RoleManager}})
	globalOwner := access("go", []string{domain.RoleOwner}, nil)

	tests := []struct {
		name   string
		actor  *domain.StaffAccess
		target *domain.StaffAccess
		want   domain.ErrorCode
	}{
		{"self", manager, manager, domain.CodeForbidden},
		{"cashier at managed outlet", manager, access("c", nil, map[string][]string{"A": {domain.RoleCashier}}), ""},
		{"fellow manager", manager, access("m2", nil, map[string][]string{"A": {domain.RoleManager}}), domain.CodeForbidden},
		{"cashier at A and B", manager, access("c2", nil, map[string][]string{"A": {domain.RoleCashier}, "B": {domain.RoleCashier}}), domain.CodeForbidden},
		{"staff at an unseen outlet", manager, access("c3", nil, map[string][]string{"B": {domain.RoleCashier}}), domain.CodeNotFound},
		{"staff with no roles", manager, access("n", nil, nil), domain.CodeNotFound},
		{"global owner target", manager, access("o", []string{domain.RoleOwner}, map[string][]string{"A": {domain.RoleCashier}}), domain.CodeForbidden},
		{"global owner acts on manager", globalOwner, access("m3", nil, map[string][]string{"A": {domain.RoleManager}}), ""},
		{"global owner acts on self", globalOwner, globalOwner, domain.CodeForbidden},
	}
	for _, tt := range tests {
		if got := code(canManage(tt.actor, tt.target)); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}
