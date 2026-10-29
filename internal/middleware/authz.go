package middleware

import (
	"context"
	"net/http"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
	"github.com/go-chi/chi/v5"
)

const accessKey ctxKey = "staff_access"

type AccessResolver interface {
	Access(ctx context.Context, staffID string) (*domain.StaffAccess, error)
}

// LoadStaffAccess resolves the authenticated staff member's roles once per
// request. A deactivated staff member holding a still-valid access token is
// refused here, on every staff route.
func LoadStaffAccess(res AccessResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := PrincipalFrom(r.Context())
			if p == nil || p.Audience != domain.AudienceStaff {
				httpx.Error(w, r, errTokenInvalid)
				return
			}
			a, err := res.Access(r.Context(), p.Subject)
			if err != nil {
				httpx.Error(w, r, err)
				return
			}
			if !a.Active {
				httpx.Error(w, r, domain.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accessKey, a)))
		})
	}
}

// RequireOutletPermission checks perm at the outlet named by the {outletID}
// route parameter: 404 if the caller has no role reaching that outlet, 403 if
// they do but lack the permission.
func RequireOutletPermission(perm string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			a := AccessFrom(r.Context())
			if a == nil {
				httpx.Error(w, r, domain.ErrForbidden)
				return
			}
			if err := a.CheckOutlet(chi.URLParam(r, "outletID"), perm); err != nil {
				httpx.Error(w, r, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAnyRole admits staff holding a role at any outlet or globally.
func RequireAnyRole(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := AccessFrom(r.Context()); a == nil || !a.HasAnyRole() {
			httpx.Error(w, r, domain.ErrForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireGlobalPermission checks a permission that must be held through a
// global role, for resources that belong to no single outlet.
func RequireGlobalPermission(perm string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if a := AccessFrom(r.Context()); a == nil || !a.CanGlobal(perm) {
				httpx.Error(w, r, domain.ErrForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AccessFrom returns the access loaded by LoadStaffAccess, or nil.
func AccessFrom(ctx context.Context) *domain.StaffAccess {
	a, _ := ctx.Value(accessKey).(*domain.StaffAccess)
	return a
}
