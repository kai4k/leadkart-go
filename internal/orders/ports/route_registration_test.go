// route_registration_test.go — TestArch_ route-conflict gate per ADR 0049.
// Mirror of internal/dispatch/ports/route_registration_test.go.
package ports_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/orders/app"
	"github.com/leadkart/leadkart-go/internal/orders/ports"
)

var errStubNotCalled = errors.New("stub verifier — not exercised by route-registration test")

type stubVerifier struct{}

func (stubVerifier) Verify(string) (*jwt.Claims, error) { return nil, errStubNotCalled }

type stubStampValidator struct{}

func (stubStampValidator) IsFresh(_ context.Context, _, _ string) (bool, error) { return true, nil }

// TestArch_RouteRegistration_NoConflicts asserts Orders route registration
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
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/orders/quotations", nil)
	_, pattern := mux.Handler(req)
	if pattern == "" {
		t.Fatal("expected at least one Orders route registered")
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
