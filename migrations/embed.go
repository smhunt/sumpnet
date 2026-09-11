// Package migrations embeds the SQL migrations so the Go store (tests, the
// ingest schema check) and the migrate CLI/container read the same files.
package migrations

import "embed"

// FS holds every *.sql migration, in golang-migrate's NNNN_name.{up,down}.sql layout.
//
//go:embed *.sql
var FS embed.FS
