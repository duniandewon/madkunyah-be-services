package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/go-chi/chi/v5"
)

func serveWith(mw func(http.Handler) http.Handler, a *domain.StaffAccess) int {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if a != nil {
		req = req.WithContext(context.WithValue(req.Context(), accessKey, a))
	}
	rec := httptest.NewRecorder()
	mw(ok).ServeHTTP(rec, req)
	return rec.Code
}

// AC-10: catalog writes need catalog.manage through a global role; reads
// need any role anywhere.
func TestCatalogAccess(t *testing.T) {
	outlet := func(role string, perms ...string) *domain.StaffAccess {
		return &domain.StaffAccess{Active: true, Outlets: map[string]domain.AccessScope{
			"o1": {Roles: []string{role}, Permissions: perms}}}
	}
	globalOwner := &domain.StaffAccess{Active: true, Global: domain.AccessScope{
		Roles: []string{domain.RoleOwner}, Permissions: []string{domain.PermCatalogManage, domain.PermCostView}}}
	tests := []struct {
		name        string
		access      *domain.StaffAccess
		read, write int
	}{
		{"global owner", globalOwner, 200, 200},
		{"outlet owner", outlet(domain.RoleOwner, domain.PermCatalogManage), 200, 403},
		{"manager", outlet(domain.RoleManager, domain.PermStaffManage), 200, 403},
		{"cashier", outlet(domain.RoleCashier, "order.create"), 200, 403},
		{"no role", &domain.StaffAccess{Active: true}, 403, 403},
		{"deactivated owner", &domain.StaffAccess{Global: globalOwner.Global}, 403, 403},
		{"no access loaded", nil, 403, 403},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serveWith(RequireAnyRole, tt.access); got != tt.read {
				t.Errorf("read = %d, want %d", got, tt.read)
			}
			if got := serveWith(RequireGlobalPermission(domain.PermCatalogManage), tt.access); got != tt.write {
				t.Errorf("write = %d, want %d", got, tt.write)
			}
		})
	}
}

// AC-3, E-4, FR-AZ-5: outlet catalog routes are 404 to staff with no role
// reaching the outlet; global role holders reach every outlet.
func TestRequireOutletRole(t *testing.T) {
	serve := func(a *domain.StaffAccess, outlet string) int {
		r := chi.NewRouter()
		r.With(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if a != nil {
					req = req.WithContext(context.WithValue(req.Context(), accessKey, a))
				}
				next.ServeHTTP(w, req)
			})
		}, RequireOutletRole).Get("/outlets/{outletID}/catalog", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/outlets/"+outlet+"/catalog", nil))
		return rec.Code
	}
	managerA := &domain.StaffAccess{Active: true, Outlets: map[string]domain.AccessScope{
		"a": {Roles: []string{domain.RoleManager}, Permissions: []string{domain.PermAvailabilityManage}}}}
	owner := &domain.StaffAccess{Active: true, Global: domain.AccessScope{Roles: []string{domain.RoleOwner}}}
	tests := []struct {
		name   string
		access *domain.StaffAccess
		outlet string
		want   int
	}{
		{"manager at own outlet", managerA, "a", 200},
		{"manager at other outlet", managerA, "b", 404},
		{"global owner anywhere", owner, "b", 200},
		{"deactivated", &domain.StaffAccess{Outlets: managerA.Outlets}, "a", 404},
		{"no access loaded", nil, "a", 403},
	}
	for _, tt := range tests {
		if got := serve(tt.access, tt.outlet); got != tt.want {
			t.Errorf("%s: got %d, want %d", tt.name, got, tt.want)
		}
	}
}
