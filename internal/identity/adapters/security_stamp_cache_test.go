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

// fakePersonReader is a controllable [adapters.PersonStampReader].
// SetStamp simulates password rotation bypassing the cache — needed
// for the proof-of-cache test.
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

// hybridFixture wires miniredis + HybridCache + fakeReader for tests.
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

// TestSecurityStampCache_Get_PopulatesFromReaderOnMiss verifies miss
// triggers exactly one reader call and returns the correct value.
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
// is the proof-of-cache test: warm → mutate source bypassing facade →
// re-call → assert the PRE-mutation value is returned.
// A "call twice, assert equal" test passes even with no cache; this fails if
// the facade doesn't actually cache.
func TestSecurityStampCache_Get_AfterWarmup_MutatedSourceReturnsCachedValue(t *testing.T) {
	t.Parallel()
	pid := person.ID("01999999-2222-7000-8000-000000000002")
	fx := newHybridFixture(t, pid, testStamp1)

	// Warm cache.
	if _, err := fx.stamps.Get(t.Context(), pid); err != nil {
		t.Fatalf("Get warmup: %v", err)
	}
	// Mutate source bypassing the facade.
	fx.reader.SetStamp(testStamp2)

	// L2 write completes synchronously; L2 is the safety net even if L1 misses.
	got, err := fx.stamps.Get(t.Context(), pid)
	if err != nil {
		t.Fatalf("Get post-mutation: %v", err)
	}
	if got != testStamp1 {
		t.Fatalf("expected cached value %q, got %q (cache layer not actually caching!)",
			testStamp1, got)
	}
}

// TestSecurityStampCache_Invalidate_BypassesCache verifies that after
// Invalidate, the next Get re-fetches and returns the post-mutation value.
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

// TestSecurityStampValidator_IsFresh_TrueOnMatch verifies the happy path.
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

// TestSecurityStampValidator_IsFresh_FalseOnStale verifies stale claim
// returns false (caller treats as 401).
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

// TestSecurityStampValidator_IsFresh_EmptyPersonID_FalseNoLookup verifies
// early-return for empty sub — defense-in-depth against stripped JWT claims.
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

// TestSecurityStampValidator_IsFresh_RepoError_PropagatesAsError verifies
// that lookup failures propagate as errors rather than being masked as stale.
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
