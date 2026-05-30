package rolehierarchytest_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy/rolehierarchytest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func seedEdge(now time.Time) *rolehierarchy.Edge {
	return rolehierarchy.UnmarshalFromDB(rolehierarchy.Snapshot{
		ID:                        rolehierarchy.ID("55555555-5555-5555-5555-555555555555"),
		TenantID:                  tenant.ID("11111111-1111-1111-1111-111111111111"),
		ChildRoleID:               role.ID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		ParentRoleID:              role.ID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
		EstablishedAt:             now,
		EstablishedByMembershipID: membership.ID("33333333-3333-3333-3333-333333333333"),
	})
}

// TestFakeRepository_UpdateByID_CommitFalseDiscardsMutations proves the
// fake honors the (false, nil) no-persist contract: removing the edge
// then returning commit=false leaves the STORED edge active, mirroring
// the pg adapter. The pre-fix `_ = commit` leaked the removal.
func TestFakeRepository_UpdateByID_CommitFalseDiscardsMutations(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	repo := rolehierarchytest.NewFakeRepository()
	e := seedEdge(now)
	if err := repo.Add(t.Context(), e); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := repo.UpdateByID(t.Context(), e.TenantID(), e.ID(), func(ed *rolehierarchy.Edge) (bool, error) {
		if rerr := ed.Remove(membership.ID(""), "", now); rerr != nil {
			return false, rerr
		}
		return false, nil // DO NOT PERSIST
	})
	if err != nil {
		t.Fatalf("UpdateByID: %v", err)
	}

	got, err := repo.GetActiveByChild(t.Context(), e.TenantID(), e.ChildRoleID())
	if err != nil {
		t.Fatalf("GetActiveByChild: %v", err)
	}
	if !got.IsActive() {
		t.Fatal("commit=false leaked mutation: stored edge is removed, want active")
	}
}

// TestFakeRepository_UpdateByID_CommitTruePersists is the positive
// companion: commit=true must persist the removal.
func TestFakeRepository_UpdateByID_CommitTruePersists(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	repo := rolehierarchytest.NewFakeRepository()
	e := seedEdge(now)
	if err := repo.Add(t.Context(), e); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := repo.UpdateByID(t.Context(), e.TenantID(), e.ID(), func(ed *rolehierarchy.Edge) (bool, error) {
		if rerr := ed.Remove(membership.ID(""), "", now); rerr != nil {
			return false, rerr
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("UpdateByID: %v", err)
	}

	if _, err := repo.GetActiveByChild(t.Context(), e.TenantID(), e.ChildRoleID()); !errors.Is(err, rolehierarchy.ErrEdgeNotFound) {
		t.Fatalf("commit=true did not persist removal: got err = %v, want ErrEdgeNotFound", err)
	}
}
