package domain

import "slices"

// Fixed v1 role codes. Escalation rules (who may grant what) are phrased in
// roles, so they are checked by code rather than by permission.
const (
	RoleCashier    = "cashier"
	RoleSupervisor = "supervisor"
	RoleManager    = "manager"
	RoleOwner      = "owner"
)

const PermStaffManage = "staff.manage"

// AccessScope is the set of roles, and the permissions they carry, that a
// staff member holds in one scope: one outlet, or globally.
type AccessScope struct {
	Roles       []string `json:"roles"`
	Permissions []string `json:"perms"`
}

// StaffAccess is a staff member's resolved authorization state. Global roles
// apply at every outlet; outlet roles grant nothing elsewhere.
type StaffAccess struct {
	StaffID string                 `json:"id"`
	Active  bool                   `json:"active"`
	Global  AccessScope            `json:"global"`
	Outlets map[string]AccessScope `json:"outlets"`
}

// Visible reports whether the staff member holds any role that reaches the
// outlet. Resources at outlets they cannot see are reported as not found.
func (a *StaffAccess) Visible(outletID string) bool {
	if !a.Active {
		return false
	}
	return len(a.Global.Roles) > 0 || len(a.Outlets[outletID].Roles) > 0
}

func (a *StaffAccess) Can(outletID, perm string) bool {
	if !a.Active {
		return false
	}
	return slices.Contains(a.Global.Permissions, perm) ||
		slices.Contains(a.Outlets[outletID].Permissions, perm)
}

// HasRole reports whether the role is held at the outlet, directly or
// globally.
func (a *StaffAccess) HasRole(outletID, role string) bool {
	if !a.Active {
		return false
	}
	return slices.Contains(a.Global.Roles, role) ||
		slices.Contains(a.Outlets[outletID].Roles, role)
}

func (a *StaffAccess) IsGlobalOwner() bool {
	return a.Active && slices.Contains(a.Global.Roles, RoleOwner)
}

// CheckOutlet enforces an outlet-scoped permission: not_found when the outlet
// is outside the caller's reach (so its resources are not disclosed),
// forbidden when it is visible but the permission is missing.
func (a *StaffAccess) CheckOutlet(outletID, perm string) error {
	if !a.Visible(outletID) {
		return ErrNotFound
	}
	if !a.Can(outletID, perm) {
		return ErrForbidden
	}
	return nil
}
