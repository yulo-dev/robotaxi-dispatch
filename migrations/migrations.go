// Package migrations embeds the SQL schema migrations.
package migrations

import "embed"

// FS holds the migration files, applied in lexical order.
//
//go:embed *.sql
var FS embed.FS
