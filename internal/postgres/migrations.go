package postgres

import "embed"

// Migrations holds the goose SQL migration files, embedded so callers (the
// server at startup, integration tests) can run them without a path on disk.
// Use it with goose.SetBaseFS and a dir of "migrations".
//
//go:embed migrations/*.sql
var Migrations embed.FS
