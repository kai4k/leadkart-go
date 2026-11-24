// inactive_not_deleted_arch_test.go - ADR 0073: products, roles and drug schedule classes are made
// inactive, never deleted.

package architecture_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestArch_InactiveNotDeleted_NoDeleteRouteOnTheThreeResources asserts no module registers a
// DELETE route for a product, a role or a drug schedule class (the lifecycle is
// POST .../deactivate and .../reactivate), and that none of the three schema tables carries a
// soft-delete column again.
//
// arch-test:no-negative-fixture — assertion target is the shipping route files and migrations.
func TestArch_InactiveNotDeleted_NoDeleteRouteOnTheThreeResources(t *testing.T) {
	t.Parallel()
	root := repoRootDir(t)
	deleteRoute := regexp.MustCompile(`"DELETE /api/v1/(inventory/products|roles|company/drug-schedule-classes)\b`)
	for _, rel := range []string{
		"internal/inventory/ports/http.go",
		"internal/identity/ports/http.go",
		"internal/companysettings/ports/http.go",
	} {
		if m := deleteRoute.FindString(mustReadFile(t, filepath.Join(root, rel))); m != "" {
			t.Errorf("%s registers %s: products, roles and drug schedule classes are made inactive, never deleted (ADR 0073)", rel, m)
		}
	}
	// The soft-delete flags the delete routes served stay dropped: the sqlc models are the schema.
	models := map[string]string{
		"internal/inventory/adapters/db/models.go": "InventoryProduct",
		"internal/identity/adapters/db/models.go":  "IdentityRole",
	}
	field := regexp.MustCompile(`(?m)^\s+(IsDeleted|DeletedAt|DeletedBy)\b`)
	for rel, typ := range models {
		body := mustReadFile(t, filepath.Join(root, rel))
		loc := regexp.MustCompile(`(?s)type ` + typ + ` struct \{(.*?)\n\}`).FindStringSubmatch(body)
		if loc == nil {
			t.Fatalf("%s: type %s not found", rel, typ)
		}
		if f := field.FindString(loc[1]); f != "" {
			t.Errorf("%s: %s has a soft-delete column again (%s); it is made inactive, never deleted (ADR 0073)", rel, typ, f)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "adr", "0073-inactive-not-deleted.md")); err != nil {
		t.Errorf("ADR 0073 is missing: %v", err)
	}
}
