package query_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/leadkart/leadkart-go/internal/identity/app/query"
)

// fakePlatformStatsReader is an inline minimal [query.PlatformStatsReader]
// that captures arguments and returns canned shapes or errors.
type fakePlatformStatsReader struct {
	base       query.PlatformStatsBase
	baseErr    error
	deltas     query.PlatformStatsDeltaCounts
	deltasErr  error
	deltaArgs  []string
	baseCalls  int
	deltaCalls int
}

func (f *fakePlatformStatsReader) Base(_ context.Context) (query.PlatformStatsBase, error) {
	f.baseCalls++
	if f.baseErr != nil {
		return query.PlatformStatsBase{}, f.baseErr
	}
	return f.base, nil
}

func (f *fakePlatformStatsReader) Deltas(_ context.Context, intervalLabel string) (query.PlatformStatsDeltaCounts, error) {
	f.deltaCalls++
	f.deltaArgs = append(f.deltaArgs, intervalLabel)
	if f.deltasErr != nil {
		return query.PlatformStatsDeltaCounts{}, f.deltasErr
	}
	return f.deltas, nil
}

var _ query.PlatformStatsReader = (*fakePlatformStatsReader)(nil)

// ----- ParseDeltaWindow ----------------------------------------------------

func TestParseDeltaWindow_ClosedSet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		wantDur time.Duration
		wantLbl string
		wantErr bool
	}{
		{"", 0, "", false},
		{"24h", 24 * time.Hour, "24h", false},
		{"7d", 7 * 24 * time.Hour, "7d", false},
		{"30d", 30 * 24 * time.Hour, "30d", false},
		{"15m", 0, "", true},
		{"forever", 0, "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			dur, lbl, err := query.ParseDeltaWindow(c.in)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error for %q", c.in)
				}
				if !errors.Is(err, query.ErrPlatformStatsInvalidWindow) {
					t.Errorf("err = %v, want ErrPlatformStatsInvalidWindow", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if dur != c.wantDur {
				t.Errorf("dur = %v, want %v", dur, c.wantDur)
			}
			if lbl != c.wantLbl {
				t.Errorf("lbl = %q, want %q", lbl, c.wantLbl)
			}
		})
	}
}

// ----- PlatformStatsHandler -----------------------------------------------

func TestNewPlatformStatsHandler_PanicsOnNilReader(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewPlatformStatsHandler(nil) // arch-test:ignore-err - test fixture setup
}

func TestPlatformStats_BaseError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("base boom")
	r := &fakePlatformStatsReader{baseErr: sentinel}
	h := query.NewPlatformStatsHandler(r)
	_, err := h.Handle(t.Context(), query.PlatformStatsQuery{})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestPlatformStats_NoWindow_SkipsDeltas(t *testing.T) {
	t.Parallel()
	r := &fakePlatformStatsReader{base: query.PlatformStatsBase{TenantsTotal: 10, TenantsActive: 7, TenantsSuspended: 1, PersonsTotal: 50, MembershipsActive: 40}}
	h := query.NewPlatformStatsHandler(r)
	got, err := h.Handle(t.Context(), query.PlatformStatsQuery{})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	want := query.PlatformStatsView{
		TenantsTotal: 10, TenantsActive: 7, TenantsSuspended: 1,
		PersonsTotal: 50, MembershipsActive: 40, Deltas: nil,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("view (-want +got)\n%s", diff)
	}
	if r.deltaCalls != 0 {
		t.Errorf("deltaCalls = %d, want 0", r.deltaCalls)
	}
}

func TestPlatformStats_WithWindow_CallsDeltasAndPopulates(t *testing.T) {
	t.Parallel()
	r := &fakePlatformStatsReader{
		base:   query.PlatformStatsBase{TenantsTotal: 100, TenantsActive: 80, TenantsSuspended: 5, PersonsTotal: 500, MembershipsActive: 400},
		deltas: query.PlatformStatsDeltaCounts{TenantsTotal: 3, TenantsActive: 2, PersonsTotal: 12, MembershipsActive: 9},
	}
	h := query.NewPlatformStatsHandler(r)
	got, err := h.Handle(t.Context(), query.PlatformStatsQuery{Window: 24 * time.Hour, WindowLabel: "24h"})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.Deltas == nil {
		t.Fatal("Deltas nil")
	}
	want := query.PlatformStatsDeltas{
		Window: "24h", TenantsTotal: 3, TenantsActive: 2, PersonsTotal: 12, MembershipsActive: 9,
	}
	if diff := cmp.Diff(want, *got.Deltas); diff != "" {
		t.Errorf("deltas (-want +got)\n%s", diff)
	}
	if r.deltaCalls != 1 {
		t.Errorf("deltaCalls = %d, want 1", r.deltaCalls)
	}
	if r.deltaArgs[0] != "24 hours" {
		t.Errorf("deltaArgs[0] = %q, want %q", r.deltaArgs[0], "24 hours")
	}
}

func TestPlatformStats_WindowLabelMappings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		label   string
		wantArg string
	}{
		{"24h", "24 hours"},
		{"7d", "7 days"},
		{"30d", "30 days"},
		{"unknown", "unknown"}, // default path — label passes through
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			t.Parallel()
			r := &fakePlatformStatsReader{}
			h := query.NewPlatformStatsHandler(r)
			if _, err := h.Handle(t.Context(), query.PlatformStatsQuery{Window: time.Hour, WindowLabel: c.label}); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if r.deltaArgs[0] != c.wantArg {
				t.Errorf("deltaArgs[0] = %q, want %q", r.deltaArgs[0], c.wantArg)
			}
		})
	}
}

func TestPlatformStats_DeltasError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("deltas boom")
	r := &fakePlatformStatsReader{deltasErr: sentinel}
	h := query.NewPlatformStatsHandler(r)
	_, err := h.Handle(t.Context(), query.PlatformStatsQuery{Window: time.Hour, WindowLabel: "24h"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// ----- CachedPlatformStatsHandler -----------------------------------------

func TestNewCachedPlatformStatsHandler_PanicsOnNilCache(t *testing.T) {
	t.Parallel()
	inner := query.NewPlatformStatsHandler(&fakePlatformStatsReader{})
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewCachedPlatformStatsHandler(inner, nil) // arch-test:ignore-err - test fixture setup
}

func TestCachedPlatformStats_HappyPath_CachesAndReturns(t *testing.T) {
	t.Parallel()
	r := &fakePlatformStatsReader{base: query.PlatformStatsBase{TenantsTotal: 5}}
	inner := query.NewPlatformStatsHandler(r)
	hc := newHybridCacheForTest(t)
	h := query.NewCachedPlatformStatsHandler(inner, hc)
	got, err := h.Handle(t.Context(), query.PlatformStatsQuery{WindowLabel: ""})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.TenantsTotal != 5 {
		t.Errorf("TenantsTotal = %d", got.TenantsTotal)
	}
	hc.L1.Wait()
	// Second call — cache hit; base() must not be called again.
	if _, err := h.Handle(t.Context(), query.PlatformStatsQuery{WindowLabel: ""}); err != nil {
		t.Fatalf("Handle 2: %v", err)
	}
	if r.baseCalls != 1 {
		t.Errorf("baseCalls = %d, want 1 (cache hit)", r.baseCalls)
	}
}

func TestCachedPlatformStats_InvalidWindowKey_FactoryError(t *testing.T) {
	t.Parallel()
	// Unknown label routes through the cache factory → ParseDeltaWindow → ErrPlatformStatsInvalidWindow.
	r := &fakePlatformStatsReader{}
	inner := query.NewPlatformStatsHandler(r)
	hc := newHybridCacheForTest(t)
	h := query.NewCachedPlatformStatsHandler(inner, hc)
	_, err := h.Handle(t.Context(), query.PlatformStatsQuery{WindowLabel: "junk"})
	if !errors.Is(err, query.ErrPlatformStatsInvalidWindow) {
		t.Fatalf("err = %v, want ErrPlatformStatsInvalidWindow", err)
	}
}
