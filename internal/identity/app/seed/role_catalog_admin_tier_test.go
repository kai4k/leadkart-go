package seed_test

import (
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/seed"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// Owner ruling 2026-11-21: only the Company Owner and the Administrator tier hold
// identity.users.create (directly or through tenant.admin). Nothing seeded below
// that tier holds it, and no seeded role below it holds any admin-tier-only name.
func TestDefaultRoleCatalog_OnlyTheAdministratorTierHoldsTheAddUsersPermission(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	holders := map[string]bool{}
	for _, spec := range seed.DefaultRoleCatalog() {
		holdsIt := slices.Contains(spec.Permissions, p.Users.Create) || slices.Contains(spec.Permissions, p.Meta.TenantAdmin)
		holders[spec.Name] = holdsIt
		if holdsIt && authority.Rank(spec.HierarchyLevel) > authority.RankAdmin {
			t.Errorf("%s (level %d) holds identity.users.create", spec.Name, spec.HierarchyLevel)
		}
		for _, n := range spec.Permissions {
			if !authority.MayHoldAt(authority.Rank(spec.HierarchyLevel), n) {
				t.Errorf("%s (level %d) holds %s, which is admin-tier-only", spec.Name, spec.HierarchyLevel, n)
			}
		}
	}
	rr := role.SystemRoles.Tenant
	for _, name := range []string{rr.CompanyOwner, rr.Administrator, rr.OfficeAdministrator} {
		if !holders[name] {
			t.Errorf("%s no longer holds identity.users.create", name)
		}
	}
	for _, name := range []string{rr.SeniorManager, rr.SalesManager, rr.PurchaseManager, rr.DispatchManager, rr.HrManager, rr.TeamLead,
		rr.OfficeExecutive, rr.SalesExecutive, rr.PurchaseExecutive, rr.DispatchExecutive, rr.HrExecutive} {
		if holders[name] {
			t.Errorf("%s holds identity.users.create", name)
		}
	}
}
