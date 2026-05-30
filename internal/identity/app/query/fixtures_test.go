// fixtures_test.go — shared test fixtures for the query package's
// handler-unit tests. Per TDL canon §6 + ADR 0062 the load-bearing
// test layer is the per-aggregate FakeRepository (in
// internal/identity/domain/<aggregate>/<aggregate>test/) wired against
// the handler — these helpers build the small set of domain aggregates
// the query handlers project from.
//
// Single-test-owner pattern: each `seed*` helper returns a fresh fake
// + seeded rows; tests construct their own per-test fixtures to keep
// t.Parallel-safe without sync primitives.

package query_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/druglicence"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/gst"
	"github.com/leadkart/leadkart-go/internal/common/pan"
	"github.com/leadkart/leadkart-go/internal/common/phone"
	"github.com/leadkart/leadkart-go/internal/common/postaladdress"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// testNow is a fixed wall-clock used across the query-test corpus.
// Pinning the clock keeps view comparisons deterministic.
var testNow = time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)

// canonical UUID-shaped IDs used across the suite. Real v7 UUIDs at
// runtime; constants here so cmp.Diff comparisons stay stable.
const (
	testTenantID      = tenant.ID("11111111-1111-1111-1111-111111111111")
	testTenantSlugStr = "acme-pharma"
	testPersonID      = person.ID("22222222-2222-2222-2222-222222222222")
	testMembershipID  = membership.ID("33333333-3333-3333-3333-333333333333")
	testRoleID        = role.ID("44444444-4444-4444-4444-444444444444")
	testEdgeID        = rolehierarchy.ID("55555555-5555-5555-5555-555555555555")
	testRequestID     = permissionrequest.ID("66666666-6666-6666-6666-666666666666")
	testParentRoleID  = role.ID("77777777-7777-7777-7777-777777777777")
	testApproverID    = membership.ID("88888888-8888-8888-8888-888888888888")
	testEmail         = "alice@example.test"
)

// mustEmail constructs an email.Address from a string. Fails the test
// on any validation error — used only with literal-known-good inputs.
func mustEmail(t *testing.T, raw string) email.Address {
	t.Helper()
	a, err := email.New(raw)
	if err != nil {
		t.Fatalf("email.New(%q): %v", raw, err)
	}
	return a
}

// mustSlug constructs a slug.Slug or fails the test.
func mustSlug(t *testing.T, raw string) slug.Slug {
	t.Helper()
	s, err := slug.New(raw)
	if err != nil {
		t.Fatalf("slug.New(%q): %v", raw, err)
	}
	return s
}

// passwordHash builds a stub PasswordHash from a known-good PHC string
// (the format Argon2id produces). Tests don't exercise login — only
// projection — so any valid-shaped hash is fine.
func passwordHash(t *testing.T) person.PasswordHash {
	t.Helper()
	// argon2id PHC shape (per RFC 9106). 22-char base64 salt + 43-char
	// base64 hash. Just needs to satisfy NewPasswordHash's parse.
	raw := "$argon2id$v=19$m=65536,t=3,p=2$c29tZXNhbHRzb21lc2FsdA$kJiHa+vQqI2/9R8sHnSr5h7vrXxoy0YZv2v5cPmw3lI"
	h, err := person.NewPasswordHash(raw)
	if err != nil {
		t.Fatalf("person.NewPasswordHash: %v", err)
	}
	return h
}

// newPerson builds a canonical Person for view-projection tests.
func newPerson(t *testing.T) *person.Person {
	t.Helper()
	p, err := person.New(testPersonID, mustEmail(t, testEmail), "Alice", "Liddell", passwordHash(t), testNow)
	if err != nil {
		t.Fatalf("person.New: %v", err)
	}
	return p
}

// newPersonAt builds a Person with a caller-specified ID + email +
// names — used for multi-row fixtures (list endpoints).
func newPersonAt(t *testing.T, id person.ID, mailAddr, first, last string) *person.Person {
	t.Helper()
	p, err := person.New(id, mustEmail(t, mailAddr), first, last, passwordHash(t), testNow)
	if err != nil {
		t.Fatalf("person.New(%s): %v", id, err)
	}
	return p
}

// newMembership builds a canonical Membership for the given tenant +
// person. Status=Active, JoinedAt=testNow.
func newMembership(t *testing.T, id membership.ID, personID person.ID, tenantID tenant.ID) *membership.Membership {
	t.Helper()
	m, err := membership.New(id, personID, tenantID, membership.ID(""), testNow)
	if err != nil {
		t.Fatalf("membership.New(%s): %v", id, err)
	}
	return m
}

// newRole builds a canonical Role with the supplied (id, tenantID, name).
// Defaults: hierarchyLevel=DefaultMiddle, isSystemDefault=false, isSuperAdmin=false.
func newRole(t *testing.T, id role.ID, tenantID tenant.ID, name string) *role.Role {
	t.Helper()
	r, err := role.New(id, tenantID, name, false, role.HierarchyLevelDefault, false, testNow)
	if err != nil {
		t.Fatalf("role.New(%s): %v", id, err)
	}
	return r
}

// newSuperAdminRole builds a role with isSuperAdmin=true +
// isSystemDefault=true. Used for capabilities-view assertions.
func newSuperAdminRole(t *testing.T, id role.ID, tenantID tenant.ID) *role.Role {
	t.Helper()
	r, err := role.New(id, tenantID, "SuperAdmin", true, role.HierarchyLevelMin, true, testNow)
	if err != nil {
		t.Fatalf("role.New SuperAdmin: %v", err)
	}
	return r
}

// newEdge builds an active rolehierarchy.Edge for (child → parent).
func newEdge(t *testing.T, id rolehierarchy.ID, tenantID tenant.ID, child, parent role.ID) *rolehierarchy.Edge {
	t.Helper()
	e, err := rolehierarchy.New(id, tenantID, child, parent, membership.ID(""), "", testNow)
	if err != nil {
		t.Fatalf("rolehierarchy.New: %v", err)
	}
	return e
}

// newTenant builds a fully-populated Tenant with statutory, contact,
// settings, prefs all set — used by the tenant-view round-trip tests
// to verify every VO field projects correctly.
func newTenant(t *testing.T, id tenant.ID, slugStr string) *tenant.Tenant {
	t.Helper()
	tn, err := tenant.New(id, mustSlug(t, slugStr), "Acme Pharma Pvt Ltd", "Acme Pharma", mustEmail(t, "admin@acme.test"), testNow)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	return tn
}

// newFullyPopulatedTenant adds non-zero statutory + contact + settings
// + prefs onto a fresh tenant so the projection-round-trip test can
// verify every VO field.
func newFullyPopulatedTenant(t *testing.T) *tenant.Tenant {
	t.Helper()
	tn := newTenant(t, testTenantID, testTenantSlugStr)

	// Statutory — a valid PAN + GST that embeds the same PAN.
	// Position-4 must be one of PFHCATBLJG (P = Individual).
	p, err := pan.New("ABCPE1234F")
	if err != nil {
		t.Fatalf("pan.New: %v", err)
	}
	g, err := gst.New("29ABCPE1234F1Z5")
	if err != nil {
		t.Fatalf("gst.New: %v", err)
	}
	dl, err := druglicence.New("KA-MUM-2024-12345")
	if err != nil {
		t.Fatalf("druglicence.New: %v", err)
	}
	stat, err := tenant.NewStatutory(g, p, dl)
	if err != nil {
		t.Fatalf("tenant.NewStatutory: %v", err)
	}
	if err := tn.UpdateStatutory(stat, testNow); err != nil {
		t.Fatalf("UpdateStatutory: %v", err)
	}

	// AdminContact — phone + postal address.
	ph, err := phone.New("+919999999999")
	if err != nil {
		t.Fatalf("phone.New: %v", err)
	}
	addr, err := postaladdress.New("12 MG Road", "Bengaluru", "Bengaluru Urban", "Karnataka", "KA", "560001")
	if err != nil {
		t.Fatalf("postaladdress.New: %v", err)
	}
	ac := tenant.NewAdminContact(ph, addr)
	if err := tn.UpdateAdminContact(ac, testNow); err != nil {
		t.Fatalf("UpdateAdminContact: %v", err)
	}

	// Settings.
	policy, err := tenant.NewPasswordPolicy(14, true, true, true, false, 8, 20)
	if err != nil {
		t.Fatalf("NewPasswordPolicy: %v", err)
	}
	if err := tn.UpdateSettings(tenant.NewSettings(policy), testNow); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	// Prefs.
	prefs, err := tenant.NewDisplayPreferences("en-IN", "Asia/Kolkata", "DD-MMM-YYYY", "INR")
	if err != nil {
		t.Fatalf("NewDisplayPreferences: %v", err)
	}
	if err := tn.UpdateDisplayPreferences(prefs, testNow); err != nil {
		t.Fatalf("UpdateDisplayPreferences: %v", err)
	}
	return tn
}

// metaTenantAdminPermission returns the canonical
// `Meta.TenantAdmin` permission used as the catalogue-known
// permission for permission-request fixtures.
func metaTenantAdminPermission(t *testing.T) *permission.Permission {
	t.Helper()
	p, err := permission.Create(permission.IdentityPermissions.Meta.TenantAdmin)
	if err != nil {
		t.Fatalf("permission.Create: %v", err)
	}
	return p
}
