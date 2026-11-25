package authority_test

import (
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
)

// G2p n4: with no platform tenant id known (the empty id), no tenant is the platform tenant: the
// company rule applies everywhere, the platform tenant included.
func TestPlatformManagedNames_UnknownPlatformTenantIsTreatedAsACompany(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	staff := mkMemberOv(t, 81, tP, nil, []string{p.Roles.Manage})
	lift := authority.Overrides{Granted: []authority.Override{}, Revoked: []string{}}

	unknown := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(82)), TenantID: tA, IsSuperUser: true})
	if got := unknown.PlatformTenantID(); got != "" {
		t.Fatalf("PlatformTenantID = %q, want empty", got)
	}
	if err := unknown.MayChangeOverrides(staff, lift); err != nil {
		t.Errorf("unknown platform tenant, lift in tP: %v, want the company rule (nil)", err)
	}

	known := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(83)), TenantID: tA, IsSuperUser: true, PlatformTenantID: tP})
	if err := known.MayChangeOverrides(staff, lift); err == nil {
		t.Error("known platform tenant, lift in tP: nil, want a refusal")
	}
}
