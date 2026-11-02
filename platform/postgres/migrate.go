package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLockKey identifies the advisory lock held while migrating. The
// value is arbitrary; it only has to be constant across instances.
const migrationLockKey int64 = 7263548190

// migrationFS is populated by the caller via SetMigrations so this package does
// not need to know where the .sql files live on disk.
var migrationFS fs.FS

func SetMigrations(f embed.FS, dir string) error {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		return err
	}
	migrationFS = sub
	return nil
}

// Migrate applies every pending *.up.sql file in lexical order inside a single
// transaction per file, recording each in schema_migrations. An advisory lock
// serializes instances that start together; a later instance sees the versions
// the first one applied.
//
// This is intentionally a small runner rather than a migration library: the
// behaviour is fully visible, and there is no second tool to install in CI or
// in the container.
func (db *DB) Migrate(ctx context.Context) error {
	if migrationFS == nil {
		return fmt.Errorf("migrations not registered: call SetMigrations first")
	}

	// The lock belongs to the session, so everything must use this connection.
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer unlockMigrations(ctx, conn)

	applied, err := appliedVersions(ctx, conn)
	if err != nil {
		return err
	}
	names, err := migrationNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		if applied[strings.TrimSuffix(name, ".up.sql")] {
			continue
		}
		if err := applyMigration(ctx, conn, name); err != nil {
			return err
		}
	}
	return nil
}

// unlockMigrations must not leave the lock on a connection going back to the
// pool, where it would block every later migration. Closing the session drops
// the lock if the unlock itself fails, for instance on a cancelled startup.
func unlockMigrations(ctx context.Context, conn *pgxpool.Conn) {
	ctx = context.WithoutCancel(ctx)
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, migrationLockKey); err != nil {
		_ = conn.Conn().Close(ctx)
	}
}

func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[string]bool, error) {
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     TEXT PRIMARY KEY,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationFS, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func applyMigration(ctx context.Context, conn *pgxpool.Conn, name string) error {
	body, err := fs.ReadFile(migrationFS, name)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("apply %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1)`, strings.TrimSuffix(name, ".up.sql")); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s: %w", name, err)
	}
	return nil
}
