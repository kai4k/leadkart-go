package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/platform/obs"
)

// silentLogger discards log output so test runs stay quiet.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestPublicServer_DoesNotMountHealth pins the v0.2 contract: probes
// (/alive, /ready, /health) live on the admin listener exclusively.
// A request to /health on the public mux returns 404.
func TestPublicServer_DoesNotMountHealth(t *testing.T) {
	t.Parallel()
	srv := newServer(silentLogger(), app.Application{}, nil)
	for _, path := range []string{"/alive", "/ready", "/health"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
			srv.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("public %s: got %d want 404 (probes belong on admin)", path, rec.Code)
			}
		})
	}
}

// TestAdminServer_MountsHealthEndpoints verifies the admin port
// surfaces the three-endpoint health split per obs.Health.
func TestAdminServer_MountsHealthEndpoints(t *testing.T) {
	t.Parallel()
	health := obs.NewHealth(nil, 0)
	adminSrv := obs.NewAdminServer(":0", health)
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/alive", 200},
		{"/ready", 200},
		{"/health", 200},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil)
			adminSrv.Handler.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("admin %s: got %d want %d", tc.path, rec.Code, tc.want)
			}
		})
	}
}

// TestAdminServer_ServesPprof — pprof index reachable on admin port.
func TestAdminServer_ServesPprof(t *testing.T) {
	t.Parallel()
	adminSrv := obs.NewAdminServer(":0", nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/debug/pprof/", nil)
	adminSrv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/debug/pprof/: got %d want 200", rec.Code)
	}
}
