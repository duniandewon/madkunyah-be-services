package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// NFR-SEC-2: public routes are limited per IP, and fall back to an
// in-process limiter without Redis.
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
	for i, want := range []int{200, 200, 429} {
		if got := call("10.0.0.3").Code; got != want {
			t.Fatalf("limiter down, request %d: got %d, want %d", i+1, got, want)
		}
	}
	if got := call("10.0.0.4").Code; got != 200 {
		t.Errorf("limiter down, other IP: got %d", got)
	}
}

func TestLimiterKeyMasksIPv6(t *testing.T) {
	tests := map[string]string{
		"2001:db8:1:2:aaaa::1": "2001:db8:1:2::/64",
		"2001:db8:1:2:bbbb::9": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":      "2001:db8:1:3::/64",
		"198.51.100.7":         "198.51.100.7",
		"::ffff:198.51.100.7":  "::ffff:198.51.100.7",
		"not-an-ip":            "not-an-ip",
	}
	for in, want := range tests {
		if got := limiterKey(in); got != want {
			t.Errorf("limiterKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLocalLimiter(t *testing.T) {
	now := time.Unix(1_000, 0)
	l := newLocalLimiter(time.Minute)
	for i, want := range []bool{true, true, false} {
		if got := l.allow("a", 2, now) == 0; got != want {
			t.Fatalf("request %d: allowed = %v", i+1, got)
		}
	}
	if wait := l.allow("a", 2, now.Add(15*time.Second)); wait != 45*time.Second {
		t.Errorf("wait = %v, want 45s", wait)
	}
	if l.allow("a", 2, now.Add(time.Minute)) != 0 {
		t.Error("window rollover must reset the count")
	}
}

// Past maxLocalKeys, new clients share one overflow bucket instead of
// growing the map.
func TestLocalLimiterOverflowBucket(t *testing.T) {
	now := time.Unix(1_000, 0)
	l := newLocalLimiter(time.Minute)
	for i := range maxLocalKeys {
		l.allow(strconv.Itoa(i), 1, now)
	}
	if l.allow("new-1", 1, now) != 0 || l.allow("new-2", 1, now) == 0 {
		t.Error("overflow clients must share one budget")
	}
	if len(l.counts) != maxLocalKeys {
		t.Errorf("map grew to %d", len(l.counts))
	}
}
