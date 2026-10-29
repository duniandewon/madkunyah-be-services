package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
)

const maxBodyBytes = 1 << 20

// Decode reads a JSON body with a size cap and rejects unknown fields, so a
// typo in a client payload surfaces as an error instead of being ignored.
func Decode(w http.ResponseWriter, r *http.Request, dst interface{}) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(dst); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		var sizeErr *http.MaxBytesError
		switch {
		case errors.As(err, &syntaxErr):
			return domain.NewError(domain.CodeInvalidInput, "request body is not valid JSON")
		case errors.As(err, &typeErr):
			return domain.ValidationError(map[string]string{
				typeErr.Field: "expected " + typeErr.Type.String(),
			})
		case errors.As(err, &sizeErr):
			return domain.NewError(domain.CodeInvalidInput, "request body is too large")
		case errors.Is(err, io.EOF):
			return domain.NewError(domain.CodeInvalidInput, "request body is empty")
		case strings.Contains(err.Error(), "unknown field"):
			return domain.NewError(domain.CodeInvalidInput, strings.TrimPrefix(err.Error(), "json: "))
		default:
			return domain.NewError(domain.CodeInvalidInput, "could not decode request body")
		}
	}

	// A second decode must hit EOF; two JSON objects in one body is a client bug
	// worth surfacing rather than silently using the first.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return domain.NewError(domain.CodeInvalidInput, "request body must contain a single JSON object")
	}
	return nil
}

// BearerToken returns the token from an "Authorization: Bearer" header, or ""
// if there is none.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

// ClientIP resolves the caller address for rate limiting. X-Forwarded-For is
// trusted only because this service is expected to sit behind a proxy that
// sets it; exposed directly, the header is attacker-controlled.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if rip := r.Header.Get("X-Real-IP"); rip != "" {
		return rip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
