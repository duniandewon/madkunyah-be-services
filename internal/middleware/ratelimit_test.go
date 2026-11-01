package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeLimiter struct {
	counts map[string]int
	err    error
}

func (f *fakeLimiter) AllowFixedWindow(_ context.Context, key string, limit int, window time.Duration) (time.Duration, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.counts[key]++
	if f.counts[key] > limit {
		return window, nil
	}
	return 0, nil
}

// NFR-SEC-2: public routes are limited per IP, and fail open without Redis.
func TestRateLimitIP(t *testing.T) {
	l := &fakeLimiter{counts: map[string]int{}}
	h := RateLimitIP(l, "public", 2, time.Minute)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	call := func(ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/outlets", nil)
		req.RemoteAddr = ip + ":5000"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for i, want := range []int{200, 200, 429} {
		if got := call("10.0.0.1").Code; got != want {
			t.Fatalf("request %d: got %d, want %d", i+1, got, want)
		}
	}
	if rec := call("10.0.0.1"); rec.Header().Get("Retry-After") != "60" {
		t.Errorf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	if got := call("10.0.0.2").Code; got != 200 {
		t.Errorf("other IP: got %d", got)
	}
	l.err = errors.New("redis down")
	if got := call("10.0.0.1").Code; got != 200 {
		t.Errorf("limiter down: got %d, want 200", got)
	}
}
