package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	Pool *pgxpool.Pool
}

type Config struct {
	DSN             string
	MaxConns        int32
	MaxConnIdle     time.Duration
	MaxConnLifetime time.Duration
}

func Connect(ctx context.Context, cfg Config) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse database dsn: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdle
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &DB{Pool: pool}, nil
}

func (db *DB) Close() { db.Pool.Close() }

func (db *DB) Health(ctx context.Context) error { return db.Pool.Ping(ctx) }

func (db *DB) InTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && rbErr != pgx.ErrTxClosed {
			return fmt.Errorf("%w (rollback failed: %v)", err, rbErr)
		}
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		// A PgError means the server answered and rolled back. Anything else
		// (a cancelled context, a dropped connection) leaves the outcome
		// unknown: the commit may have landed.
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			return fmt.Errorf("commit transaction: %w: %w", ErrCommitUncertain, err)
		}
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// ErrCommitUncertain marks an InTx error where the commit was sent but its
// outcome is unknown. Callers that react to a successful write, such as
// cache invalidation, should treat it as possibly applied.
var ErrCommitUncertain = errors.New("commit outcome unknown")
