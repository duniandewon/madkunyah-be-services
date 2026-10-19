// Package staff manages staff members and their role grants.
package staff

import "github.com/duniandewon/madkunyah-be-service/internal/domain"

var (
	errSelfChange = domain.NewError(domain.CodeForbidden, "You cannot change your own access.")
	errEscalation = domain.NewError(domain.CodeForbidden, "You are not allowed to manage this role.")
)

// canAssign enforces FR-AZ-6 for granting or revoking one outlet role:
// staff.manage covers cashier and supervisor; manager and owner need an owner
// at that outlet (directly or globally).
func canAssign(actor *domain.StaffAccess, outletID, role string) error {
	switch role {
	case domain.RoleCashier, domain.RoleSupervisor:
		if actor.Can(outletID, domain.PermStaffManage) {
			return nil
		}
	case domain.RoleManager, domain.RoleOwner:
		if actor.HasRole(outletID, domain.RoleOwner) {
			return nil
		}
	}
	return errEscalation
}

// canResetPIN enforces FR-PIN-5: staff.manage at an outlet where the target
// holds a role. A global owner may reset any PIN, including one belonging to
// a global role holder, who has no outlet role to qualify through.
func canResetPIN(actor, target *domain.StaffAccess) error {
	if actor.IsGlobalOwner() {
		return nil
	}
	visible := false
	for outletID := range target.Outlets {
		if actor.Can(outletID, domain.PermStaffManage) {
			return nil
		}
		visible = visible || actor.Visible(outletID)
	}
	if !visible {
		return domain.ErrNotFound
	}
	return domain.ErrForbidden
}

// canManage decides whether actor may deactivate target or end its sessions.
// A global owner may act on anyone else. Otherwise the target must be visible
// to the actor (else not_found), hold no global role, and every role it holds
// must be one the actor could assign at that outlet.
func canManage(actor, target *domain.StaffAccess) error {
	if actor.StaffID == target.StaffID {
		return errSelfChange
	}
	if actor.IsGlobalOwner() {
		return nil
	}
	visible := false
	for outletID := range target.Outlets {
		if actor.Visible(outletID) {
			visible = true
			break
		}
	}
	if !visible {
		return domain.ErrNotFound
	}
	if len(target.Global.Roles) > 0 {
		return errEscalation
	}
	for outletID, scope := range target.Outlets {
		for _, role := range scope.Roles {
			if err := canAssign(actor, outletID, role); err != nil {
				return err
			}
		}
	}
	return nil
}
