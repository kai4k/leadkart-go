package adapters

import (
	"testing"
)

// TestStringPtr_EmptyReturnsNil pins reviewer H7: the helper is
// load-bearing for every nullable filter on the ListCrmLeadsPage SQL
// query (Stage / Temperature / City / Pincode / BusinessType /
// MedicineSystem). A bug that returned `&""` for empty input would
// inject `WHERE stage = ''` predicates that match ZERO rows for every
// unfiltered listing — a silent denial-of-service against the entire
// CRM read surface.
//
// The integration-level guarantee lives in
// TestCrmLeadRepository_ListPage_FilterByStage (unfiltered listing
// returns N rows, not 0). This unit test belts-and-suspenders the
// invariant at the helper layer so any future regression fails fast
// without spinning a testcontainer.
func TestStringPtr_EmptyReturnsNil(t *testing.T) {
	t.Parallel()
	if got := stringPtr(""); got != nil {
		t.Fatalf("stringPtr(\"\"): want nil, got %v (%q)", got, *got)
	}
}

// TestStringPtr_NonEmptyReturnsPointer pins the round-trip: a real
// filter value MUST emerge as a non-nil pointer with the right value.
func TestStringPtr_NonEmptyReturnsPointer(t *testing.T) {
	t.Parallel()
	got := stringPtr("contacted")
	if got == nil {
		t.Fatal("stringPtr(\"contacted\"): want non-nil")
	}
	if *got != "contacted" {
		t.Fatalf("stringPtr(\"contacted\"): value = %q", *got)
	}
}
