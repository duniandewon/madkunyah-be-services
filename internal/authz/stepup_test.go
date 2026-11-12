package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

type fakeSource map[string]*domain.StaffAccess

func (f fakeSource) Access(_ context.Context, id string) (*domain.StaffAccess, error) {
	if a, ok := f[id]; ok {
		return a, nil
	}
	return &domain.StaffAccess{StaffID: id}, nil
}

func at(id, outlet string, active bool, perms ...string) *domain.StaffAccess {
	return &domain.StaffAccess{StaffID: id, Active: active, Outlets: map[string]domain.AccessScope{
		outlet: {Roles: []string{"role"}, Permissions: perms}}}
}

func TestApprover(t *testing.T) {
	const outlet, perm = "outlet-a", domain.PermShiftOpen
	cashier := at("cashier", outlet, true)
	supervisor := at("supervisor", outlet, true, perm)
	owner := &domain.StaffAccess{StaffID: "owner", Active: true,
		Global: domain.AccessScope{Roles: []string{domain.RoleOwner}, Permissions: []string{perm}}}
	src := fakeSource{
		"cashier": cashier, "supervisor": supervisor, "owner": owner,
		"other-cashier": at("other-cashier", outlet, true),
		"deactivated":   at("deactivated", outlet, false, perm),
		"elsewhere":     at("elsewhere", "outlet-b", true, perm),
	}
	for _, tt := range []struct {
		name       string
		acting     *domain.StaffAccess
		approvedBy string
		want       string // the approver, "" for forbidden
	}{
		{"holder acts alone", supervisor, "", "supervisor"},
		{"holder ignores approved_by", supervisor, "cashier", "supervisor"},
		{"global holder", owner, "", "owner"},
		{"cashier without approver", cashier, "", ""},
		{"cashier with supervisor", cashier, "supervisor", "supervisor"},
		{"cashier with global owner", cashier, "owner", "owner"},
		{"cashier approving themselves", cashier, "cashier", ""},
		{"approver lacks the permission", cashier, "other-cashier", ""},
		{"approver deactivated", cashier, "deactivated", ""},
		{"approver at another outlet", cashier, "elsewhere", ""},
		{"approver unknown", cashier, "nobody", ""},
		{"no acting staff", nil, "supervisor", ""},
	} {
		got, err := Approver(context.Background(), src, tt.acting, outlet, perm, tt.approvedBy)
		if tt.want == "" {
			if !errors.Is(err, domain.ErrForbidden) {
				t.Errorf("%s: got %q, %v; want forbidden", tt.name, got, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("%s: got %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}
}
