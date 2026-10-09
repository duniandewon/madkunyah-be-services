package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

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
// transaction per file, recording each in schema_migrations.
//
// This is intentionally a small runner rather than a migration library: the
// behaviour is fully visible, and there is no second tool to install in CI or
// in the container.
func (db *DB) Migrate(ctx context.Context) error {
	if migrationFS == nil {
		return fmt.Errorf("migrations not registered: call SetMigrations first")
	}

	_, err := db.Pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     TEXT PRIMARY KEY,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := db.Pool.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	entries, err := fs.ReadDir(migrationFS, ".")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	var versions []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			versions = append(versions, e.Name())
		}
	}
	sort.Strings(versions)

	for _, name := range versions {
		version := strings.TrimSuffix(name, ".up.sql")
		if applied[version] {
			continue
		}
		body, err := fs.ReadFile(migrationFS, name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		tx, err := db.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
	}
	return nil
}
