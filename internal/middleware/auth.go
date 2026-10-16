package middleware

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
)

const principalKey ctxKey = "principal"

type TokenVerifier interface {
	Verify(raw string, allowed ...domain.Audience) (*domain.Principal, error)
}

type TokenDenylist interface {
	IsTokenDenied(ctx context.Context, jti string) (bool, error)
}

var errTokenInvalid = domain.NewError(domain.CodeTokenInvalid, "Token is invalid.")

// Authenticate requires a bearer access token for one of the allowed
// audiences. A token for any other audience is token_invalid (401), never
// 403. Route groups pass exactly one audience.
func Authenticate(v TokenVerifier, deny TokenDenylist, allowed ...domain.Audience) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := httpx.BearerToken(r)
			if raw == "" {
				httpx.Error(w, r, errTokenInvalid)
				return
			}
			p, err := v.Verify(raw, allowed...)
			if err != nil {
				httpx.Error(w, r, err)
				return
			}
			if isDenied(r.Context(), deny, p.TokenID) {
				httpx.Error(w, r, errTokenInvalid)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
		})
	}
}

// isDenied fails open when Redis is unreachable: the exposure is bounded by
// the 15-minute access-token lifetime, and failing closed would log out every
// client during an outage.
func isDenied(ctx context.Context, deny TokenDenylist, jti string) bool {
	denied, err := deny.IsTokenDenied(ctx, jti)
	if err != nil {
		slog.ErrorContext(ctx, "token denylist unavailable, failing open", "error", err)
		return false
	}
	return denied
}

// PrincipalFrom returns the authenticated caller, or nil outside an
// Authenticate-protected route.
func PrincipalFrom(ctx context.Context) *domain.Principal {
	p, _ := ctx.Value(principalKey).(*domain.Principal)
	return p
}
