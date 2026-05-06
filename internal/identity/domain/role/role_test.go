package role_test

import (
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

func TestHierarchyConstants(t *testing.T) {
	t.Parallel()
	if role.HierarchyLevelMin != 0 || role.HierarchyLevelMax != 99 ||
		role.HierarchyLevelDefault != 50 || role.HierarchyLevelNoRole != 99 {
		t.Fatal("hierarchy constants drifted from .NET parent")
	}
}

func TestID_IsZero(t *testing.T) {
	t.Parallel()
	if !role.ID("").IsZero() {
		t.Fatal("empty ID should be zero")
	}
	if role.ID("x").IsZero() {
		t.Fatal("non-empty ID should not be zero")
	}
}
