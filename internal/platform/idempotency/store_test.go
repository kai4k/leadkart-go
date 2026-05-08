package idempotency_test

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/platform/idempotency"
)

const testCaller = "tenant:019df708-f642-7f66-b73b-c7919f2447cb"

func hashOf(s string) [32]byte { return sha256.Sum256([]byte(s)) }

func TestInMemoryStore_PutGet_RoundTrip(t *testing.T) {
	t.Parallel()
	store := idempotency.NewInMemoryStore(nil)
	key := uuid.New()
	body := hashOf("body-1")

	rec := idempotency.Record{
		CallerID:        testCaller,
		Key:             key,
		BodyHash:        body,
		ResponseStatus:  201,
		ResponseBody:    []byte(`{"id":"abc"}`),
		ResponseHeaders: map[string]string{"Content-Type": "application/json"},
		CreatedAt:       time.Now(),
		ExpiresAt:       time.Now().Add(time.Hour),
	}
	if err := store.Put(t.Context(), rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(t.Context(), testCaller, key, body)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Key != key {
		t.Errorf("Key = %v", got.Key)
	}
	if string(got.ResponseBody) != `{"id":"abc"}` {
		t.Errorf("ResponseBody = %q", got.ResponseBody)
	}
	if got.ResponseStatus != 201 {
		t.Errorf("ResponseStatus = %d", got.ResponseStatus)
	}
}

func TestInMemoryStore_Get_AbsentKey_ReturnsZero(t *testing.T) {
	t.Parallel()
	store := idempotency.NewInMemoryStore(nil)
	got, err := store.Get(t.Context(), testCaller, uuid.New(), hashOf("anything"))
	if err != nil {
		t.Fatalf("Get on absent: %v", err)
	}
	if got.Key != uuid.Nil {
		t.Errorf("expected zero record, got Key=%v", got.Key)
	}
}

func TestInMemoryStore_Get_BodyMismatch_ReturnsErrBodyMismatch(t *testing.T) {
	t.Parallel()
	store := idempotency.NewInMemoryStore(nil)
	key := uuid.New()
	original := hashOf("original-body")
	different := hashOf("different-body")

	_ = store.Put(t.Context(), idempotency.Record{
		CallerID:        testCaller,
		Key:             key,
		BodyHash:        original,
		ResponseStatus:  200,
		ResponseBody:    []byte("ok"),
		ResponseHeaders: map[string]string{},
		CreatedAt:       time.Now(),
		ExpiresAt:       time.Now().Add(time.Hour),
	})
	_, err := store.Get(t.Context(), testCaller, key, different)
	if !errors.Is(err, idempotency.ErrBodyMismatch) {
		t.Errorf("expected ErrBodyMismatch, got %v", err)
	}
}

// TestInMemoryStore_Get_DifferentCaller_TreatedAsAbsent verifies the
// per-caller scoping invariant — two tenants picking the same
// X-Command-Id MUST NOT see each other's cached response. This is the
// load-bearing security property the audit-fix migration enforces.
func TestInMemoryStore_Get_DifferentCaller_TreatedAsAbsent(t *testing.T) {
	t.Parallel()
	store := idempotency.NewInMemoryStore(nil)
	key := uuid.New()
	body := hashOf("body-1")
	const callerA = "tenant:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const callerB = "tenant:bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	_ = store.Put(t.Context(), idempotency.Record{
		CallerID:        callerA,
		Key:             key,
		BodyHash:        body,
		ResponseStatus:  201,
		ResponseBody:    []byte(`{"secret":"A"}`),
		ResponseHeaders: map[string]string{},
		CreatedAt:       time.Now(),
		ExpiresAt:       time.Now().Add(time.Hour),
	})
	got, err := store.Get(t.Context(), callerB, key, body)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Key != uuid.Nil {
		t.Errorf("cross-caller leak: caller B saw caller A's record %+v", got)
	}
}

func TestInMemoryStore_Get_ExpiredRecord_TreatedAsAbsent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	clock := now
	store := idempotency.NewInMemoryStore(func() time.Time { return clock })
	key := uuid.New()
	body := hashOf("b")

	_ = store.Put(t.Context(), idempotency.Record{
		CallerID:        testCaller,
		Key:             key,
		BodyHash:        body,
		ResponseStatus:  200,
		ResponseBody:    []byte("ok"),
		ResponseHeaders: map[string]string{},
		CreatedAt:       now,
		ExpiresAt:       now.Add(time.Hour),
	})
	clock = now.Add(2 * time.Hour)

	got, err := store.Get(t.Context(), testCaller, key, body)
	if err != nil {
		t.Fatalf("Get expired: %v", err)
	}
	if got.Key != uuid.Nil {
		t.Errorf("expired record returned: %+v", got)
	}
}

func TestInMemoryStore_Put_RejectsZeroKey(t *testing.T) {
	t.Parallel()
	store := idempotency.NewInMemoryStore(nil)
	err := store.Put(t.Context(), idempotency.Record{
		CallerID:  testCaller,
		Key:       uuid.Nil,
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if !errors.Is(err, idempotency.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestInMemoryStore_Put_RejectsEmptyCaller(t *testing.T) {
	t.Parallel()
	store := idempotency.NewInMemoryStore(nil)
	err := store.Put(t.Context(), idempotency.Record{
		Key:       uuid.New(),
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if !errors.Is(err, idempotency.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestInMemoryStore_Put_RejectsZeroExpiresAt(t *testing.T) {
	t.Parallel()
	store := idempotency.NewInMemoryStore(nil)
	err := store.Put(t.Context(), idempotency.Record{
		CallerID: testCaller,
		Key:      uuid.New(),
	})
	if !errors.Is(err, idempotency.ErrInvalid) {
		t.Errorf("expected ErrInvalid, got %v", err)
	}
}

func TestInMemoryStore_Purge_DropsExpired(t *testing.T) {
	t.Parallel()
	store := idempotency.NewInMemoryStore(nil)
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)

	live := uuid.New()
	expired := uuid.New()

	_ = store.Put(t.Context(), idempotency.Record{
		CallerID: testCaller,
		Key:      live, BodyHash: hashOf("a"), ResponseStatus: 200,
		ExpiresAt: now.Add(time.Hour),
	})
	_ = store.Put(t.Context(), idempotency.Record{
		CallerID: testCaller,
		Key:      expired, BodyHash: hashOf("b"), ResponseStatus: 200,
		ExpiresAt: now.Add(-time.Hour),
	})
	if got := store.Len(); got != 2 {
		t.Fatalf("expected 2 entries, got %d", got)
	}
	purged, err := store.Purge(t.Context(), now)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if purged != 1 {
		t.Errorf("Purge returned %d, want 1", purged)
	}
	if store.Len() != 1 {
		t.Errorf("Len after purge = %d, want 1", store.Len())
	}
}

// Compile-time assertion: *InMemoryStore satisfies Store.
var _ idempotency.Store = (*idempotency.InMemoryStore)(nil)

// Compile-time assertion: *PostgresStore satisfies Store.
var _ idempotency.Store = (*idempotency.PostgresStore)(nil)
