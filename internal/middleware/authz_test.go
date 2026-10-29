package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
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
