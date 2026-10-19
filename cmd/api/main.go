package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/duniandewon/madkunyah-be-service/internal/auth"
	"github.com/duniandewon/madkunyah-be-service/internal/authz"
	"github.com/duniandewon/madkunyah-be-service/internal/config"
	"github.com/duniandewon/madkunyah-be-service/internal/domain"
	"github.com/duniandewon/madkunyah-be-service/internal/pos"
	"github.com/duniandewon/madkunyah-be-service/internal/server"
	"github.com/duniandewon/madkunyah-be-service/internal/staff"
	"github.com/duniandewon/madkunyah-be-service/migrations"
	"github.com/duniandewon/madkunyah-be-service/platform/notifier"
	"github.com/duniandewon/madkunyah-be-service/platform/postgres"

	credis "github.com/duniandewon/madkunyah-be-service/platform/redis"
)

func main() {
	if err := run(); err != nil {
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	setupLogging(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := postgres.Connect(ctx, postgres.Config{
		DSN:         cfg.DB.DSN,
		MaxConns:    cfg.DB.MaxConns,
		MaxConnIdle: cfg.DB.MaxConnIdle,
	})
	if err != nil {
		return err
	}
	defer db.Close()

	if err := postgres.SetMigrations(migrations.FS, "."); err != nil {
		return err
	}
	if err := db.Migrate(ctx); err != nil {
		return err
	}

	// Redis holds refresh tokens and OTP state, so it is a hard dependency:
	// without it nobody can authenticate. Run it with AOF persistence.
	rdb, err := credis.Connect(ctx, credis.Config{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := rdb.Close(); err != nil {
			slog.Error("close redis", "error", err)
		}
	}()

	signer, err := auth.NewSigner(cfg.Auth.JWTPrivateKeyPEM, cfg.Auth.JWTPublicKeyPEM,
		cfg.Auth.JWTPreviousPublicKeyPEM, cfg.Auth.Issuer, cfg.Auth.AccessTTL)
	if err != nil {
		return err
	}
	access := authz.NewResolver(db, rdb, cfg.Auth.AuthzCacheTTL)
	authRepo := auth.NewRepository(db)
	authSvc, dispatcher, err := newAuthService(cfg, authRepo, rdb, signer, access)
	if err != nil {
		return err
	}
	defer dispatcher.Wait()

	staffSvc := staff.NewService(staff.NewRepository(db), access, rdb, staff.PINConfig{
		Denylist:  cfg.PIN.Denylist,
		MemoryKiB: cfg.PIN.Argon2MemoryKiB,
		Time:      cfg.PIN.Argon2Time,
		Threads:   cfg.PIN.Argon2Threads,
	})

	srv := server.New(server.Deps{
		Config:  cfg,
		DB:      db,
		Redis:   rdb,
		Auth:    auth.NewHandler(authSvc, signer),
		Tokens:  signer,
		Access:  access,
		Devices: authRepo,
		Staff:   staff.NewHandler(staffSvc),
		POS:     pos.NewHandler(pos.NewService(pos.NewRepository(db))),
	})

	errCh := make(chan error, 1)
	go func() {
		slog.Info("api listening", "port", cfg.HTTP.Port, "env", cfg.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	}

	// Drain in-flight requests: an order being written when SIGTERM arrives
	// must finish, or the POS is left holding a number with no order.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	slog.Info("shutdown complete")
	return nil
}

func newAuthService(cfg *config.Config, repo *auth.Repository, rdb *credis.Client, signer *auth.Signer,
	access *authz.Resolver) (*auth.Service, *auth.Dispatcher, error) {
	loc, err := time.LoadLocation(cfg.OTP.QuotaTimezone)
	if err != nil {
		return nil, nil, err
	}
	// The log notifier stands in until WhatsApp/SMTP are integrated. Codes are
	// printed only in local development.
	dispatcher := auth.NewDispatcher(notifier.NewLog(cfg.Env == "development"), rdb, cfg.OTP.DispatchRetryWindow)

	svc := auth.NewService(auth.Config{
		Pepper:              []byte(cfg.OTP.Pepper),
		OTPLength:           cfg.OTP.Length,
		OTPTTL:              cfg.OTP.TTL,
		Cooldown:            cfg.OTP.Cooldown,
		MaxAttempts:         cfg.OTP.MaxAttempts,
		DailyQuota:          cfg.OTP.DailyQuota,
		IPHourlyQuota:       cfg.OTP.IPHourlyQuota,
		VerifyIPHourlyQuota: cfg.OTP.VerifyIPHourlyQuota,
		QuotaLocation:       loc,
		RefreshTTL: map[domain.Audience]time.Duration{
			domain.AudienceCustomer: cfg.Auth.CustomerRefreshTTL,
			domain.AudienceStaff:    cfg.Auth.StaffRefreshTTL,
			domain.AudiencePOS:      cfg.Auth.DeviceRefreshTTL,
		},
		RefreshGrace: cfg.Auth.RefreshGrace,
		DeviceLimit:  cfg.Auth.DeviceLimit,
	}, auth.Deps{
		OTPs:       rdb,
		Limiter:    rdb,
		Sessions:   rdb,
		Revoker:    rdb,
		Subjects:   repo,
		Profiles:   repo,
		Events:     repo,
		Dispatcher: dispatcher,
		Signer:     signer,
		Access:     access,
		Devices:    repo,
		Tracker:    repo,
	})
	return svc, dispatcher, nil
}

func setupLogging(cfg *config.Config) {
	level := slog.LevelDebug
	if cfg.IsProduction() {
		level = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
}
