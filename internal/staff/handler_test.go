package staff

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// A global role passes the access check for any outlet string, so the handler
// has to catch a malformed ID before it reaches Postgres.
func TestMalformedOutletIDIsNotFound(t *testing.T) {
	h := NewHandler(nil)
	r := chi.NewRouter()
	r.Get("/outlets/{outletID}/staff", h.List)
	r.Post("/outlets/{outletID}/staff", h.Create)
	r.Put("/outlets/{outletID}/staff/{staffID}/roles/{role}", h.GrantRole)
	r.Delete("/outlets/{outletID}/staff/{staffID}/roles/{role}", h.RevokeRole)

	const staffID = "00000000-0000-0000-0000-000000000001"
	tests := []struct{ method, path string }{
		{http.MethodGet, "/outlets/abc/staff"},
		{http.MethodPost, "/outlets/abc/staff"},
		{http.MethodPut, "/outlets/abc/staff/" + staffID + "/roles/cashier"},
		{http.MethodDelete, "/outlets/abc/staff/" + staffID + "/roles/cashier"},
	}
	for _, tt := range tests {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, strings.NewReader(`{}`)))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404", tt.method, tt.path, rec.Code)
		}
	}
}
