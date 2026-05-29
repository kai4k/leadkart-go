package membershiptest_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// TestFakeRepository_UpdateByID_CommitFalseDiscardsMutations proves the
// fake honors the (false, nil) no-persist contract: an updateFn that
// mutates the aggregate but returns commit=false must leave the STORED
// state untouched — exactly the pg adapter's behavior (load fresh,
// discard on no-commit). The pre-fix `_ = commit` leaked the mutation.
func TestFakeRepository_UpdateByID_CommitFalseDiscardsMutations(t *testing.T) {
	t.Parallel()

	const (
		tenantID = tenant.ID("11111111-1111-1111-1111-111111111111")
		memID    = membership.ID("22222222-2222-2222-2222-222222222222")
		personID = person.ID("33333333-3333-3333-3333-333333333333")
	)
	now := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)

	repo := membershiptest.NewFakeRepository()
	seed := membership.UnmarshalFromDB(membership.Snapshot{
		ID:       memID,
		PersonID: personID,
		TenantID: tenantID,
		Status:   membership.StatusActive,
		JoinedAt: now,
	})
	if err := repo.Add(t.Context(), seed); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// updateFn deactivates (mutates status → Inactive) but returns commit=false.
	err := repo.UpdateByID(t.Context(), tenantID, memID, func(m *membership.Membership) (bool, error) {
		if derr := m.Deactivate("test", now); derr != nil {
			return false, derr
		}
		return false, nil // DO NOT PERSIST
	})
	if err != nil {
		t.Fatalf("UpdateByID: %v", err)
	}

	got, err := repo.GetByID(t.Context(), tenantID, memID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status() != membership.StatusActive {
		t.Fatalf("commit=false leaked mutation: stored status = %v, want %v", got.Status(), membership.StatusActive)
	}
}

// TestFakeRepository_UpdateByID_CommitTruePersists is the positive
// companion: commit=true must persist the mutation.
func TestFakeRepository_UpdateByID_CommitTruePersists(t *testing.T) {
	t.Parallel()

	const (
		tenantID = tenant.ID("11111111-1111-1111-1111-111111111111")
		memID    = membership.ID("22222222-2222-2222-2222-222222222222")
		personID = person.ID("33333333-3333-3333-3333-333333333333")
	)
	now := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)

	repo := membershiptest.NewFakeRepository()
	if err := repo.Add(t.Context(), membership.UnmarshalFromDB(membership.Snapshot{
		ID: memID, PersonID: personID, TenantID: tenantID, Status: membership.StatusActive, JoinedAt: now,
	})); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := repo.UpdateByID(t.Context(), tenantID, memID, func(m *membership.Membership) (bool, error) {
		if derr := m.Deactivate("test", now); derr != nil {
			return false, derr
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("UpdateByID: %v", err)
	}

	got, err := repo.GetByID(t.Context(), tenantID, memID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status() != membership.StatusInactive {
		t.Fatalf("commit=true did not persist: stored status = %v, want %v", got.Status(), membership.StatusInactive)
	}
}
