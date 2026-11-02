package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
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

type clientIPKey struct{}

// WithClientIP stores the resolved caller address on the context.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// ClientIP returns the caller address resolved by the ClientIP middleware. It
// falls back to the socket peer and never reads forwarding headers itself.
func ClientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey{}).(string); ok {
		return ip
	}
	return remoteHost(r)
}

// ResolveClientIP finds the caller address. Forwarding headers are attacker
// controlled unless the socket peer is a trusted proxy, so they are read only
// in that case. X-Forwarded-For is walked from the right because each proxy
// appends: the first untrusted hop is the last address a trusted proxy saw,
// and anything further left is whatever the client sent.
func ResolveClientIP(r *http.Request, trusted []netip.Prefix) string {
	remote, ok := parseAddr(remoteHost(r))
	if !ok {
		return remoteHost(r)
	}
	if !isTrusted(remote, trusted) {
		return remote.String()
	}
	forwarded := strings.Join(r.Header.Values("X-Forwarded-For"), ",")
	if strings.TrimSpace(forwarded) == "" {
		if real, ok := parseAddr(r.Header.Get("X-Real-IP")); ok {
			return real.String()
		}
		return remote.String()
	}
	hops := strings.Split(forwarded, ",")
	best := remote
	for i := len(hops) - 1; i >= 0; i-- {
		addr, ok := parseAddr(hops[i])
		if !ok {
			break
		}
		best = addr
		if !isTrusted(addr, trusted) {
			break
		}
	}
	return best.String()
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// parseAddr returns the canonical address, so one client maps to one rate
// limit key however it is written.
func parseAddr(s string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap().WithZone(""), true
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
