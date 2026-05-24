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

	"github.com/leadkart/leadkart-go/internal/common/cache"
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
	t.Cleanup(func() { _ = cli.Close() })

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

	got, err := fx.facade.Get(t.Context(), "p1")
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

	if _, err := fx.facade.Get(t.Context(), "p1"); err != nil {
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
	if _, err := fx.facade.Get(t.Context(), "p1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fx.cache.L1.Wait()
	fx.cache.L1.Clear() // clear L1 only

	got, err := fx.facade.Get(t.Context(), "p1")
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

	_, err := fx.facade.Get(t.Context(), "p1")
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected source error, got %v", err)
	}
}

func TestFacade_Invalidate_EvictsBothLayers(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, okFactory(person{ID: "p1", Email: "a@b.test"}))

	if _, err := fx.facade.Get(t.Context(), "p1"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	fx.cache.L1.Wait()

	if err := fx.facade.Invalidate(t.Context(), "p1"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if fx.store.Exists("test:facade:p1") {
		t.Fatal("L2 key still present after Invalidate")
	}

	// Next Get must re-call factory.
	if _, err := fx.facade.Get(t.Context(), "p1"); err != nil {
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
	first, err := fx.facade.Get(t.Context(), "p1")
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
	cached, err := fx.facade.Get(t.Context(), "p1")
	if err != nil {
		t.Fatalf("cached: %v", err)
	}
	if cached.Email != "before@b.test" {
		t.Fatalf("proof-of-cache failed: got %q (cache layer not real)", cached.Email)
	}

	// 4. Invalidate; next call MUST observe the post-mutation value.
	if err := fx.facade.Invalidate(t.Context(), "p1"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	post, err := fx.facade.Get(t.Context(), "p1")
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
	// The singleflight contract is: concurrent Gets for the same key
	// must coalesce into ONE factory invocation. The factory parks on
	// `release`; the test releases AFTER giving the herd time to all
	// enter the singleflight group. Singleflight doesn't expose its
	// in-flight count, so we use the canonical "wait long enough" sync
	// — bounded by the test framework's hung-test timeout above.
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
	startGate := make(chan struct{})
	var startWG sync.WaitGroup
	startWG.Add(callers)
	for i := range callers {
		// Go 1.22 — loop var per-iteration safe; Go 1.25 — wg.Go.
		wg.Go(func() {
			startWG.Done()
			<-startGate
			results[i], errs[i] = fx.facade.Get(t.Context(), "p1")
		})
	}
	// Park the herd at the gate, then release as one.
	startWG.Wait()
	close(startGate)
	// Give every herd member time to enter the singleflight group.
	// 500ms is a CI-safety bound — well above the OS scheduler-jitter
	// floor on shared runners; far below the test framework's hung-test
	// timeout. Singleflight membership has no observable signal; the
	// bounded wait is the canonical sync.
	time.Sleep(500 * time.Millisecond) // arch-test:wait-justified — bounded sync for unobservable singleflight membership
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
		if _, err := fx.facade.Get(t.Context(), k); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	fx.cache.L1.Wait()
	if got, want := fx.calls.Load(), int64(3); got != want {
		t.Fatalf("seed factory calls: got %d want %d", got, want)
	}

	if err := fx.facade.InvalidateMany(t.Context(), keys); err != nil {
		t.Fatalf("InvalidateMany: %v", err)
	}
	for _, k := range keys {
		if fx.store.Exists("test:facade:" + k) {
			t.Fatalf("L2 key %q still present", k)
		}
	}

	// Re-fetch all → factory called again.
	for _, k := range keys {
		if _, err := fx.facade.Get(t.Context(), k); err != nil {
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

	if err := fx.facade.Set(t.Context(), "p1", person{ID: "from-set", Email: "set@b.test"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	fx.cache.L1.Wait()

	got, err := fx.facade.Get(t.Context(), "p1")
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
	t.Cleanup(func() { _ = cli.Close() })
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

	if _, err := f.Get(t.Context(), "p1"); err != nil {
		t.Fatalf("warm: %v", err)
	}
	hc.L1.Wait()

	// Mutate source so we can detect a factory call.
	source.Store(person{ID: "p1", Email: "second@b.test"})

	// Wait past L1 TTL.
	time.Sleep(120 * time.Millisecond) // arch-test:wait-justified — must exceed 50ms L1 TTL to assert L2 fallback
	hc.L1.Clear() // belt-and-braces — ristretto TTL is best-effort

	got, err := f.Get(t.Context(), "p1")
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


// TestFacade_Invalidate_DuringFactory_DoesNotRePoisonCache pins the
// stale-write fence: a factory call that captures the source BEFORE
// an Invalidate must NOT commit its result to L1+L2 after the
// Invalidate clears them. Without the per-facade generation counter,
// the factory's post-clear write would re-poison the cache with the
// pre-mutation value — the bug audit-checklist.md §12b cache-facade
// canon protects against.
func TestFacade_Invalidate_DuringFactory_DoesNotRePoisonCache(t *testing.T) {
	t.Parallel()

	store := miniredis.RunT(t)
	cli := redis.NewClient(&redis.Options{Addr: store.Addr()})
	t.Cleanup(func() { _ = cli.Close() })
	hc, err := cache.New(cache.Config{
		L1MaxItems: 1000,
		L2:         cli,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(hc.Close)

	// First-call signalling is one-shot: subsequent factory calls run
	// straight through. The blocking dance only matters for the in-
	// flight Get the test races against an Invalidate.
	source := atomic.Pointer[person]{}
	source.Store(&person{ID: "p1", Email: "stale@source.test"})
	factoryEntered := make(chan struct{})
	releaseFactory := make(chan struct{})
	var firstCallOnce sync.Once

	factory := func(context.Context, string) (person, error) {
		v := *source.Load()
		firstCallOnce.Do(func() {
			close(factoryEntered)
			<-releaseFactory
		})
		return v, nil
	}
	keyer := func(s string) string { return "race:" + s }
	f := cache.NewFacade(hc, "race-facade", keyer, factory)

	// Background Get — captures genBefore + source view, then blocks
	// on releaseFactory before attempting to commit.
	type getResult struct {
		v   person
		err error
	}
	resultCh := make(chan getResult, 1)
	go func() {
		v, err := f.Get(t.Context(), "p1")
		resultCh <- getResult{v: v, err: err}
	}()

	<-factoryEntered

	// Mutate source + Invalidate. This bumps gen — the Get's pending
	// factory MUST detect this and skip its cache write.
	source.Store(&person{ID: "p1", Email: "fresh@source.test"})
	if err := f.Invalidate(t.Context(), "p1"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	close(releaseFactory)
	got := <-resultCh
	if got.err != nil {
		t.Fatalf("Get during invalidate: %v", got.err)
	}
	// The factory's stale snapshot is still returned to its caller
	// (they asked "what's the source-of-truth?", and at the time we
	// read it that WAS this value). The cache, however, must NOT be
	// poisoned with this value after Invalidate.
	if got.v.Email != "stale@source.test" {
		t.Fatalf("Get factory result: got %q want stale snapshot %q", got.v.Email, "stale@source.test")
	}

	// Drain ristretto's async buffer so any race-window L1 writes are
	// applied (or skipped, if the gen check caught them).
	hc.L1.Wait()

	// Crucial assertion: NEXT Get must not return the stale value.
	next, err := f.Get(t.Context(), "p1")
	if err != nil {
		t.Fatalf("post-invalidate Get: %v", err)
	}
	if next.Email == "stale@source.test" {
		t.Fatalf("cache served stale value after Invalidate during factory: got %q", next.Email)
	}
	if next.Email != "fresh@source.test" {
		t.Fatalf("post-invalidate Get: got %q want %q", next.Email, "fresh@source.test")
	}
}

// TestFacade_Invalidate_AfterMutation_NextGetSeesFreshValue is the
// canonical sequential proof-of-cache-invalidation: warm cache,
// mutate source, Invalidate, Get → fresh value. Without the
// ristretto L1 Wait drain in Invalidate, the next Get could still
// hit the stale L1 entry that hasn't been processed yet.
func TestFacade_Invalidate_AfterMutation_NextGetSeesFreshValue(t *testing.T) {
	t.Parallel()

	store := miniredis.RunT(t)
	cli := redis.NewClient(&redis.Options{Addr: store.Addr()})
	t.Cleanup(func() { _ = cli.Close() })
	hc, err := cache.New(cache.Config{
		L1MaxItems: 1000,
		L2:         cli,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(hc.Close)

	source := atomic.Pointer[person]{}
	source.Store(&person{ID: "p1", Email: "v1@source.test"})
	factory := func(_ context.Context, _ string) (person, error) {
		return *source.Load(), nil
	}
	keyer := func(s string) string { return "seq:" + s }
	f := cache.NewFacade(hc, "seq-facade", keyer, factory)

	// Warm.
	if got, err := f.Get(t.Context(), "p1"); err != nil || got.Email != "v1@source.test" {
		t.Fatalf("warm: got %+v err=%v", got, err)
	}

	// Mutate source bypassing the cache.
	source.Store(&person{ID: "p1", Email: "v2@source.test"})

	if err := f.Invalidate(t.Context(), "p1"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	got, err := f.Get(t.Context(), "p1")
	if err != nil {
		t.Fatalf("post-invalidate Get: %v", err)
	}
	if got.Email != "v2@source.test" {
		t.Fatalf("post-invalidate Get returned stale value %q (Wait-drain or gen fence broken)", got.Email)
	}
}

// TestFacade_Set_FencesInFlightFactory mirrors the Invalidate race
// proof, but for explicit Set: a factory call that captured an older
// source view must not overwrite a Set that landed during its run.
func TestFacade_Set_FencesInFlightFactory(t *testing.T) {
	t.Parallel()

	store := miniredis.RunT(t)
	cli := redis.NewClient(&redis.Options{Addr: store.Addr()})
	t.Cleanup(func() { _ = cli.Close() })
	hc, err := cache.New(cache.Config{
		L1MaxItems: 1000,
		L2:         cli,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(hc.Close)

	factoryEntered := make(chan struct{})
	releaseFactory := make(chan struct{})
	var once sync.Once
	factory := func(context.Context, string) (person, error) {
		once.Do(func() {
			close(factoryEntered)
			<-releaseFactory
		})
		return person{ID: "p1", Email: "factory-stale@source.test"}, nil
	}
	keyer := func(s string) string { return "set-race:" + s }
	f := cache.NewFacade(hc, "set-race-facade", keyer, factory)

	type getResult struct {
		v   person
		err error
	}
	resultCh := make(chan getResult, 1)
	go func() {
		v, err := f.Get(t.Context(), "p1")
		resultCh <- getResult{v: v, err: err}
	}()

	<-factoryEntered
	if err := f.Set(t.Context(), "p1", person{ID: "p1", Email: "explicit@source.test"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	close(releaseFactory)
	<-resultCh

	hc.L1.Wait()
	got, err := f.Get(t.Context(), "p1")
	if err != nil {
		t.Fatalf("post-Set Get: %v", err)
	}
	if got.Email != "explicit@source.test" {
		t.Fatalf("Set was overwritten by stale factory: got %q want %q",
			got.Email, "explicit@source.test")
	}
}
