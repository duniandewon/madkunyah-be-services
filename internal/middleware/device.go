package middleware

import (
	"context"
	"net/http"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
	"github.com/google/uuid"
)

const actingStaffKey ctxKey = "acting_staff"

const actingStaffHeader = "X-Acting-Staff-Id"

type DeviceChecker interface {
	DeviceActive(ctx context.Context, deviceID, outletID string) (bool, error)
}

// RequireActiveDevice refuses a pos token whose device has been revoked or
// moved, even though the token itself has not expired yet (FR-DEV-5).
func RequireActiveDevice(dc DeviceChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := PrincipalFrom(r.Context())
			if p == nil || p.Audience != domain.AudiencePOS || p.DeviceID == "" || p.OutletID == "" {
				httpx.Error(w, r, errTokenInvalid)
				return
			}
			active, err := dc.DeviceActive(r.Context(), p.DeviceID, p.OutletID)
			if err != nil {
				httpx.Error(w, r, err)
				return
			}
			if !active {
				httpx.Error(w, r, domain.ErrDeviceRevoked)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireActingStaff resolves the staff member operating the device from
// X-Acting-Staff-Id (FR-DEV-7). They must be active and hold a role reaching
// the device's outlet. Mount it after RequireActiveDevice on POS routes that
// act on someone's behalf.
func RequireActingStaff(res AccessResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(actingStaffHeader)
			if _, err := uuid.Parse(id); err != nil {
				httpx.Error(w, r, domain.NewError(domain.CodeInvalidInput,
					actingStaffHeader+" header must be a staff ID"))
				return
			}
			a, err := res.Access(r.Context(), id)
			if err != nil {
				httpx.Error(w, r, err)
				return
			}
			if !a.Visible(PrincipalFrom(r.Context()).OutletID) {
				httpx.Error(w, r, domain.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actingStaffKey, a)))
		})
	}
}

// ActingStaffFrom returns the staff member resolved by RequireActingStaff,
// or nil.
func ActingStaffFrom(ctx context.Context) *domain.StaffAccess {
	a, _ := ctx.Value(actingStaffKey).(*domain.StaffAccess)
	return a
}
