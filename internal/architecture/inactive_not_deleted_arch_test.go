// inactive_not_deleted_arch_test.go - ADR 0073: products, roles and drug schedule classes are made
// inactive, never deleted.

package architecture_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestArch_InactiveNotDeleted_NoDeleteRouteOnTheThreeResources asserts no module registers a
// DELETE route for a product, a role or a drug schedule class in ANY non-test file of the three
// modules' ports (the lifecycle is POST .../deactivate and .../reactivate), that the two schema
// tables do not carry a soft-delete column again, that the product and role domain types hold no
// deleted state, that the batch domain and table carry none either (a batch is written off or
// quarantined by a stock movement, never deleted), and that the company settings domain never
// removes a class from the list.
//
// arch-test:no-negative-fixture — assertion target is the shipping route files and migrations.
func TestArch_InactiveNotDeleted_NoDeleteRouteOnTheThreeResources(t *testing.T) {
	t.Parallel()
	root := repoRootDir(t)
	goFiles := func(rel string) []string {
		var out []string
		_ = filepath.WalkDir(filepath.Join(root, rel), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				out = append(out, p)
			}
			return nil
		})
		return out
	}
	deleteRoute := regexp.MustCompile(`"DELETE /api/v1/(inventory/products|roles|company/drug-schedule-classes)\b`)
	for _, dir := range []string{"internal/inventory/ports", "internal/identity/ports", "internal/companysettings/ports"} {
		for _, f := range goFiles(dir) {
			if m := deleteRoute.FindString(mustReadFile(t, f)); m != "" {
				t.Errorf("%s registers %s: products, roles and drug schedule classes are made inactive, never deleted (ADR 0073)", f, m)
			}
		}
	}
	// The soft-delete flags the delete routes served stay dropped: the sqlc models are the schema.
	models := [][2]string{
		{"internal/inventory/adapters/db/models.go", "InventoryProduct"},
		{"internal/inventory/adapters/db/models.go", "InventoryBatch"},
		{"internal/identity/adapters/db/models.go", "IdentityRole"},
	}
	field := regexp.MustCompile(`(?m)^\s+(IsDeleted|DeletedAt|DeletedBy)\b`)
	for _, m := range models {
		rel, typ := m[0], m[1]
		body := mustReadFile(t, filepath.Join(root, rel))
		loc := regexp.MustCompile(`(?s)type ` + typ + ` struct \{(.*?)\n\}`).FindStringSubmatch(body)
		if loc == nil {
			t.Fatalf("%s: type %s not found", rel, typ)
		}
		if f := field.FindString(loc[1]); f != "" {
			t.Errorf("%s: %s has a soft-delete column again (%s); it is made inactive, never deleted (ADR 0073)", rel, typ, f)
		}
	}
	// The domain types hold no deleted state (a field, a getter or a Delete / SoftDelete method).
	deleted := regexp.MustCompile(`(?m)^\s+(deleted|deletedAt|deletedBy)\s|func \([a-z]+ \*?(Product|Role|Batch)\) (IsDeleted|DeletedAt|DeletedBy|Delete|SoftDelete)\(`)
	for _, dir := range []string{"internal/inventory/domain/product", "internal/inventory/domain/batch", "internal/identity/domain/role"} {
		for _, f := range goFiles(dir) {
			if strings.Contains(f, "test") { // the producttest / roletest fakes are not the domain
				continue
			}
			if m := deleted.FindString(mustReadFile(t, f)); m != "" {
				t.Errorf("%s holds deleted state (%q): products, roles and batches are never deleted (ADR 0073)", f, strings.TrimSpace(m))
			}
		}
	}
	// A drug schedule class is never removed from the company list: no code in the settings domain
	// deletes from DrugSchedule.Classes or brings back a Remove...Class command.
	removal := regexp.MustCompile(`slices\.Delete\([^)]*Classes|func \([a-z]+ \*?Settings\) Remove[A-Za-z]*Class`)
	for _, f := range goFiles("internal/companysettings/domain") {
		if m := removal.FindString(mustReadFile(t, f)); m != "" {
			t.Errorf("%s removes a drug schedule class (%q): classes are made inactive, never deleted (ADR 0073)", f, m)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "docs", "adr", "0073-inactive-not-deleted.md")); err != nil {
		t.Errorf("ADR 0073 is missing: %v", err)
	}
}
