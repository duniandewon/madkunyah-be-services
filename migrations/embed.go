// Package migrations embeds the SQL migration files so the binary carries its
// own schema and needs no files on disk at runtime.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
