package role_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/humantext"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

var rloName = "Sales" + string(rune(0x202e)) + "Lead"

func TestNew_RefusesANameWithDirectionOverride(t *testing.T) {
	t.Parallel()
	_, err := role.New(role.ID(ids.NewV7().String()), tenant.ID(ids.NewV7().String()), rloName, false, role.HierarchyLevelDefault, false, testNow)
	var he *humantext.Error
	if !errors.Is(err, role.ErrInvalid) || !errors.As(err, &he) || he.Field != "name" {
		t.Fatalf("err = %v, want role.ErrInvalid + humantext.Error for name", err)
	}
}

func TestRename_RefusesAChangedNameButAcceptsAnUnchangedLegacyOne(t *testing.T) {
	t.Parallel()
	legacy := role.UnmarshalFromDB(role.Snapshot{ID: role.ID(ids.NewV7().String()), TenantID: tenant.ID(ids.NewV7().String()),
		Name: rloName, HierarchyLevel: role.HierarchyLevelDefault, CreatedAt: testNow, IsActive: true})
	if err := legacy.Rename(rloName, testNow); err != nil {
		t.Fatalf("an unchanged legacy name must be accepted: %v", err)
	}
	err := legacy.Rename(rloName+"x", testNow)
	if !errors.Is(err, role.ErrInvalid) || !errors.Is(err, humantext.ErrInvalidCharacters) {
		t.Fatalf("changed name: err = %v", err)
	}
	if legacy.Name() != rloName {
		t.Fatal("a refused rename must change nothing")
	}
}
