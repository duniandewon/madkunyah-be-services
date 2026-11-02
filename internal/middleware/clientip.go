package middleware

import (
	"net/http"
	"net/netip"

	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
)

// ClientIP resolves the caller address once per request, so rate limits, audit
// events and logs all see the same value. Mount it before anything that reads
// httpx.ClientIP.
func ClientIP(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := httpx.WithClientIP(r.Context(), httpx.ResolveClientIP(r, trusted))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
