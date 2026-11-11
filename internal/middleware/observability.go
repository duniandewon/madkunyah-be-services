package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
	"github.com/google/uuid"
)

type ctxKey string

const requestIDKey ctxKey = "request_id"

// maxRequestIDLength bounds a client-supplied X-Request-ID; a UUID is 36.
const maxRequestIDLength = 64

// RequestID assigns each request an ID and echoes it back, so a customer
// reporting an error can be traced to exact log lines. A client-supplied ID
// is kept only when it is short and plain; otherwise a new one is issued, so
// arbitrary header bytes never reach the logs or the response.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !validRequestID(id) {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !requestIDByte(id[i]) {
			return false
		}
	}
	return true
}

func requestIDByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	default:
		return c == '-' || c == '_' || c == '.'
	}
}

func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Logging emits one structured line per request. Request bodies are never
// logged: they carry OTP codes, refresh tokens and customer phone numbers.
func Logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(sw, r)

		level := slog.LevelInfo
		if sw.status >= 500 {
			level = slog.LevelError
		} else if sw.status >= 400 {
			level = slog.LevelWarn
		}

		slog.Log(r.Context(), level, "http request",
			"request_id", RequestIDFromContext(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"bytes", sw.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", httpx.ClientIP(r),
		)
	})
}

// Recover turns a panic into a 500 rather than dropping the connection, and
// keeps the rest of the server alive. Mount it inside Logging and CORS, so a
// panicking request is still logged and its 500 still carries CORS headers.
// http.ErrAbortHandler is re-raised: it is net/http's deliberate way to abort
// a response, not a bug.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler { //nolint:errorlint // panic values are compared, not wrapped
					panic(rec)
				}
				slog.ErrorContext(r.Context(), "panic recovered",
					"request_id", RequestIDFromContext(r.Context()),
					"panic", rec,
					"stack", string(debug.Stack()),
					"path", r.URL.Path)
				httpx.Error(w, r, domain.NewError(domain.CodeInternal, "an unexpected error occurred"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// Timeout bounds handler execution. The POS sync endpoint sets its own longer
// budget; everything else uses the default.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
