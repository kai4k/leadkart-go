package obs_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/platform/obs"
)

func okChecker(name string) obs.HealthChecker {
	return obs.HealthCheckerFunc{N: name, Fn: func(context.Context) error { return nil }}
}

func failChecker(name, msg string) obs.HealthChecker {
	return obs.HealthCheckerFunc{N: name, Fn: func(context.Context) error { return errors.New(msg) }}
}

func slowChecker(name string, d time.Duration) obs.HealthChecker {
	return obs.HealthCheckerFunc{N: name, Fn: func(ctx context.Context) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
}

func TestAlive_Always200(t *testing.T) {
	t.Parallel()
	h := obs.NewHealth(nil, 0)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alive", nil)
	h.Alive(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	if rec.Body.String() != "ok\n" {
		t.Fatalf("body: got %q want ok\\n", rec.Body.String())
	}
}

func TestReady_AllOK_Returns200(t *testing.T) {
	t.Parallel()
	h := obs.NewHealth([]obs.HealthChecker{okChecker("postgres"), okChecker("redis")}, 0)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	h.Ready(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rec.Code)
	}
	var report map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report["postgres"] != "ok" || report["redis"] != "ok" {
		t.Fatalf("expected all ok, got %+v", report)
	}
}

func TestReady_AnyFailing_Returns503(t *testing.T) {
	t.Parallel()
	h := obs.NewHealth([]obs.HealthChecker{
		okChecker("postgres"),
		failChecker("redis", "connection refused"),
	}, 0)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	h.Ready(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503", rec.Code)
	}
	var report map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if report["postgres"] != "ok" {
		t.Fatalf("postgres: got %q", report["postgres"])
	}
	if report["redis"] == "ok" {
		t.Fatal("redis reported ok despite failing checker")
	}
}

func TestHealth_AlwaysReturns200(t *testing.T) {
	t.Parallel()
	h := obs.NewHealth([]obs.HealthChecker{
		okChecker("postgres"),
		failChecker("redis", "down"),
	}, 0)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	h.Health(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/health is diagnostics — must always 200; got %d", rec.Code)
	}
	var report map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &report)
	if report["redis"] == "ok" {
		t.Fatal("/health should still surface failed checker status")
	}
}

func TestReady_TimeoutEnforced(t *testing.T) {
	t.Parallel()
	h := obs.NewHealth([]obs.HealthChecker{
		slowChecker("slow", 200*time.Millisecond),
	}, 50*time.Millisecond)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	h.Ready(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on timeout, got %d", rec.Code)
	}
}

func TestProbes_NeverCached(t *testing.T) {
	t.Parallel()
	h := obs.NewHealth(nil, 0)
	for _, path := range []string{"/alive", "/ready", "/health"} {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			switch path {
			case "/alive":
				h.Alive(rec, req)
			case "/ready":
				h.Ready(rec, req)
			case "/health":
				h.Health(rec, req)
			}
			cc := rec.Header().Get("Cache-Control")
			if cc != "no-store, no-cache, must-revalidate" {
				t.Fatalf("%s Cache-Control: got %q", path, cc)
			}
		})
	}
}

func TestRegister_PostConstruction(t *testing.T) {
	t.Parallel()
	h := obs.NewHealth(nil, 0)
	h.Register(okChecker("late"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ready", nil)
	h.Ready(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with late-registered checker, got %d", rec.Code)
	}
	var report map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &report)
	if report["late"] != "ok" {
		t.Fatalf("late checker not reported: %+v", report)
	}
}
