// Package openapi serves the canonical LeadKart HTTP API contract
// per ADR 0046 — spec-first OpenAPI 3.1 + Scalar UI at /docs.
//
// The spec lives at repo-root api/openapi.yaml; this package embeds
// it into the binary via //go:embed so the Chainguard distroless
// image needs no external file dependency. Two handlers exposed:
//
//   - [SpecHandler] serves the raw YAML at GET /openapi.yaml
//     (clients + tooling fetch this for codegen + introspection)
//   - [ScalarHandler] serves the Scalar UI HTML page at GET /docs
//     (renders the spec for human + AI engineers)
//
// The handlers are stateless and safe to share. Composition root
// mounts them on the public mux per [cmd/api/main.go].
package openapi

import (
	_ "embed"
	"net/http"
)

// specYAML is the entire canonical spec embedded at build time.
// Per ADR 0024 (Chainguard distroless static): the binary ships
// self-contained; no /api/openapi.yaml file on the deployed image.
//
//go:embed all_routes.yaml
var specYAML []byte

// SpecHandler serves the raw OpenAPI YAML at the mount point.
// Tooling (openapi-typescript, openapi-generator, Prism, Postman
// import) fetches this. Cache-Control set to a short TTL — the
// spec is build-baked but tooling re-fetches between deploys.
func SpecHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=60")
		_, _ = w.Write(specYAML)
	})
}

// ScalarHandler serves the Scalar UI HTML page. Loaded from CDN at
// runtime (single-script tag); the page references the spec at
// /openapi.yaml on the SAME origin.
//
// CDN-loaded JS is a deliberate v0.2 simplification — keeps the
// binary smaller + lets the UI auto-update across Scalar releases.
// Phase 5 hardening can switch to a bundled Scalar build embedded
// alongside the spec for offline / strict-CSP deployments.
//
// References /openapi.yaml as a relative URL so the same handler
// works under any host / front-door path prefix.
func ScalarHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write([]byte(scalarHTML))
	})
}

// scalarHTML is the single-page Scalar UI shell. Loads spec from
// /openapi.yaml (same-origin) + the Scalar bundle from CDN.
//
// The configuration object follows Scalar's documented schema:
// https://github.com/scalar/scalar/blob/main/documentation/configuration.md
const scalarHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>LeadKart API — Documentation</title>
<link rel="icon" href="data:," />
</head>
<body>
  <script id="api-reference" data-url="/openapi.yaml"></script>
  <script>
    var configuration = {
      theme: 'default',
      layout: 'modern',
      hideClientButton: false,
      defaultOpenAllTags: false,
      metaData: {
        title: 'LeadKart API',
        description: 'Multi-tenant SaaS for Indian PCD pharma'
      }
    };
    document.getElementById('api-reference').dataset.configuration =
      JSON.stringify(configuration);
  </script>
  <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
</body>
</html>
`
