// route_registration_test.go — TestArch_ guard against Go 1.22+ ServeMux
// pattern-conflict panics at PR time, BEFORE smoke-test CI.
//
// Per ADR 0049: every route mounted by [ports.AddRoutes] is registered
// against an http.ServeMux. Go 1.22+'s ServeMux panics at registration
// time if two patterns overlap without one being strictly more
// specific (e.g. `/x/{a}/y` vs `/x/b/{c}` — both can match `/x/b/y`).
//
// History: Wave 7 first surfaced this class of bug when
// `GET /tenants/{tenantId}/activity` (Wave 2) collided with
// `GET /tenants/by-slug/{slug}` (slug branch). Both 5-segment patterns
// with literal-and-wildcard in opposite positions. Local `task ci` did
// NOT catch it because no unit test exercised the full route table;
// only the Docker smoke test caught it after merge attempt. This test
// closes that gap.
//
// The test wires AddRoutes with synthetic-minimal dependencies:
//   - mux: a fresh http.ServeMux
//   - log: nil (handler factories don't touch log at construction)
//   - a: zero-value app.Application (handler factories return closures
//     that capture `a`; bodies execute on request, not on registration)
//   - verifier + stampValidator: no-op stubs so auth-gated routes ALSO
//     register (the full route table is exercised)
//
// Any future addition of a conflicting route surfaces as a `t.Fatalf`
// at unit-test time. Drift impossible.

package ports_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// stubVerifier is the no-op authn.Verifier implementation used solely
// to make AddRoutes register the auth-gated route block during the
// arch test. Verify is never invoked because the test never sends a
// request; we only care about [ServeMux.Handle] not panicking.
type stubVerifier struct{}

// Verify intentionally returns (nil, nil) — the test never invokes
// this method (no requests are sent); the implementation exists only
// to satisfy the interface so [ports.AddRoutes] registers the auth-
// gated route block. nilnil lint exemption is the canonical pattern
// for stub-test-doubles per Khorikov "Unit Testing Principles" §5.5.
//
//nolint:nilnil // stub-test-double; method body unreachable in this arch test
func (stubVerifier) Verify(string) (*jwt.Claims, error) { return nil, nil }

// stubStampValidator is the no-op authn.StampValidator counterpart.
type stubStampValidator struct{}

func (stubStampValidator) IsFresh(context.Context, string, string) (bool, error) {
	return true, nil
}

// TestArch_RouteRegistration_NoConflicts wires AddRoutes against a
// fresh ServeMux and fails on any panic from Go's pattern-overlap
// detector. The test is fast (< 50ms) and runs on every PR.
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
