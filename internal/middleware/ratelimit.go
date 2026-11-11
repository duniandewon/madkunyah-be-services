package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
)

// limiterTimeout bounds the limiter's Redis call, so an outage does not
// slow every public request down by the client's retries.
const limiterTimeout = 150 * time.Millisecond

// maxLocalKeys bounds the fallback limiter's memory. Clients beyond it
// share one overflow bucket until the window rolls over.
const maxLocalKeys = 50_000

type WindowLimiter interface {
	AllowFixedWindow(ctx context.Context, key string, limit int, window time.Duration) (time.Duration, error)
}

// RateLimitIP allows limit requests per client per window on the routes it
// wraps, counted under name. IPv6 clients are counted per /64, since one
// subscriber usually holds the whole prefix.
//
// When Redis is unreachable it falls back to an in-process limiter with the
// same budget, counted per instance. Failing open would leave these
// unauthenticated reads, which miss the cache at the same time, free to
// exhaust the Postgres pool; refusing them would take the shop down with
// Redis.
func RateLimitIP(l WindowLimiter, name string, limit int, window time.Duration) func(http.Handler) http.Handler {
	local := newLocalLimiter(window)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := limiterKey(httpx.ClientIP(r))
			ctx, cancel := context.WithTimeout(r.Context(), limiterTimeout)
			wait, err := l.AllowFixedWindow(ctx, name+":"+key, limit, window)
			cancel()
			if err != nil {
				slog.WarnContext(r.Context(), "rate limiter unavailable, using in-process fallback",
					"error", err, "limiter", name)
				wait = local.allow(key, limit, time.Now())
			}
			if wait > 0 {
				httpx.Error(w, r, domain.RateLimitError(domain.CodeRateLimited, "Too many requests. Try again shortly.", wait))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// limiterKey masks IPv6 addresses to their /64. Anything that does not parse
// is used as is.
func limiterKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is6() || addr.Is4In6() {
		return ip
	}
	return netip.PrefixFrom(addr, 64).Masked().String()
}

// localLimiter is a fixed-window counter shared by every key: when the
// window rolls over, all counts reset at once, so memory holds only the
// clients seen in the current window.
type localLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	start   time.Time
	counts  map[string]int
	overrun int
}

func newLocalLimiter(window time.Duration) *localLimiter {
	return &localLimiter{window: window, counts: make(map[string]int)}
}

// allow returns how long the caller must wait, or 0 when the request may
// proceed.
func (l *localLimiter) allow(key string, limit int, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.start) >= l.window {
		l.start = now
		clear(l.counts)
		l.overrun = 0
	}
	var count int
	if c, ok := l.counts[key]; ok || len(l.counts) < maxLocalKeys {
		count = c + 1
		l.counts[key] = count
	} else {
		l.overrun++
		count = l.overrun
	}
	if count > limit {
		return l.start.Add(l.window).Sub(now)
	}
	return 0
}
