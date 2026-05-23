// route_registration_test.go — TestArch_ route-conflict gate per ADR 0049.
// Mirror of internal/identity/ports/route_registration_test.go.
//
// Wires CRM's AddRoutes against a fresh http.ServeMux with stub
// dependencies. Any panic from Go 1.22+'s pattern-overlap detector
// fails the test. Catches the bug class that broke Wave 7 at unit-
// test time, NOT at remote Docker smoke time.

package ports_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/crm/app"
	"github.com/leadkart/leadkart-go/internal/crm/ports"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
)

// errStubNotCalled is the sentinel error stub authorities return so
// linters don't flag nil/nil returns. The route-registration test
// never invokes these stubs — AddRoutes only wires against them.
var errStubNotCalled = errors.New("stub verifier — not exercised by route-registration test")

type stubVerifier struct{}

func (stubVerifier) Verify(string) (*jwt.Claims, error) { return nil, errStubNotCalled }

type stubStampValidator struct{}

func (stubStampValidator) IsFresh(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}

// TestArch_RouteRegistration_NoConflicts asserts CRM route registration
// completes without a Go 1.22 ServeMux pattern-overlap panic.
func TestArch_RouteRegistration_NoConflicts(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("route registration panicked — Go 1.22 ServeMux pattern conflict.\n panic: %v", r)
		}
	}()
	mux := http.NewServeMux()
	log := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	ports.AddRoutes(mux, log, app.Application{}, stubVerifier{}, stubStampValidator{})
	// Smoke-test that at least one route exists post-registration.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/crm/leads", nil)
	_, pattern := mux.Handler(req)
	if pattern == "" {
		t.Fatal("expected at least one CRM route registered")
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
