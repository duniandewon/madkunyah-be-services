package domain

import "testing"

func managerAtA() *StaffAccess {
	return &StaffAccess{
		StaffID: "m", Active: true,
		Outlets: map[string]AccessScope{
			"A": {Roles: []string{RoleManager}, Permissions: []string{PermStaffManage, "report.outlet.view"}},
			"B": {Roles: []string{RoleCashier}, Permissions: []string{"order.create"}},
		},
	}
}

// AC-27 at unit level: manager at A, cashier at B, nothing at C.
func TestCheckOutlet(t *testing.T) {
	a := managerAtA()
	if err := a.CheckOutlet("A", "report.outlet.view"); err != nil {
		t.Errorf("A: %v", err)
	}
	if err := a.CheckOutlet("B", "report.outlet.view"); AsError(err).Code != CodeForbidden {
		t.Errorf("B: got %v, want forbidden", err)
	}
	if err := a.CheckOutlet("C", "report.outlet.view"); AsError(err).Code != CodeNotFound {
		t.Errorf("C: got %v, want not_found", err)
	}
}

func TestGlobalRoleReachesEveryOutlet(t *testing.T) {
	a := &StaffAccess{StaffID: "o", Active: true,
		Global: AccessScope{Roles: []string{RoleOwner}, Permissions: []string{"report.all.view"}}}
	if err := a.CheckOutlet("any-outlet", "report.all.view"); err != nil {
		t.Errorf("global owner: %v", err)
	}
	if !a.IsGlobalOwner() || !a.HasRole("any-outlet", RoleOwner) {
		t.Error("global owner role not recognised")
	}
}

func TestInactiveStaffHasNoAccess(t *testing.T) {
	a := managerAtA()
	a.Active = false
	if a.Visible("A") || a.Can("A", PermStaffManage) || a.HasRole("A", RoleManager) {
		t.Error("inactive staff retained access")
	}
}
