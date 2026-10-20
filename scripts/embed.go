// Package scripts compiles seed.sql into the Go binary, so the staging
// migration job can load test data (`api migrate --seed`) without the file
// being shipped separately. It is never run on production (see cmd/api/testmode.go).
package scripts

import _ "embed"

//go:embed seed.sql
var Seed string
