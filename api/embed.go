// Package api embeds the OpenAPI contract (api/openapi.yaml, the single source of
// truth per ADR 0046) so the API binary can serve it at GET /openapi.yaml without
// a file on the image. go:embed cannot reach a parent directory, so the embed
// lives next to the file.
package api

import _ "embed"

// OpenAPIYAML is api/openapi.yaml, baked in at build time.
//
//go:embed openapi.yaml
var OpenAPIYAML []byte
