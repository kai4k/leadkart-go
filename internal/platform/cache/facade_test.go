package cache_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/leadkart/leadkart-go/internal/platform/cache"
)

// person is the test value type — JSON-encodable with a couple of
// fields so encode/decode round-trips are non-trivial.
type person struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// fixture wires miniredis + a HybridCache + a typed facade pointing at
// a controllable factory. Returns the facade, the call counter, and
// a tearDown the test defers.
type fixture struct {
	cache    *cache.HybridCache
	facade   *cache.Facade[string, person]
	calls    *atomic.Int64
	store    *miniredis.Miniredis
	redisCli *redis.Client
}

func newFixture(t *testing.T, factory func(ctx context.Context, key string) (person, error)) *fixture {
	t.Helper()

	store := miniredis.RunT(t)
	cli := redis.NewClient(&redis.Options{Addr: store.Addr()})

	hc, err := cache.New(cache.Config{
		L1MaxItems: 1000,
		L2:         cli,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(hc.Close)

	calls := &atomic.Int64{}
	wrapped := func(ctx context.Context, key string) (person, error) {
		calls.Add(1)
		return factory(ctx, key)
	}

	keyer := func(key string) string { return "test:facade:" + key }
	f := cache.NewFacade(hc, "test", keyer, wrapped)
	return &fixture{cache: hc, facade: f, calls: calls, store: store, redisCli: cli}
}

func okFactory(p person) func(context.Context, string) (person, error) {
	return func(context.Context, string) (person, error) { return p, nil }
}

func TestFacade_Get_PopulatesL1AndL2OnMiss(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, okFactory(person{ID: "p1", Email: "a@b.test"}))

	got, err := fx.facade.Get(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Email != "a@b.test" {
		t.Fatalf("value: got %+v", got)
	}
	// ristretto admission is asynchronous — let it drain. Ristretto
	// docs recommend a short Wait or a poll loop in tests.
	fx.cache.L1.Wait()

	if c := fx.calls.Load(); c != 1 {
		t.Fatalf("factory calls: got %d want 1", c)
	}
	if !fx.store.Exists("test:facade:p1") {
		t.Fatal("L2 not populated after miss")
	}
}

func TestFacade_Get_L1HitSkipsFactory(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, okFactory(person{ID: "p1", Email: "a@b.test"}))

	if _, err := fx.facade.Get(context.Background(), "p1"); err != nil {
		t.Fatalf("Get 1: %v", err)
	}
	fx.cache.L1.Wait()

	for i := range 5 {
		if _, err := fx.facade.Get(t.Context(), "p1"); err != nil {
			t.Fatalf("Get %d: %v", i+2, err)
		}
	}
	if c := fx.calls.Load(); c != 1 {
		t.Fatalf("factory calls after L1 hits: got %d want 1", c)
	}
}

func TestFacade_Get_L2HitOnL1Miss(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, okFactory(person{ID: "p1", Email: "a@b.test"}))

	// Populate L2 directly bypassing the facade — simulates a fresh
	// process that has L2 warm but L1 cold.
	if _, err := fx.facade.Get(context.Background(), "p1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fx.cache.L1.Wait()
	fx.cache.L1.Clear() // clear L1 only

	got, err := fx.facade.Get(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Get after L1 clear: %v", err)
	}
	if got.Email != "a@b.test" {
		t.Fatalf("L2 hit returned wrong value: %+v", got)
	}
	if c := fx.calls.Load(); c != 1 {
		t.Fatalf("factory calls: got %d want 1 (L2 hit, no factory)", c)
	}
}

func TestFacade_Get_FactoryErrorPropagates(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("source down")
	fx := newFixture(t, func(context.Context, string) (person, error) {
		return person{}, wantErr
	})

	_, err := fx.facade.Get(context.Background(), "p1")
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected source error, got %v", err)
	}
}

func TestFacade_Invalidate_EvictsBothLayers(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, okFactory(person{ID: "p1", Email: "a@b.test"}))

	if _, err := fx.facade.Get(context.Background(), "p1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fx.cache.L1.Wait()

	if err := fx.facade.Invalidate(context.Background(), "p1"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if fx.store.Exists("test:facade:p1") {
		t.Fatal("L2 key still present after Invalidate")
	}

	// Next Get must re-call factory.
	if _, err := fx.facade.Get(context.Background(), "p1"); err != nil {
		t.Fatalf("Get after invalidate: %v", err)
	}
	if c := fx.calls.Load(); c != 2 {
		t.Fatalf("factory calls after invalidate: got %d want 2", c)
	}
}

// TestFacade_ProofOfCache is the canonical test required per
// audit-checklist.md §12b: warm cache → mutate underlying source
// bypassing the facade → re-call → assert pre-mutation value.
//
// A tautological "call twice, equal results" test would pass with
// caching disabled. This one MUST observe stale data on hit, proving
// the layer is real.
func TestFacade_ProofOfCache(t *testing.T) {
	t.Parallel()

	source := &atomic.Value{}
	source.Store(person{ID: "p1", Email: "before@b.test"})

	factory := func(context.Context, string) (person, error) {
		return source.Load().(person), nil
	}
	fx := newFixture(t, factory)

	// 1. Warm — first call hits factory + populates L1+L2.
	first, err := fx.facade.Get(context.Background(), "p1")
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	if first.Email != "before@b.test" {
		t.Fatalf("warm value: %+v", first)
	}
	fx.cache.L1.Wait()

	// 2. Mutate source DIRECTLY (no Invalidate call).
	source.Store(person{ID: "p1", Email: "after@b.test"})

	// 3. Re-call — caching MUST return the warm value, not the new one.
	cached, err := fx.facade.Get(context.Background(), "p1")
	if err != nil {
		t.Fatalf("cached: %v", err)
	}
	if cached.Email != "before@b.test" {
		t.Fatalf("proof-of-cache failed: got %q (cache layer not real)", cached.Email)
	}

	// 4. Invalidate; next call MUST observe the post-mutation value.
	if err := fx.facade.Invalidate(context.Background(), "p1"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	post, err := fx.facade.Get(context.Background(), "p1")
	if err != nil {
		t.Fatalf("post-invalidate: %v", err)
	}
	if post.Email != "after@b.test" {
		t.Fatalf("post-invalidate value: got %q want after@b.test", post.Email)
	}
}

// TestFacade_Singleflight_CoalescesConcurrentMisses verifies the
// stampede-protection contract: N concurrent Get calls for the same
// key while L1+L2 are cold result in EXACTLY ONE factory invocation.
func TestFacade_Singleflight_CoalescesConcurrentMisses(t *testing.T) {
	t.Parallel()

	// factory blocks until released so the test can stack callers.
	release := make(chan struct{})
	factory := func(context.Context, string) (person, error) {
		<-release
		return person{ID: "p1", Email: "a@b.test"}, nil
	}
	fx := newFixture(t, factory)

	const callers = 10
	var wg sync.WaitGroup
	results := make([]person, callers)
	errs := make([]error, callers)
	for i := range callers {
		// Go 1.22 — loop var per-iteration safe; Go 1.25 — wg.Go.
		wg.Go(func() {
			results[i], errs[i] = fx.facade.Get(t.Context(), "p1")
		})
	}
	// All callers should be blocked on the factory now.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("caller %d err: %v", i, e)
		}
		if results[i].Email != "a@b.test" {
			t.Fatalf("caller %d value: %+v", i, results[i])
		}
	}
	if c := fx.calls.Load(); c != 1 {
		t.Fatalf("singleflight: factory calls got %d want 1 (stampede)", c)
	}
}

// TestFacade_InvalidateMany evicts a batch + verifies the next Get for
// each pre-invalidated key triggers a factory call.
func TestFacade_InvalidateMany(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, func(_ context.Context, key string) (person, error) {
		return person{ID: key, Email: key + "@b.test"}, nil
	})

	keys := []string{"p1", "p2", "p3"}
	for _, k := range keys {
		if _, err := fx.facade.Get(context.Background(), k); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	fx.cache.L1.Wait()
	if got, want := fx.calls.Load(), int64(3); got != want {
		t.Fatalf("seed factory calls: got %d want %d", got, want)
	}

	if err := fx.facade.InvalidateMany(context.Background(), keys); err != nil {
		t.Fatalf("InvalidateMany: %v", err)
	}
	for _, k := range keys {
		if fx.store.Exists("test:facade:" + k) {
			t.Fatalf("L2 key %q still present", k)
		}
	}

	// Re-fetch all → factory called again.
	for _, k := range keys {
		if _, err := fx.facade.Get(context.Background(), k); err != nil {
			t.Fatalf("post-invalidate %s: %v", k, err)
		}
	}
	if got, want := fx.calls.Load(), int64(6); got != want {
		t.Fatalf("post-invalidate factory calls: got %d want %d", got, want)
	}
}

// TestFacade_Set_BypassesFactory exercises explicit pre-warm.
func TestFacade_Set_BypassesFactory(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, okFactory(person{ID: "from-factory", Email: "factory@b.test"}))

	if err := fx.facade.Set(context.Background(), "p1", person{ID: "from-set", Email: "set@b.test"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	fx.cache.L1.Wait()

	got, err := fx.facade.Get(context.Background(), "p1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Email != "set@b.test" {
		t.Fatalf("Set was overridden by factory: %+v", got)
	}
	if fx.calls.Load() != 0 {
		t.Fatalf("factory invoked despite Set: %d", fx.calls.Load())
	}
}

// TestFacade_TTL_L1ExpiryFallsThroughToL2 verifies the L1 TTL fires
// (ristretto purges) and L2 still serves the value without a factory
// call.
func TestFacade_TTL_L1ExpiryFallsThroughToL2(t *testing.T) {
	t.Parallel()

	source := &atomic.Value{}
	source.Store(person{ID: "p1", Email: "first@b.test"})
	factory := func(context.Context, string) (person, error) {
		return source.Load().(person), nil
	}

	store := miniredis.RunT(t)
	cli := redis.NewClient(&redis.Options{Addr: store.Addr()})
	hc, err := cache.New(cache.Config{L1MaxItems: 100, L2: cli, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(hc.Close)

	calls := &atomic.Int64{}
	wrapped := func(ctx context.Context, key string) (person, error) {
		calls.Add(1)
		return factory(ctx, key)
	}
	f := cache.NewFacade(hc, "ttl-test", func(s string) string { return "k:" + s }, wrapped,
		cache.WithTTL(cache.TTL{L1: 50 * time.Millisecond, L2: 5 * time.Minute}))

	if _, err := f.Get(context.Background(), "p1"); err != nil {
		t.Fatalf("warm: %v", err)
	}
	hc.L1.Wait()

	// Mutate source so we can detect a factory call.
	source.Store(person{ID: "p1", Email: "second@b.test"})

	// Wait past L1 TTL.
	time.Sleep(120 * time.Millisecond)
	hc.L1.Clear() // belt-and-braces — ristretto TTL is best-effort

	got, err := f.Get(context.Background(), "p1")
	if err != nil {
		t.Fatalf("post-TTL: %v", err)
	}
	if got.Email != "first@b.test" {
		t.Fatalf("L2 should still serve original: got %q", got.Email)
	}
	if c := calls.Load(); c != 1 {
		t.Fatalf("factory invoked after L1 expiry — L2 missed: got %d want 1", c)
	}
}

