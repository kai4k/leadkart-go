package adapters_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/leadkart/leadkart-go/internal/common/cache"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// fakePersonReader is a controllable in-memory implementation of
// [adapters.PersonStampReader]. Tests mutate the stamp via the
// SetStamp method to simulate password-rotation while bypassing the
// cache — the proof-of-cache test below relies on this to prove the
// facade actually caches (not just calls the factory twice and gets
// the same value).
type fakePersonReader struct {
	id      person.ID
	stamp   *atomic.Value // string
	calls   *atomic.Int64
	missing bool
}

func newFakeReader(id person.ID, initialStamp string) *fakePersonReader {
	stamp := &atomic.Value{}
	stamp.Store(initialStamp)
	return &fakePersonReader{id: id, stamp: stamp, calls: &atomic.Int64{}}
}

func (f *fakePersonReader) GetByID(_ context.Context, id person.ID) (*person.Person, error) {
	f.calls.Add(1)
	if f.missing || id != f.id {
		return nil, person.ErrNotFound
	}
	stampStr := f.stamp.Load().(string)
	stamp, err := person.SecurityStampFromString(stampStr)
	if err != nil {
		return nil, err
	}
	addr, _ := email.New("alice@flow.test")
	pwd, _ := person.NewPasswordHash("$argon2id$v=19$m=19456,t=2,p=1$c2FsdHNhbHQ$aGFzaGhhc2g")
	return person.UnmarshalFromDB(person.Snapshot{
		ID:            id,
		Email:         addr,
		FirstName:     "Alice",
		LastName:      "Test",
		PasswordHash:  pwd,
		SecurityStamp: stamp,
		IsActive:      true,
	}), nil
}

func (f *fakePersonReader) SetStamp(s string) { f.stamp.Store(s) }

// hybridFixture wires miniredis + HybridCache + fakeReader for the
// SecurityStampCache tests. Returns the cache + reader so tests can
// assert call-counts + mutate-bypass-cache to prove caching is real.
type hybridFixture struct {
	cache  *cache.HybridCache
	stamps *adapters.SecurityStampCache
	reader *fakePersonReader
}

func newHybridFixture(t *testing.T, personID person.ID, initialStamp string) *hybridFixture {
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
	reader := newFakeReader(personID, initialStamp)
	return &hybridFixture{
		cache:  hc,
		stamps: adapters.NewSecurityStampCache(hc, reader),
		reader: reader,
	}
}

const testStamp1 = "00000000-0000-7000-8000-000000000001"
const testStamp2 = "00000000-0000-7000-8000-000000000002"

// TestSecurityStampCache_Get_PopulatesFromReaderOnMiss is the basic
// read-through cache assertion: miss triggers a single reader call,
// the value comes back, the call counter ticked once.
func TestSecurityStampCache_Get_PopulatesFromReaderOnMiss(t *testing.T) {
	t.Parallel()
	pid := person.ID("01999999-1111-7000-8000-000000000001")
	fx := newHybridFixture(t, pid, testStamp1)

	got, err := fx.stamps.Get(t.Context(), pid)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != testStamp1 {
		t.Fatalf("stamp: got %q want %q", got, testStamp1)
	}
	if calls := fx.reader.calls.Load(); calls != 1 {
		t.Fatalf("reader calls: got %d want 1", calls)
	}
}

// TestSecurityStampCache_Get_AfterWarmup_MutatedSourceReturnsCachedValue
// is the canonical proof-of-cache test per `audit-checklist.md §12b`:
// warm cache → mutate underlying source bypassing the facade →
// re-call → assert PRE-mutation value still returned. Tautological
// "call twice, assert equal" passes even with cache disabled — this
// proves the cache layer is actually reading from cache, not the
// reader.
func TestSecurityStampCache_Get_AfterWarmup_MutatedSourceReturnsCachedValue(t *testing.T) {
	t.Parallel()
	pid := person.ID("01999999-2222-7000-8000-000000000002")
	fx := newHybridFixture(t, pid, testStamp1)

	// Warm cache (reader called once).
	if _, err := fx.stamps.Get(t.Context(), pid); err != nil {
		t.Fatalf("Get warmup: %v", err)
	}
	// Mutate source bypassing the facade — this would force any
	// non-cached implementation to return the new value on next read.
	fx.reader.SetStamp(testStamp2)

	// Allow ristretto admission to settle (set is async). The L2 write
	// completes synchronously inside facade.Get so even if L1 misses,
	// L2 is the safety net here.
	got, err := fx.stamps.Get(t.Context(), pid)
	if err != nil {
		t.Fatalf("Get post-mutation: %v", err)
	}
	if got != testStamp1 {
		t.Fatalf("expected cached value %q, got %q (cache layer not actually caching!)",
			testStamp1, got)
	}
}

// TestSecurityStampCache_Invalidate_BypassesCache asserts that after
// Invalidate is called, the next Get re-fetches from the reader and
// surfaces the post-mutation value. Without this, cascade subscribers
// would have no way to close the eventual-consistency window faster
// than the 30s TTL.
func TestSecurityStampCache_Invalidate_BypassesCache(t *testing.T) {
	t.Parallel()
	pid := person.ID("01999999-3333-7000-8000-000000000003")
	fx := newHybridFixture(t, pid, testStamp1)

	if _, err := fx.stamps.Get(t.Context(), pid); err != nil {
		t.Fatalf("Get warmup: %v", err)
	}
	fx.reader.SetStamp(testStamp2)
	if err := fx.stamps.Invalidate(t.Context(), pid); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	got, err := fx.stamps.Get(t.Context(), pid)
	if err != nil {
		t.Fatalf("Get post-invalidate: %v", err)
	}
	if got != testStamp2 {
		t.Fatalf("after Invalidate: got %q want %q (cache still serving stale)",
			got, testStamp2)
	}
}

// TestSecurityStampValidator_IsFresh_TrueOnMatch is the happy-path
// validator assertion.
func TestSecurityStampValidator_IsFresh_TrueOnMatch(t *testing.T) {
	t.Parallel()
	pid := person.ID("01999999-4444-7000-8000-000000000004")
	fx := newHybridFixture(t, pid, testStamp1)
	v := adapters.NewSecurityStampValidator(fx.stamps)

	fresh, err := v.IsFresh(t.Context(), pid.String(), testStamp1)
	if err != nil {
		t.Fatalf("IsFresh: %v", err)
	}
	if !fresh {
		t.Fatal("IsFresh: got false, want true")
	}
}

// TestSecurityStampValidator_IsFresh_FalseOnStale asserts the staleness
// detection — caller treats false as 401 per security.md.
func TestSecurityStampValidator_IsFresh_FalseOnStale(t *testing.T) {
	t.Parallel()
	pid := person.ID("01999999-5555-7000-8000-000000000005")
	fx := newHybridFixture(t, pid, testStamp1)
	v := adapters.NewSecurityStampValidator(fx.stamps)

	fresh, err := v.IsFresh(t.Context(), pid.String(), testStamp2)
	if err != nil {
		t.Fatalf("IsFresh: %v", err)
	}
	if fresh {
		t.Fatal("IsFresh on stale claim: got true, want false")
	}
}

// TestSecurityStampValidator_IsFresh_EmptyPersonID_FalseNoLookup
// asserts the early-return for malformed claims (defense-in-depth
// against an attacker stripping `sub` from the JWT body).
func TestSecurityStampValidator_IsFresh_EmptyPersonID_FalseNoLookup(t *testing.T) {
	t.Parallel()
	pid := person.ID("01999999-6666-7000-8000-000000000006")
	fx := newHybridFixture(t, pid, testStamp1)
	v := adapters.NewSecurityStampValidator(fx.stamps)

	fresh, err := v.IsFresh(t.Context(), "", testStamp1)
	if err != nil {
		t.Fatalf("IsFresh on empty: %v", err)
	}
	if fresh {
		t.Fatal("IsFresh on empty PersonID: got true, want false")
	}
	if calls := fx.reader.calls.Load(); calls != 0 {
		t.Fatalf("reader calls on empty: got %d want 0 (early return broken)", calls)
	}
}

// TestSecurityStampValidator_IsFresh_RepoError_PropagatesAsError
// asserts that lookup failures (Person not found, DB error) surface as
// errors rather than silently masking as "stale". The middleware
// translates either to 401 — but operators care which branch fired.
func TestSecurityStampValidator_IsFresh_RepoError_PropagatesAsError(t *testing.T) {
	t.Parallel()
	pid := person.ID("01999999-7777-7000-8000-000000000007")
	fx := newHybridFixture(t, pid, testStamp1)
	fx.reader.missing = true
	v := adapters.NewSecurityStampValidator(fx.stamps)

	_, err := v.IsFresh(t.Context(), pid.String(), testStamp1)
	if err == nil {
		t.Fatal("IsFresh with missing person: got nil err, want propagated error")
	}
	if !errors.Is(err, person.ErrNotFound) {
		t.Fatalf("IsFresh err: got %v, want wrapping person.ErrNotFound", err)
	}
}
