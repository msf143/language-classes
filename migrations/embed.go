// Package migrations compiles the *.up.sql files in this folder into the Go
// binary, so the deployed image can migrate the database (`api migrate`)
// without shipping the files separately. The golang-migrate CLI used in dev
// reads the same files from disk and ignores this .go file.
package migrations

import "embed"

//go:embed *.up.sql
var FS embed.FS
