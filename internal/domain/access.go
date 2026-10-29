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

const (
	PermStaffManage  = "staff.manage"
	PermDeviceEnroll = "device.enroll"
	PermDeviceRevoke = "device.revoke"
	// Catalog permissions are checked globally: the catalog is shared by
	// every outlet, so an outlet-scoped grant does not reach it.
	PermCatalogManage = "catalog.manage"
	PermCostView      = "cost.view"
)

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

// CanGlobal reports whether perm is held through a global role.
func (a *StaffAccess) CanGlobal(perm string) bool {
	return a.Active && slices.Contains(a.Global.Permissions, perm)
}

// HasAnyRole reports whether the staff member holds a role anywhere.
func (a *StaffAccess) HasAnyRole() bool {
	if !a.Active {
		return false
	}
	if len(a.Global.Roles) > 0 {
		return true
	}
	for _, scope := range a.Outlets {
		if len(scope.Roles) > 0 {
			return true
		}
	}
	return false
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
