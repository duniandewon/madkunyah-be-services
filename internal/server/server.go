package server

import (
	"context"
	"net/http"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/config"
	"github.com/duniandewon/madkunyah-be-service/internal/httpx"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"
	"github.com/go-chi/chi/v5"

	mw "github.com/duniandewon/madkunyah-be-service/internal/middleware"
	credis "github.com/duniandewon/madkunyah-be-service/platform/redis"
)

type Deps struct {
	Config *config.Config
	DB     *postgres.DB
	Redis  *credis.Client
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
