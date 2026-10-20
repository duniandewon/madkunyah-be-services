package server

import (
	"context"
	"net/http"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/auth"
	"github.com/duniandewon/madkunyah-be-service/internal/config"
	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
	"github.com/duniandewon/madkunyah-be-service/internal/pos"
	"github.com/duniandewon/madkunyah-be-service/internal/staff"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"github.com/go-chi/chi/v5"

	mw "github.com/duniandewon/madkunyah-be-service/internal/middleware"
	credis "github.com/duniandewon/madkunyah-be-service/platform/redis"
)

type Deps struct {
	Config  *config.Config
	DB      *postgres.DB
	Redis   *credis.Client
	Auth    *auth.Handler
	Tokens  mw.TokenVerifier
	Access  mw.AccessResolver
	Devices mw.DeviceChecker
	Staff   *staff.Handler
	POS     *pos.Handler
}

func New(d Deps) *http.Server {
	return &http.Server{
		Addr:         ":" + d.Config.HTTP.Port,
		Handler:      Router(d),
		ReadTimeout:  d.Config.HTTP.ReadTimeout,
		WriteTimeout: d.Config.HTTP.WriteTimeout,
	}
}

func Router(d Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(mw.RequestID)
	r.Use(mw.Recover)
	r.Use(mw.Logging)
	r.Use(mw.CORS(d.Config.HTTP.CORSOrigins))
	r.Use(mw.Timeout(25 * time.Second))

	r.Get("/health", healthHandler())
	r.Get("/ready", readyHandler(d))
	r.Get("/.well-known/jwks.json", d.Auth.JWKS)

	authn := func(auds ...domain.Audience) func(http.Handler) http.Handler {
		return mw.Authenticate(d.Tokens, d.Redis, auds...)
	}

	r.Route("/v1", func(r chi.Router) {
		r.Route("/auth", func(r chi.Router) {
			r.Post("/otp/request", d.Auth.RequestOTP)
			r.Post("/otp/verify", d.Auth.VerifyOTP)
			r.Post("/token/refresh", d.Auth.Refresh)
			r.Post("/logout", d.Auth.Logout)
			// Self-service: acts only on the caller's own sessions, so it is
			// the one route shared by both browser audiences.
			r.With(authn(domain.AudienceCustomer, domain.AudienceStaff)).
				Post("/sessions/revoke-all", d.Auth.RevokeAll)
			// Unauthenticated: the device_enroll code in the body is the
			// credential.
			r.Post("/devices/enroll", d.Auth.EnrollDevice)
			r.With(authn(domain.AudienceStaff), mw.LoadStaffAccess(d.Access)).
				Post("/devices/{deviceID}/revoke", d.Auth.RevokeDevice)
		})

		r.Group(func(r chi.Router) {
			r.Use(authn(domain.AudienceCustomer))
			r.Get("/me", d.Auth.Me)
			r.Post("/me/identifiers/otp", d.Auth.RequestIdentifierOTP)
			r.Post("/me/identifiers", d.Auth.AddIdentifier)
		})

		r.Route("/staff", func(r chi.Router) {
			r.Use(authn(domain.AudienceStaff), mw.LoadStaffAccess(d.Access))
			r.Get("/me", d.Auth.Me)
			r.Post("/me/pin", d.Staff.SetPIN)
			r.Delete("/{staffID}/pin", d.Staff.ResetPIN)
			r.Put("/{staffID}/global-roles/{role}", d.Staff.GrantGlobalRole)
			r.Delete("/{staffID}/global-roles/{role}", d.Staff.RevokeGlobalRole)
			r.Post("/{staffID}/deactivate", d.Staff.Deactivate)
			r.Post("/{staffID}/sessions/revoke-all", d.Staff.RevokeSessions)
		})

		r.Route("/outlets/{outletID}/staff", func(r chi.Router) {
			r.Use(authn(domain.AudienceStaff), mw.LoadStaffAccess(d.Access),
				mw.RequireOutletPermission(domain.PermStaffManage))
			r.Get("/", d.Staff.List)
			r.Post("/", d.Staff.Create)
			r.Put("/{staffID}/roles/{role}", d.Staff.GrantRole)
			r.Delete("/{staffID}/roles/{role}", d.Staff.RevokeRole)
		})

		r.Route("/pos", func(r chi.Router) {
			r.Use(authn(domain.AudiencePOS), mw.RequireActiveDevice(d.Devices))
			r.Get("/roster", d.POS.Roster)
			r.Post("/pin-failures", d.POS.PINFailures)
		})
	})

	return r
}

func healthHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func readyHandler(d Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		checks := map[string]string{"database": "ok", "redis": "ok"}
		status := http.StatusOK

		if err := d.DB.Health(ctx); err != nil {
			checks["database"] = "unavailable"
			status = http.StatusServiceUnavailable
		}

		if err := d.Redis.Health(ctx); err != nil {
			checks["redis"] = "unavailable"
			status = http.StatusServiceUnavailable
		}

		httpx.JSON(w, status, checks)
	}
}
