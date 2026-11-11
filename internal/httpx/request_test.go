package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

// E-11: prices must be JSON integers; strings and floats are rejected on
// their field.
func TestDecodeRejectsNonIntegerPrices(t *testing.T) {
	var dst struct {
		BasePrice int64 `json:"base_price"`
	}
	for _, body := range []string{`{"base_price":"22000"}`, `{"base_price":22000.5}`, `{"base_price":2e4}`} {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		err := Decode(httptest.NewRecorder(), req, &dst)
		de := domain.AsError(err)
		if de == nil || de.Code != domain.CodeInvalidInput || de.Fields["base_price"] != "expected an integer" {
			t.Errorf("%s: err = %v", body, err)
		}
	}
}

func TestDecodeBodyTooLarge(t *testing.T) {
	var dst map[string]string
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"a":"`+strings.Repeat("x", 100)+`"}`))
	rec := httptest.NewRecorder()
	req.Body = http.MaxBytesReader(rec, req.Body, 50)
	err := Decode(rec, req, &dst)
	if de := domain.AsError(err); de == nil || de.Code != domain.CodePayloadTooLarge {
		t.Errorf("err = %v", err)
	}
}

func TestResolveClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	tests := []struct {
		name    string
		remote  string
		xff     []string
		realIP  string
		trusted []netip.Prefix
		want    string
	}{
		{name: "no proxies configured ignores headers", remote: "203.0.113.9:1", xff: []string{"1.1.1.1"}, realIP: "2.2.2.2", want: "203.0.113.9"},
		{name: "untrusted peer ignores headers", remote: "203.0.113.9:1", xff: []string{"1.1.1.1"}, realIP: "2.2.2.2", trusted: trusted, want: "203.0.113.9"},
		{name: "trusted peer, one hop", remote: "10.0.0.1:1", xff: []string{"198.51.100.7"}, trusted: trusted, want: "198.51.100.7"},
		{name: "spoofed left entry is skipped", remote: "10.0.0.1:1", xff: []string{"1.1.1.1, 198.51.100.7, 10.0.0.2"}, trusted: trusted, want: "198.51.100.7"},
		{name: "repeated header lines are combined", remote: "10.0.0.1:1", xff: []string{"1.1.1.1", "198.51.100.7"}, trusted: trusted, want: "198.51.100.7"},
		{name: "all hops trusted returns the left-most", remote: "10.0.0.1:1", xff: []string{"10.0.0.3, 10.0.0.2"}, trusted: trusted, want: "10.0.0.3"},
		{name: "invalid hop stops the walk", remote: "10.0.0.1:1", xff: []string{"1.1.1.1, garbage, 10.0.0.2"}, trusted: trusted, want: "10.0.0.2"},
		{name: "only an invalid hop keeps the peer", remote: "10.0.0.1:1", xff: []string{"garbage"}, trusted: trusted, want: "10.0.0.1"},
		{name: "X-Real-IP from a trusted peer", remote: "10.0.0.1:1", realIP: "198.51.100.7", trusted: trusted, want: "198.51.100.7"},
		{name: "invalid X-Real-IP keeps the peer", remote: "10.0.0.1:1", realIP: "garbage", trusted: trusted, want: "10.0.0.1"},
		{name: "IPv4-mapped address is canonical", remote: "[::ffff:10.0.0.1]:1", xff: []string{"::ffff:198.51.100.7"}, trusted: trusted, want: "198.51.100.7"},
		{name: "unparsable peer is returned as is", remote: "@", want: "@"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = tt.remote
		for _, v := range tt.xff {
			req.Header.Add("X-Forwarded-For", v)
		}
		if tt.realIP != "" {
			req.Header.Set("X-Real-IP", tt.realIP)
		}
		if got := ResolveClientIP(req, tt.trusted); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestClientIPNeverReadsHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:1"
	req.Header.Set("X-Forwarded-For", "1.1.1.1")
	if got := ClientIP(req); got != "203.0.113.9" {
		t.Errorf("without middleware: got %q", got)
	}
	req = req.WithContext(WithClientIP(req.Context(), "198.51.100.7"))
	if got := ClientIP(req); got != "198.51.100.7" {
		t.Errorf("with resolved value: got %q", got)
	}
}
