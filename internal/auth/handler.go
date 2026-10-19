package auth

import (
	"math"
	"net/http"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
	"github.com/duniandewon/madkunyah-be-service/internal/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
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
	Purpose    string `json:"purpose" validate:"required,oneof=customer_login staff_login device_enroll"`
	OutletID   string `json:"outlet_id" validate:"required_if=Purpose device_enroll,omitempty,uuid"`
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

// enrollBody redeems a device_enroll code. The device submits it itself, so
// the refresh token is bound to the device that will hold it.
type enrollBody struct {
	Identifier        string `json:"identifier" validate:"required"`
	Code              string `json:"code" validate:"required,numeric,max=10"`
	OutletID          string `json:"outlet_id" validate:"required,uuid"`
	Name              string `json:"name" validate:"required,max=100"`
	DeviceFingerprint string `json:"device_fingerprint" validate:"required,max=255"`
	Replace           bool   `json:"replace"`
}

type enrollResponse struct {
	tokenResponse
	DeviceID string `json:"device_id"`
	OutletID string `json:"outlet_id"`
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
	in := OTPRequest{Identifier: body.Identifier, Purpose: Purpose(body.Purpose), OutletID: body.OutletID}
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

func (h *Handler) EnrollDevice(w http.ResponseWriter, r *http.Request) {
	var body enrollBody
	if err := httpx.DecodeValid(w, r, &body); err != nil {
		httpx.Error(w, r, err)
		return
	}
	dev, err := h.svc.EnrollDevice(r.Context(), EnrollInput{
		Identifier: body.Identifier, Code: body.Code, OutletID: body.OutletID,
		Name: body.Name, Fingerprint: body.DeviceFingerprint, Replace: body.Replace,
	}, meta(r))
	if err != nil {
		httpx.Error(w, r, err)
		return
	}
	resp := enrollResponse{tokenResponse: h.tokenBody(w, dev.Tokens), DeviceID: dev.DeviceID, OutletID: dev.OutletID}
	httpx.JSON(w, http.StatusCreated, resp)
}

// RevokeDevice treats a malformed device ID as not_found, like an unknown one.
func (h *Handler) RevokeDevice(w http.ResponseWriter, r *http.Request) {
	deviceID := chi.URLParam(r, "deviceID")
	if _, err := uuid.Parse(deviceID); err != nil {
		httpx.Error(w, r, domain.ErrNotFound)
		return
	}
	if err := h.svc.RevokeDevice(r.Context(), middleware.AccessFrom(r.Context()), deviceID, meta(r)); err != nil {
		httpx.Error(w, r, err)
		return
	}
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
	httpx.JSON(w, http.StatusOK, h.tokenBody(w, pair))
}

// tokenBody sets the refresh cookie and no-store header for pair and
// returns the response body.
func (h *Handler) tokenBody(w http.ResponseWriter, pair *TokenPair) tokenResponse {
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
	return resp
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
