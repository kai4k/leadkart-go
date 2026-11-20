package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/config"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

type noVerifier struct{}

func (noVerifier) Verify(string) (*jwt.Claims, error) { return nil, http.ErrNoCookie }

type noStamp struct{}

func (noStamp) IsFresh(context.Context, string, string) (bool, error) { return false, nil }

// Every per-route limit must sit on a pattern the identity module really
// registers: a path rename would otherwise drop its limit without any failure.
func TestRouteRateLimits_KeysAreRegisteredRoutes(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	ports.AddRoutes(mux, silentLogger(), app.Application{}, noVerifier{}, noStamp{})
	limits := routeRateLimits(config.Defaults().RateLimit)
	if len(limits) != 6 {
		t.Fatalf("%d route limits, want 6", len(limits))
	}
	for key, cfg := range limits {
		if cfg.RatePerSecond <= 0 || cfg.Burst <= 0 {
			t.Errorf("%s: non-positive limit %+v", key, cfg)
		}
		method, path, _ := strings.Cut(key, " ")
		req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
		if _, pattern := mux.Handler(req); pattern != key {
			t.Errorf("limit key %q resolves to registered pattern %q", key, pattern)
		}
	}
}
