package obs

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// HealthChecker is a single component-level liveness/readiness probe.
// Implementations close around the resource they check (DB ping, Redis
// ping, downstream service GET). Name returns the component label.
type HealthChecker interface {
	Name() string
	Check(ctx context.Context) error
}

// HealthCheckerFunc adapts a plain func to HealthChecker for ad-hoc
// probes registered at composition root.
type HealthCheckerFunc struct {
	N  string
	Fn func(ctx context.Context) error
}

// Name returns the human-readable component identifier.
func (h HealthCheckerFunc) Name() string { return h.N }

// Check runs the wrapped function against the supplied context.
func (h HealthCheckerFunc) Check(ctx context.Context) error { return h.Fn(ctx) }

// Health holds the registered checkers + serves the three canonical
// endpoints per `audit-checklist.md §12`:
//
//	/alive  — process up; never checks dependencies. K8s liveness.
//	/ready  — process up + every dependency reachable. K8s readiness.
//	/health — full diagnostics body; never a probe target.
//
// Per the checklist: caching either of the probe endpoints masks
// downstream failures from Kubernetes, so they MUST be excluded from
// any OutputCache policy. The HTTP handlers below set Cache-Control
// no-store explicitly to defend against accidental caching.
type Health struct {
	mu       sync.RWMutex
	checkers []HealthChecker
	timeout  time.Duration
}

// NewHealth constructs a Health with the supplied per-check timeout.
// Default timeout (per arg <= 0) is 2 seconds — Kubernetes default
// readiness probe timeout is also 1-2s.
func NewHealth(checkers []HealthChecker, timeout time.Duration) *Health {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &Health{checkers: checkers, timeout: timeout}
}

// Register adds a HealthChecker post-construction. Useful when the
// composition root constructs Health early, then the Postgres + Redis
// clients are wired below.
func (h *Health) Register(c HealthChecker) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.checkers = append(h.checkers, c)
}

// Alive — liveness probe handler. Always 200 with `ok\n`.
func (h *Health) Alive(w http.ResponseWriter, _ *http.Request) {
	noStore(w)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// healthStatusOK is the wire-stable string returned by /ready and
// /health for a component that passed its check. Anything else in the
// `report` map's value position is the error message.
const healthStatusOK = "ok"

// Ready — readiness probe. Runs every checker concurrently with the
// per-check timeout; 200 if all pass, 503 if ANY fail. Body is a
// JSON map of {component: status}.
func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	report := h.runChecks(r.Context())
	status := http.StatusOK
	for _, v := range report {
		if v != healthStatusOK {
			status = http.StatusServiceUnavailable
			break
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(report)
}

// Health — diagnostics endpoint. Always 200; body carries every
// checker's status + the per-check error message. NEVER a probe target.
func (h *Health) Health(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	report := h.runChecks(r.Context())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(report)
}

// runChecks executes all registered checkers concurrently; returns
// {name: "ok"} OR {name: error.Error()} per component.
func (h *Health) runChecks(ctx context.Context) map[string]string {
	h.mu.RLock()
	checkers := make([]HealthChecker, len(h.checkers))
	copy(checkers, h.checkers)
	h.mu.RUnlock()

	report := make(map[string]string, len(checkers))
	if len(checkers) == 0 {
		return report
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	// Go 1.22 — loop variable per-iteration capture is safe; no need
	// for `c := c` shadowing. Go 1.25 — wg.Go captures spawn + Done.
	for _, c := range checkers {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, h.timeout)
			defer cancel()
			err := c.Check(cctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				report[c.Name()] = err.Error()
				return
			}
			report[c.Name()] = healthStatusOK
		})
	}
	wg.Wait()
	return report
}

// Mount registers /alive, /ready, /health on the supplied mux. Use
// stdlib `net/http.ServeMux` ≥1.22 method routing.
func (h *Health) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /alive", h.Alive)
	mux.HandleFunc("GET /ready", h.Ready)
	mux.HandleFunc("GET /health", h.Health)
}

// noStore sets Cache-Control: no-store + Pragma: no-cache so the
// reverse proxy / OutputCache cannot mask downstream failures from
// Kubernetes probes.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
}
