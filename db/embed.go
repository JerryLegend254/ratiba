// Package db embeds Ratiba's SQL migrations.
//
// Embedding rather than shipping a directory means the migrate binary is a
// single self-contained file. That matters for the deployment model: Railway's
// pre-deploy step runs this binary from the same image as the API, with no
// volume mounts and no assumption that a migrations directory was copied to the
// right place.
package db

import "embed"

// Migrations holds the goose migration files, applied in filename order.
//
//go:embed migrations/*.sql
var Migrations embed.FS
