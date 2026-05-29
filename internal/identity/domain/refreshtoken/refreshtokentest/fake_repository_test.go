package refreshtokentest_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken/refreshtokentest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func seedFamily(now time.Time) *refreshtoken.Family {
	return refreshtoken.UnmarshalFromDB(refreshtoken.FamilySnapshot{
		ID:          refreshtoken.FamilyID("44444444-4444-4444-4444-444444444444"),
		PersonID:    person.ID("33333333-3333-3333-3333-333333333333"),
		TenantID:    tenant.ID("11111111-1111-1111-1111-111111111111"),
		DeviceLabel: "test-device",
		CreatedAt:   now,
		LastUsedAt:  now,
	})
}

// TestFakeRepository_UpdateByID_CommitFalseDiscardsMutations proves the
// fake honors the (false, nil) no-persist contract: mutating then
// returning commit=false leaves the STORED family untouched, mirroring
// the pg adapter. The pre-fix `_ = commit` leaked the revocation.
func TestFakeRepository_UpdateByID_CommitFalseDiscardsMutations(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	repo := refreshtokentest.NewFakeRepository()
	fam := seedFamily(now)
	if err := repo.Add(t.Context(), fam); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := repo.UpdateByID(t.Context(), fam.ID(), func(f *refreshtoken.Family) (bool, error) {
		if rerr := f.Revoke("test", now); rerr != nil {
			return false, rerr
		}
		return false, nil // DO NOT PERSIST
	})
	if err != nil {
		t.Fatalf("UpdateByID: %v", err)
	}

	got, err := repo.GetByID(t.Context(), fam.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.IsRevoked() {
		t.Fatal("commit=false leaked mutation: stored family is revoked, want active")
	}
}

// TestFakeRepository_UpdateByID_CommitTruePersists is the positive
// companion: commit=true must persist the revocation.
func TestFakeRepository_UpdateByID_CommitTruePersists(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	repo := refreshtokentest.NewFakeRepository()
	fam := seedFamily(now)
	if err := repo.Add(t.Context(), fam); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := repo.UpdateByID(t.Context(), fam.ID(), func(f *refreshtoken.Family) (bool, error) {
		if rerr := f.Revoke("test", now); rerr != nil {
			return false, rerr
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("UpdateByID: %v", err)
	}

	got, err := repo.GetByID(t.Context(), fam.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !got.IsRevoked() {
		t.Fatal("commit=true did not persist: stored family is not revoked")
	}
}
