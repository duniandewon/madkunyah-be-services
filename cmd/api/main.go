package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/duniandewon/madkunyah-be-service/internal/config"
	"github.com/duniandewon/madkunyah-be-service/internal/server"
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
	defer rdb.Close()

	srv := server.New(server.Deps{
		Config: cfg,
		DB:     db,
		Redis:  rdb,
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

func setupLogging(cfg *config.Config) {
	level := slog.LevelDebug
	if cfg.IsProduction() {
		level = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(handler))
}
