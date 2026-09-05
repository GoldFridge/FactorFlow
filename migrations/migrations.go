// Package migrations embeds the SQL schema so a binary carries its own migrations.
//
// Shipping the schema inside the binary means a deploy cannot land with application code
// and migration files out of step, which on a one-server demo is the failure that costs an
// evening.
package migrations

import "embed"

// FS holds every goose migration in this directory.
//
//go:embed *.sql
var FS embed.FS
