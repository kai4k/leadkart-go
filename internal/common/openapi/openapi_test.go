package openapi_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/leadkart/leadkart-go/internal/common/openapi"
)

// GET /openapi.yaml serves api/openapi.yaml itself: one source, no embedded copy
// that drifts (the old all_routes.yaml was a stale 3 200-line duplicate).
func TestSpecHandler_ServesTheCanonicalSpec(t *testing.T) {
	t.Parallel()
	want, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read api/openapi.yaml: %v", err)
	}

	rec := httptest.NewRecorder()
	openapi.SpecHandler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/openapi.yaml", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/yaml; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatalf("served spec (%d bytes) differs from api/openapi.yaml (%d bytes): the endpoint must embed that file", rec.Body.Len(), len(want))
	}

	var doc struct {
		OpenAPI string         `yaml:"openapi"`
		Paths   map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("served spec does not parse: %v", err)
	}
	if doc.OpenAPI == "" || len(doc.Paths) < 50 {
		t.Errorf("served spec looks wrong: openapi=%q paths=%d", doc.OpenAPI, len(doc.Paths))
	}
}
