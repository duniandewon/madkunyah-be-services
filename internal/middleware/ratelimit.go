package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
)

// limiterTimeout bounds the limiter's Redis call, so an outage does not
// slow every public request down by the client's retries.
const limiterTimeout = 150 * time.Millisecond

type WindowLimiter interface {
	AllowFixedWindow(ctx context.Context, key string, limit int, window time.Duration) (time.Duration, error)
}

// RateLimitIP allows limit requests per client IP per window on the routes
// it wraps, counted under name. It fails open when Redis is unreachable:
// these are public reads, and refusing them would take the shop down with
// the cache.
func RateLimitIP(l WindowLimiter, name string, limit int, window time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), limiterTimeout)
			wait, err := l.AllowFixedWindow(ctx, name+":"+httpx.ClientIP(r), limit, window)
			cancel()
			if err != nil {
				slog.ErrorContext(r.Context(), "rate limiter unavailable, failing open", "error", err, "limiter", name)
			} else if wait > 0 {
				httpx.Error(w, r, domain.RateLimitError(domain.CodeRateLimited, "Too many requests. Try again shortly.", wait))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
