// Package api embeds Ratiba's machine-readable API contract.
//
// The OpenAPI document is the source of truth for the wire format. Embedding it
// into the binary means the running service always serves the contract it was
// built from — a deployed API and its published schema cannot drift apart, and
// there is no separate artifact to copy into the container image.
package api

import _ "embed"

// OpenAPISpec is the OpenAPI 3.1 document, served at GET /openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPISpec []byte
