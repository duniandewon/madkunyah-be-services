package authz

import (
	"context"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

// AccessSource resolves a staff member's access; Resolver implements it.
type AccessSource interface {
	Access(ctx context.Context, staffID string) (*domain.StaffAccess, error)
}

// Approver decides who approves a step-up action needing perm at the
// outlet. It returns the acting staff member when they hold the permission
// themselves, in which case approvedBy is ignored (FR-STEP-5). Otherwise
// approvedBy must name an active staff member who holds it there, checked
// against the server's own view rather than the device's roster. Anything
// else is forbidden.
func Approver(ctx context.Context, src AccessSource, acting *domain.StaffAccess,
	outletID, perm, approvedBy string) (string, error) {
	if acting == nil {
		return "", domain.ErrForbidden
	}
	if acting.Can(outletID, perm) {
		return acting.StaffID, nil
	}
	if approvedBy == "" {
		return "", domain.ErrForbidden
	}
	approver, err := src.Access(ctx, approvedBy)
	if err != nil {
		return "", err
	}
	if !approver.Can(outletID, perm) {
		return "", domain.ErrForbidden
	}
	return approver.StaffID, nil
}
