package command

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role/roletest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// factsPersons answers GetByID with an active person for any id.
type factsPersons struct{ person.Repository }

func (factsPersons) GetByID(_ context.Context, id person.ID) (*person.Person, error) {
	addr, err := email.New(strings.ReplaceAll(string(id), "-", "") + "@facts.test")
	if err != nil {
		return nil, err
	}
	hash, err := person.NewPasswordHash("$argon2id$v=19$m=65536,t=3,p=4$YWJjZGVmZ2g$ZS1zdHViLWhhc2gtdGVzdC1maXh0dXJlLW9ubHk")
	if err != nil {
		return nil, err
	}
	return person.New(id, addr, "A", "B", hash, time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC))
}

const factsTenant = tenant.ID("0190c000-0000-7000-8000-00000000000a")

func factsID(n int) string { return fmt.Sprintf("0190c000-0000-7000-8000-%012d", n) }

// TestOwnersLoader_OnlyTheCompanyOwnerSystemRoleCountsAsOwnership: a role that
// merely carries the owner role's NAME (custom, or flagged super-admin) is not
// the ownership role, so its holders are not "owners" for the last-owner guard;
// a missing role yields no owners; the real system role yields its holders.
func TestOwnersLoader_OnlyTheCompanyOwnerSystemRoleCountsAsOwnership(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	ownerName := role.SystemRoles.Tenant.CompanyOwner
	admin := []*permission.Permission{permission.FromConstant(permission.IdentityPermissions.Meta.TenantAdmin)}
	for name, c := range map[string]struct {
		snap      *role.Snapshot
		wantCount int
	}{
		"no role of that name": {nil, 0},
		"custom role with the owner name": {&role.Snapshot{ID: role.ID(factsID(1)), TenantID: factsTenant, Name: ownerName, HierarchyLevel: 50,
			Permissions: admin, CreatedAt: now}, 0},
		"system role flagged super-admin with the owner name": {&role.Snapshot{ID: role.ID(factsID(1)), TenantID: factsTenant, Name: ownerName,
			IsSystemDefault: true, IsSuperAdmin: true, HierarchyLevel: 0, CreatedAt: now}, 0},
		"the Company Owner system role": {&role.Snapshot{ID: role.ID(factsID(1)), TenantID: factsTenant, Name: ownerName,
			IsSystemDefault: true, HierarchyLevel: 0, Permissions: admin, CreatedAt: now}, 2},
	} {
		roles, members := roletest.NewFakeRepository(), membershiptest.NewFakeRepository()
		var holders []role.ID
		if c.snap != nil {
			r := role.UnmarshalFromDB(*c.snap)
			if err := roles.Add(t.Context(), r); err != nil {
				t.Fatalf("%s: add role: %v", name, err)
			}
			holders = []role.ID{r.ID()}
		}
		for i := range 2 {
			m := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(factsID(10 + i)), PersonID: person.ID(factsID(20 + i)), TenantID: factsTenant,
				Status: membership.StatusActive, JoinedAt: now, RoleAssignments: holders})
			if err := members.Add(t.Context(), m); err != nil {
				t.Fatalf("%s: add member: %v", name, err)
			}
		}
		f := authorityFacts{memberships: members, roles: roles, persons: factsPersons{}}
		got, err := f.owners(t.Context(), factsTenant, now)
		if err != nil {
			t.Fatalf("%s: owners: %v", name, err)
		}
		if len(got) != c.wantCount {
			t.Errorf("%s: %d owners, want %d", name, len(got), c.wantCount)
		}
		for _, o := range got {
			if !o.IsOwner() {
				t.Errorf("%s: an owners() entry is not an owner", name)
			}
		}
	}
}
