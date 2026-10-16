package auth

import (
	"math"
	"net/http"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
	"github.com/duniandewon/madkunyah-be-service/internal/middleware"
)

// Browser clients (shop, dashboard) get the refresh token in an HttpOnly
// cookie scoped to the auth endpoints; one cookie per audience so a person
// signed in to both does not overwrite one session with the other.
const refreshCookiePath = "/v1/auth"

var refreshCookieNames = map[domain.Audience]string{
	domain.AudienceCustomer: "mk_refresh_customer",
	domain.AudienceStaff:    "mk_refresh_staff",
}

type Handler struct {
	svc    *Service
	signer *Signer
}

func NewHandler(svc *Service, signer *Signer) *Handler {
	return &Handler{svc: svc, signer: signer}
}

type otpRequestBody struct {
	Identifier string `json:"identifier" validate:"required"`
	Purpose    string `json:"purpose" validate:"required,oneof=customer_login staff_login"`
}

type otpVerifyBody struct {
	Identifier string `json:"identifier" validate:"required"`
	Purpose    string `json:"purpose" validate:"required,oneof=customer_login staff_login"`
	Code       string `json:"code" validate:"required,numeric,max=10"`
}

// refreshBody carries the token for non-browser clients. Browser clients
// send only the audience, which picks the cookie to read.
type refreshBody struct {
	RefreshToken string `json:"refresh_token"`
	Audience     string `json:"audience" validate:"omitempty,oneof=customer staff pos"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

type otpAcceptedResponse struct {
	Status      string `json:"status"`
	ExpiresIn   int    `json:"expires_in"`
	ResendAfter int    `json:"resend_after"`
}

// RequestOTP responds identically for every well-formed, non-rate-limited
// request, whether or not a code was sent.
func (h *Handler) RequestOTP(w http.ResponseWriter, r *http.Request) {
	var body otpRequestBody
	if err := httpx.DecodeValid(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	in := OTPRequest{Identifier: body.Identifier, Purpose: Purpose(body.Purpose)}
	if err := h.svc.RequestOTP(r.Context(), in, meta(r)); err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusAccepted, otpAcceptedResponse{
		Status:      "sent_if_eligible",
		ExpiresIn:   seconds(h.svc.cfg.OTPTTL),
		ResendAfter: seconds(h.svc.cfg.Cooldown),
	})
}

func (h *Handler) VerifyOTP(w http.ResponseWriter, r *http.Request) {
	var body otpVerifyBody
	if err := httpx.DecodeValid(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	in := OTPVerify{Identifier: body.Identifier, Purpose: Purpose(body.Purpose), Code: body.Code}
	pair, err := h.svc.VerifyOTP(r.Context(), in, meta(r))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	h.writeTokens(w, pair)
}

func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var body refreshBody
	if err := httpx.DecodeValid(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	aud := domain.Audience(body.Audience)
	pair, err := h.svc.Refresh(r.Context(), refreshTokenFrom(r, &body), aud, meta(r))
	if err != nil {
		clearRefreshCookie(w, aud)
		httpx.Error(w, r, err)
		return
	}
	h.writeTokens(w, pair)
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	var body refreshBody
	if err := httpx.DecodeValid(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	err := h.svc.Logout(r.Context(), refreshTokenFrom(r, &body), httpx.BearerToken(r), meta(r))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	clearRefreshCookie(w, domain.Audience(body.Audience))
	httpx.NoContent(w)
}

func (h *Handler) RevokeAll(w http.ResponseWriter, r *http.Request) {
	p := middleware.PrincipalFrom(r.Context())
	if err := h.svc.RevokeAll(r.Context(), p, meta(r)); err != nil {
		httpx.Error(w, r, err)
		return
	}
	clearRefreshCookie(w, p.Audience)
	httpx.NoContent(w)
}

func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	prof, err := h.svc.Profile(r.Context(), middleware.PrincipalFrom(r.Context()))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, prof)
}

func (h *Handler) JWKS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	httpx.JSON(w, http.StatusOK, map[string][]JWK{"keys": h.signer.JWKS()})
}

// writeTokens returns the access token in the body. Browser audiences get the
// refresh token as a cookie only; the POS (keystore storage) gets it in the
// body.
func (h *Handler) writeTokens(w http.ResponseWriter, pair *TokenPair) {
	resp := tokenResponse{
		AccessToken: pair.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   seconds(time.Until(pair.AccessExpiresAt)),
	}
	if name, ok := refreshCookieNames[pair.Audience]; ok {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    pair.RefreshToken,
			Path:     refreshCookiePath,
			Expires:  pair.RefreshExpiresAt,
			MaxAge:   seconds(time.Until(pair.RefreshExpiresAt)),
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
		})
	} else {
		resp.RefreshToken = pair.RefreshToken
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, http.StatusOK, resp)
}

func refreshTokenFrom(r *http.Request, body *refreshBody) string {
	if body.RefreshToken != "" {
		return body.RefreshToken
	}
	name, ok := refreshCookieNames[domain.Audience(body.Audience)]
	if !ok {
		return ""
	}
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

func clearRefreshCookie(w http.ResponseWriter, aud domain.Audience) {
	name, ok := refreshCookieNames[aud]
	if !ok {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: refreshCookiePath, MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

func meta(r *http.Request) RequestMeta {
	return RequestMeta{IP: httpx.ClientIP(r), UserAgent: r.UserAgent()}
}

func seconds(d time.Duration) int { return int(math.Round(d.Seconds())) }
