// Package migrations owns the embedded, ordered SQLite schema migrations.
package migrations

import "embed"

// Files is the schema shipped with the binary; deployment needs no SQL files.
//
//go:embed *.sql
var Files embed.FS
