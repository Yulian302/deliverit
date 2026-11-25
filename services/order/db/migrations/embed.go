// Package migrations holds Order's schema migrations (goose .sql files),
// compiled into the binary so the container needs no extra files.
package migrations

import "embed"

// FS contains every migration file; pass it to pg.Migrate.
//
//go:embed *.sql
var FS embed.FS
