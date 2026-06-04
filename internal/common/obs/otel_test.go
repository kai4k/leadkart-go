package obs

import (
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/config"
)

// TestSetup_ResourceMergeNoSchemaConflict pins the semconv⇄sdk schema-URL
// coupling. resource.Merge(resource.Default(), …) runs on EVERY Setup —
// including the dev no-op path (empty OTLPEndpoint) — and fails with
// "conflicting Schema URL" whenever the semconv version imported here drifts
// from the one go.opentelemetry.io/otel/sdk's resource.Default() uses
// internally. That failure only surfaces at runtime (containers crash-loop on
// boot), so the build + the rest of the unit suite miss it — this test is the
// gate. If it goes red after an otel bump, realign the semconv import in
// otel.go with the new sdk's schema URL.
func TestSetup_ResourceMergeNoSchemaConflict(t *testing.T) {
	t.Parallel()

	shutdown, err := Setup(t.Context(), config.OTelConfig{
		ServiceName:    "leadkart-test",
		ServiceVersion: "0.0.0-test",
		// OTLPEndpoint left empty → no-op providers, but resource.Merge
		// still runs and is what we're guarding.
	})
	if err != nil {
		t.Fatalf("obs.Setup: %v", err)
	}
	if shutdown == nil {
		t.Fatal("obs.Setup returned nil Shutdown")
	}
	if err := shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
