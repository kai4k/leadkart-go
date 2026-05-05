package obs

import (
	"net/http"
	"net/http/pprof"
	"time"
)

// NewAdminServer returns the admin-port http.Handler carrying:
//
//   - /debug/pprof/* (heap, goroutine, profile, trace, etc.)
//   - /alive, /ready, /health (mounted via Health.Mount)
//   - future /metrics if Prometheus scraping is added
//
// The admin port MUST be a separate listener from the public API per
// `audit-checklist.md §12` — pprof on the public port leaks profiling
// surface to attackers + makes it trivial to dump heap snapshots.
//
// Server timeouts are intentionally generous; pprof traces can run
// 30s+ when sampling. ReadHeaderTimeout still bounds slowloris attacks.
func NewAdminServer(addr string, health *Health) *http.Server {
	mux := http.NewServeMux()

	// pprof handlers — register the canonical paths the Go tool expects.
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)

	// Health endpoints live on admin so kube probes don't compete with
	// public traffic for connection slots.
	if health != nil {
		health.Mount(mux)
	}

	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// pprof Profile + Trace handlers stream for ≥30s; long
		// WriteTimeout accommodates while still bounding stuck handlers.
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}
}
