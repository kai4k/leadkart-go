// route_registration_test.go — TestArch_ guard against Go 1.22+ ServeMux
// pattern-conflict panics at PR time, BEFORE smoke-test CI.
//
// Mirror of internal/identity/ports/route_registration_test.go per ADR 0049.
// Catches pattern overlaps that would panic at server startup, NOT at
// remote CI smoke time.
package ports_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/inventory/app"
	"github.com/leadkart/leadkart-go/internal/inventory/ports"
)

type stubVerifier struct{}

//nolint:nilnil // stub-test-double; method body unreachable in this arch test
func (stubVerifier) Verify(string) (*jwt.Claims, error) { return nil, nil }

type stubStampValidator struct{}

func (stubStampValidator) IsFresh(context.Context, string, string) (bool, error) {
	return true, nil
}

// TestArch_RouteRegistration_NoConflicts wires AddRoutes against a
// fresh ServeMux + fails on any panic from Go's pattern-overlap detector.
// <50ms; joins task test:arch.
func TestArch_RouteRegistration_NoConflicts(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("route registration panicked — Go 1.22+ ServeMux\n"+
				"detected a pattern overlap. Per ADR 0049 URL-design rule:\n"+
				"  - lookups by non-primary-key use query params (?slug=, ?email=)\n"+
				"    instead of path segments (/by-slug/{slug})\n"+
				"  - tenant-scoped sub-resources use a literal segment after\n"+
				"    {tenantId} (/tenants/{id}/audit/events, not /activity)\n"+
				"Fix the new route, don't relax this gate.\n\n"+
				"panic: %v", r)
		}
	}()

	mux := http.NewServeMux()
	ports.AddRoutes(mux, nil, app.Application{}, stubVerifier{}, stubStampValidator{})
}
