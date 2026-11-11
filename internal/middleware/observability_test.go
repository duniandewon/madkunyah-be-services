package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDKeepsOnlyPlainIDs(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	tests := []struct {
		name string
		in   string
		keep bool
	}{
		{"uuid", "3f2a8c1e-9b7d-4e2a-8c1f-0a1b2c3d4e5f", true},
		{"plain token", "pos_abc.123", true},
		{"empty", "", false},
		{"too long", strings.Repeat("a", maxRequestIDLength+1), false},
		{"space", "abc def", false},
		{"control byte", "abc\x01", false},
		{"quote", `abc"`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("X-Request-ID", tt.in)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			got := rec.Header().Get("X-Request-ID")
			if tt.keep && got != tt.in {
				t.Errorf("got %q, want %q kept", got, tt.in)
			}
			if !tt.keep && (got == tt.in || len(got) != 36) {
				t.Errorf("got %q, want a fresh UUID", got)
			}
		})
	}
}

func TestRecoverWritesInternalError(t *testing.T) {
	h := Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestRecoverReraisesAbortHandler(t *testing.T) {
	h := Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	defer func() {
		if rec := recover(); rec != http.ErrAbortHandler { //nolint:errorlint // panic values are compared, not wrapped
			t.Errorf("recovered %v, want http.ErrAbortHandler re-raised", rec)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

// A panicking handler still gets CORS headers on its 500, because Recover
// sits inside CORS.
func TestPanicResponseCarriesCORS(t *testing.T) {
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := Logging(CORS([]string{"https://shop.example"})(Recover(panicky)))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://shop.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Access-Control-Allow-Origin") != "https://shop.example" {
		t.Errorf("status = %d, allow-origin = %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
}
