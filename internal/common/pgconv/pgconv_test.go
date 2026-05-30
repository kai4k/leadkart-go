package pgconv_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pgconv"
)

var fixedNow = time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)

// TestZeroToNil_EmptyReturnsNil pins the load-bearing invariant relocated
// from crm/adapters: ZeroToNil is used for every nullable filter on the
// CRM/inventory listing queries. A bug returning &"" for empty input would
// inject `WHERE col = ”` predicates that match zero rows for every
// unfiltered listing — a silent denial-of-service against the read surface.
func TestZeroToNil_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	if got := pgconv.ZeroToNil(""); got != nil {
		t.Fatalf("ZeroToNil(\"\"): want nil, got %q", *got)
	}
	if got := pgconv.ZeroToNil(0); got != nil {
		t.Fatalf("ZeroToNil(0): want nil, got %d", *got)
	}
}

// TestZeroToNil_NonZeroReturnsPointer pins the round-trip: a real value
// emerges as a non-nil pointer carrying that value.
func TestZeroToNil_NonZeroReturnsPointer(t *testing.T) {
	t.Parallel()
	got := pgconv.ZeroToNil("contacted")
	if got == nil || *got != "contacted" {
		t.Fatalf("ZeroToNil(\"contacted\"): got %v", got)
	}
}

func TestPgUUID_AlwaysValid(t *testing.T) {
	t.Parallel()
	if p := pgconv.PgUUID(uuid.Nil); !p.Valid {
		t.Fatal("PgUUID(Nil): want Valid=true (zero uuid is a real value)")
	}
}

// TestPgUUIDOrNull round-trips through UUIDFromPg so the test never names
// pgtype directly (DB types stay out of unit tests per arch canon).
func TestPgUUIDOrNull_NilIsNull(t *testing.T) {
	t.Parallel()
	if p := pgconv.PgUUIDOrNull(uuid.Nil); p.Valid {
		t.Fatal("PgUUIDOrNull(Nil): want Valid=false")
	}
	id := uuid.New()
	if got := pgconv.UUIDFromPg(pgconv.PgUUIDOrNull(id)); got != id {
		t.Fatalf("PgUUIDOrNull→UUIDFromPg round-trip: want %s, got %s", id, got)
	}
}

// TestTimestampRoundTrip exercises both directions without constructing a
// pgtype value in the test: PgTimestamp(zero) is NULL, which TimeFromPg
// reads back as the zero time; a real time round-trips UTC-normalised.
func TestTimestampRoundTrip(t *testing.T) {
	t.Parallel()
	if got := pgconv.TimeFromPg(pgconv.PgTimestamp(time.Time{})); !got.IsZero() {
		t.Fatalf("PgTimestamp(zero)→TimeFromPg: want zero, got %v", got)
	}
	if got := pgconv.TimeFromPg(pgconv.PgTimestamp(fixedNow)); !got.Equal(fixedNow) {
		t.Fatalf("PgTimestamp(now)→TimeFromPg: want %v, got %v", fixedNow, got)
	}
	// Required timestamps are always Valid even at the zero value.
	if got := pgconv.TimeFromPg(pgconv.PgRequiredTimestamp(time.Time{})); !got.IsZero() {
		t.Fatalf("PgRequiredTimestamp(zero)→TimeFromPg: want zero, got %v", got)
	}
}
