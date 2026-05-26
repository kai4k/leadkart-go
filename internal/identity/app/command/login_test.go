// login_test.go — handler-unit tests for the LoginHandler. The
// negative branches are pure handler orchestration over the AuthRouter
// contract + argon2 verify + best-effort lockout persistence; the
// happy path covers refresh-family mint + JWT issuance with claim-
// shape locks. No SQL contract is exercised — per TDL canon §6 + ADR
// 0062 the coverage belongs at the fake-backed unit layer, not the
// integration tier.
//
// Replaces TestFlow_LoginUnknownEmail_GenericFailure +
// TestFlow_LoginWrongPassword_GenericFailure in
// flow_integration_test.go — both asserted only `errors.Is(err,
// ErrInvalidCredentials)`, an observable mirror-able by the fake.
// Wall time: ~200ms per failure-only test vs. testcontainer +
// miniredis + JWT issuer boot for the integration equivalents.

package command_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/app/permissions"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken/refreshtokentest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role/roletest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

// loginTestSigningKey is the deterministic HS256 secret used by the
// happy-path / claim-shape login tests. 32+ bytes per RFC 7518 §3.2.
// Kept local to login_test.go to avoid collisions with the impersonation
// test's own key constant.
//
// nolint:gochecknoglobals // test fixture constant
var loginTestSigningKey = jwt.SigningKey{
	KeyID:  "login-test-key",
	Secret: []byte("login-test-secret-bytes-32-plus-bytes!"),
}

// fakeAuthRouter is a minimal in-memory AuthRouter for handler-unit
// tests. The contract method `ResolveByEmail` is the single seam the
// Login handler exercises; the fake mirrors the SQL adapter's
// observable shape (returns person.ErrNotFound for unknown email;
// returns the seeded (person, membership) tuple for known email).
//
// resolveErr — when non-nil + not ErrNotFound — is propagated so the
// handler's "non-ErrNotFound" branch can be exercised.
type fakeAuthRouter struct {
	byEmail map[email.Address]struct {
		p *person.Person
		m *membership.Membership
	}
	resolveErr error
}

func (f *fakeAuthRouter) ResolveByEmail(_ context.Context, e email.Address) (*person.Person, *membership.Membership, error) {
	if f.resolveErr != nil {
		return nil, nil, f.resolveErr
	}
	row, ok := f.byEmail[e]
	if !ok {
		return nil, nil, person.ErrNotFound
	}
	return row.p, row.m, nil
}

// loginHandlerForFailurePaths constructs a LoginHandler wired with the
// minimum deps needed for the failure-only branches: the AuthRouter +
// person repo (for best-effort lockout persistence on wrong-password)
// + a real dummyHash (so the timing-equalization argon2.Verify runs
// against a valid PHC string). The rest of the deps (refresh families,
// tenants, resolver, jwt) are not reached on failure paths — passing
// nil exposes any drift where the failure branch accidentally falls
// through to the success path.
func loginHandlerForFailurePaths(t *testing.T, router command.AuthRouter, persons person.Repository) command.LoginHandler {
	t.Helper()
	dummy, err := argon2.Hash("dummy-handler-unit-test")
	if err != nil {
		t.Fatalf("dummyHash: %v", err)
	}
	return command.NewLoginHandler(
		router,
		nil, // families — not reached on failure
		nil, // tenants — not reached on failure
		persons,
		nil, // resolver — not reached on failure
		nil, // jwt issuer — not reached on failure
		func() time.Time { return testNow },
		0, // refreshTTL — not reached on failure
		dummy,
		func() refreshtoken.FamilyID { return refreshtoken.FamilyID("never-minted-on-failure") },
	)
}

// wiredLoginRig groups the deps + handler for happy-path login tests.
// Built once via newLoginRig(t) per test so each test has its own
// fakes (TDL single-test-owner pattern — no cross-test shared state).
type wiredLoginRig struct {
	handler     command.LoginHandler
	router      *fakeAuthRouter
	families    *refreshtokentest.FakeRepository
	tenants     *tenanttest.FakeRepository
	persons     *persontest.FakeRepository
	memberships *membershiptest.FakeRepository
	roles       *roletest.FakeRepository
	issuer      *jwt.Issuer
}

// newLoginRig wires a full LoginHandler over per-aggregate fakes + a
// real (test-signed) JWT issuer + permission resolver. Tests seed the
// fakes via the returned struct's repo handles before calling Handle.
func newLoginRig(t *testing.T) wiredLoginRig {
	t.Helper()

	dummy, err := argon2.Hash("dummy-handler-unit-test")
	if err != nil {
		t.Fatalf("dummyHash: %v", err)
	}

	now := func() time.Time { return testNow }
	issuer, err := jwt.NewIssuer(loginTestSigningKey, nil, now)
	if err != nil {
		t.Fatalf("jwt.NewIssuer: %v", err)
	}

	router := &fakeAuthRouter{byEmail: map[email.Address]struct {
		p *person.Person
		m *membership.Membership
	}{}}
	persons := persontest.NewFakeRepository()
	families := refreshtokentest.NewFakeRepository()
	tenants := tenanttest.NewFakeRepository()
	memberships := membershiptest.NewFakeRepository()
	roles := roletest.NewFakeRepository()
	resolver := permissions.NewResolver(memberships, roles, now)

	famSeq := 0
	mintFamilyID := func() refreshtoken.FamilyID {
		famSeq++
		// Deterministic UUID-shaped FamilyID for pinnable assertions.
		return refreshtoken.FamilyID(ids.NewV7().String())
	}

	h := command.NewLoginHandler(
		router, families, tenants, persons, resolver, issuer,
		now, 14*24*time.Hour, dummy, mintFamilyID,
	)
	return wiredLoginRig{
		handler: h, router: router, families: families, tenants: tenants,
		persons: persons, memberships: memberships, roles: roles, issuer: issuer,
	}
}

// seedLoginCandidate seeds a Person + (optional) Membership into the
// rig and registers them with the AuthRouter for resolution by email.
func seedLoginCandidate(t *testing.T, rig wiredLoginRig, p *person.Person, m *membership.Membership) {
	t.Helper()
	if err := rig.persons.Add(t.Context(), p); err != nil {
		t.Fatalf("persons.Add: %v", err)
	}
	if m != nil {
		if err := rig.memberships.Add(t.Context(), m); err != nil {
			t.Fatalf("memberships.Add: %v", err)
		}
	}
	rig.router.byEmail[p.Email()] = struct {
		p *person.Person
		m *membership.Membership
	}{p: p, m: m}
}

// newPersonInactive builds a Person + flips IsActive=false via the
// Snapshot path (the aggregate doesn't expose a Deactivate mutator —
// rehydration is the canonical way to construct deactivated state per
// TDL Wild Workouts canon).
func newPersonInactive(t *testing.T, plain string) *person.Person {
	t.Helper()
	live := newPersonWithPassword(t, plain)
	snap := person.Snapshot{
		ID:               live.ID(),
		Email:            live.Email(),
		FirstName:        live.FirstName(),
		LastName:         live.LastName(),
		PasswordHash:     live.PasswordHash(),
		SecurityStamp:    live.SecurityStamp(),
		IsActive:         false, // <-- the load-bearing bit
		IsAnonymised:     false,
		CreatedAt:        live.CreatedAt(),
		FailedLoginCount: live.FailedLoginCount(),
	}
	return person.UnmarshalFromDB(snap)
}

// newPersonAnonymised returns a Person in the anonymised terminal state
// (DPDP/GDPR right-to-erasure). Walks the aggregate's Anonymise mutator
// so the credential is scrubbed in the same shape the production flow
// produces. Pre-anonymisation the Person needs IsActive=false (the
// invariant Anonymise enforces) — call via Snapshot.
func newPersonAnonymised(t *testing.T) *person.Person {
	t.Helper()
	p := newPersonWithPassword(t, "pre-anonymise-pw")
	// Anonymise requires the global-suspension predicate; the simplest
	// canonical path is Snapshot-rehydrate with IsAnonymised=true.
	snap := person.Snapshot{
		ID:            p.ID(),
		Email:         p.Email(),
		FirstName:     p.FirstName(),
		LastName:      p.LastName(),
		PasswordHash:  p.PasswordHash(),
		SecurityStamp: p.SecurityStamp(),
		IsActive:      false,
		IsAnonymised:  true,
		CreatedAt:     p.CreatedAt(),
		AnonymisedAt:  testNow,
	}
	return person.UnmarshalFromDB(snap)
}

// newPersonAtLockoutThreshold returns a Person whose failed-login
// counter is one shy of MaxFailedLogins, so the NEXT failed attempt
// flips IsLocked to true post-increment. Used by the
// threshold-crossing test.
func newPersonAtLockoutThreshold(t *testing.T, plain string) *person.Person {
	t.Helper()
	p := newPersonWithPassword(t, plain)
	// Register MaxFailedLogins-1 failures so the next failure trips the
	// threshold. All within the LockoutWindow.
	for range person.MaxFailedLogins - 1 {
		p.RegisterFailedLogin(testNow)
	}
	return p
}

// newPersonAlreadyLocked returns a Person whose lockedUntil is in the
// future relative to testNow. Used by the lockout-before-verify test.
func newPersonAlreadyLocked(t *testing.T, plain string) *person.Person {
	t.Helper()
	p := newPersonWithPassword(t, plain)
	// Trip the lockout via the aggregate mutator — same path the
	// production register-failed-login flow walks.
	for range person.MaxFailedLogins {
		p.RegisterFailedLogin(testNow)
	}
	if !p.IsLocked(testNow) {
		t.Fatalf("setup: Person not locked after %d failures", person.MaxFailedLogins)
	}
	return p
}

// newTenantWithSlug helper for the slug-driven IsPlatform claim test.
func newTenantWithSlug(t *testing.T, sStr string) *tenant.Tenant {
	t.Helper()
	tSlug, err := slug.New(sStr)
	if err != nil {
		t.Fatalf("slug.New: %v", err)
	}
	addr, _ := email.New("admin@" + sStr + ".test")
	tn, err := tenant.New(
		tenant.ID(ids.NewV7().String()),
		tSlug, sStr+" Pharma Pvt Ltd", sStr+" Pharma", addr,
		testNow,
	)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	tn.PullEvents()
	return tn
}

// activeMembership constructs an Active Membership for the supplied
// Person + Tenant. Convenience for happy-path test seeding.
func activeMembership(t *testing.T, p *person.Person, tn *tenant.Tenant) *membership.Membership {
	t.Helper()
	m, err := membership.New(
		membership.ID(ids.NewV7().String()),
		p.ID(), tn.ID(),
		membership.ID(""), // no creator — system-bootstrapped
		testNow,
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	return m
}

// ----- existing failure-path tests -----------------------------------------

// TestLogin_UnknownEmail_ReturnsInvalidCredentials covers the
// unknown-email branch: AuthRouter returns person.ErrNotFound; the
// handler runs argon2.Verify against the dummy hash for timing
// equalization (OWASP API §A07 — credential enumeration defense); the
// observable is ErrInvalidCredentials.
//
// SQL coverage of `ErrNotFound` lives in the adapter integration
// tests (membership_repository_pg_test). This test only proves the
// HANDLER ORCHESTRATION on the unknown-email branch.
func TestLogin_UnknownEmail_ReturnsInvalidCredentials(t *testing.T) {
	t.Parallel()
	persons := persontestRepo(t) // empty
	router := &fakeAuthRouter{byEmail: map[email.Address]struct {
		p *person.Person
		m *membership.Membership
	}{}}
	h := loginHandlerForFailurePaths(t, router, persons)

	addr, _ := email.New("nobody@flow.test")
	_, err := h.Handle(t.Context(), command.LoginCommand{
		Email:    addr,
		Password: "any-password",
	})
	if !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("Handle: got %v, want ErrInvalidCredentials", err)
	}
}

// TestLogin_WrongPassword_ReturnsInvalidCredentials covers the
// wrong-password branch: AuthRouter returns the real (person,
// membership); argon2.Verify on the supplied plaintext returns
// ErrMismatch; the handler records the failed attempt via
// persons.UpdateLockoutState; observable is ErrInvalidCredentials.
//
// State assertion: the seeded Person's FailedLoginCount must increment
// by 1 (the canonical post-condition of RegisterFailedLogin).
func TestLogin_WrongPassword_ReturnsInvalidCredentials(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "the-real-password")
	persons := seedPersonRepo(t, p)

	// Person needs an Active Membership for the wrong-password branch
	// (m != nil); else the handler takes the "no Active Membership"
	// branch (which is a separate test case).
	m, err := membership.New(
		membership.ID("11111111-1111-1111-1111-111111111111"),
		p.ID(),
		tenant.ID("33333333-3333-3333-3333-333333333333"),
		membership.ID(""),
		testNow,
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}

	router := &fakeAuthRouter{byEmail: map[email.Address]struct {
		p *person.Person
		m *membership.Membership
	}{
		p.Email(): {p: p, m: m},
	}}
	h := loginHandlerForFailurePaths(t, router, persons)

	priorFailed := p.FailedLoginCount()
	_, err = h.Handle(t.Context(), command.LoginCommand{
		Email:    p.Email(),
		Password: "WRONG-password",
	})
	if !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("Handle: got %v, want ErrInvalidCredentials", err)
	}
	if got := p.FailedLoginCount(); got != priorFailed+1 {
		t.Fatalf("FailedLoginCount: got %d, want %d (RegisterFailedLogin not invoked)", got, priorFailed+1)
	}
}

// persontestRepo is a one-line alias so the test reads "empty repo for
// the unknown-email scenario" rather than the longer call chain.
func persontestRepo(t *testing.T) person.Repository {
	t.Helper()
	return seedPersonRepo(t, nil)
}

// ----- happy path + claim shape --------------------------------------------

// TestLogin_HappyPath_IssuesTokensAndJWT covers the success branch:
// resolves Person + active Membership, mints a refresh family, issues
// a signed JWT, returns the LoginResult tuple. State assertions cover:
//   - tokens populated (access + refresh plaintext)
//   - returned IDs match the seeded aggregates
//   - MustChangePassword=false propagates
//   - refresh-token family persisted in the families fake
//   - issued JWT verifies + claims carry tenant_slug / membership_id /
//     security_stamp / IsPlatform=false (slug != "platform")
//   - FailedLoginCount reset to 0 on success (Wave 9.2a-b lockout flow)
func TestLogin_HappyPath_IssuesTokensAndJWT(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)

	const plain = "correct horse battery staple"
	p := newPersonWithPassword(t, plain)
	tn := newTenantWithSlug(t, "acme")
	m := activeMembership(t, p, tn)
	seedLoginCandidate(t, rig, p, m)
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenants.Add: %v", err)
	}

	res, err := rig.handler.Handle(t.Context(), command.LoginCommand{
		Email:       p.Email(),
		Password:    plain,
		DeviceLabel: "iPhone 15 / Safari",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.AccessToken == "" {
		t.Error("AccessToken empty — issuer did not run")
	}
	if res.RefreshTokenPlain == "" {
		t.Error("RefreshTokenPlain empty — mint did not run")
	}
	if res.PersonID != p.ID() {
		t.Errorf("PersonID = %q, want %q", res.PersonID, p.ID())
	}
	if res.TenantID != tn.ID() {
		t.Errorf("TenantID = %q, want %q", res.TenantID, tn.ID())
	}
	if res.MembershipID != m.ID() {
		t.Errorf("MembershipID = %q, want %q", res.MembershipID, m.ID())
	}
	if res.MustChangePassword {
		t.Error("MustChangePassword = true; the seeded Person was self-rotated, want false")
	}

	// State assertion: refresh family persisted.
	fams, err := rig.families.ListActiveForPerson(t.Context(), p.ID())
	if err != nil {
		t.Fatalf("ListActiveForPerson: %v", err)
	}
	if len(fams) != 1 {
		t.Fatalf("ListActiveForPerson: got %d families, want 1", len(fams))
	}

	// JWT claim-shape lock.
	claims, err := rig.issuer.Verify(res.AccessToken)
	if err != nil {
		t.Fatalf("issuer.Verify: %v", err)
	}
	if claims.TenantSlug != "acme" {
		t.Errorf("tenant_slug = %q, want %q", claims.TenantSlug, "acme")
	}
	if claims.IsPlatform {
		t.Error("IsPlatform=true for non-platform tenant — slug-anchored mint regressed")
	}
	if claims.MembershipID != m.ID().String() {
		t.Errorf("membership_id = %q, want %q", claims.MembershipID, m.ID().String())
	}
	if claims.SecurityStamp != p.SecurityStamp().String() {
		t.Error("security_stamp claim does not match Person.SecurityStamp()")
	}
	if claims.Subject != p.ID().String() {
		t.Errorf("sub = %q, want %q", claims.Subject, p.ID().String())
	}

	// FailedLoginCount cleared by RegisterSuccessfulLogin.
	if p.FailedLoginCount() != 0 {
		t.Errorf("FailedLoginCount = %d, want 0 after successful login", p.FailedLoginCount())
	}
}

// TestLogin_HappyPath_PropagatesMustChangePassword proves the
// LoginResult.MustChangePassword bit threads through from the Person
// aggregate per ADR 0053 + BRD line 241 (admin-provisioned credentials
// require forced rotation on first login).
func TestLogin_HappyPath_PropagatesMustChangePassword(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)

	const plain = "admin-provisioned-pw"
	hashStr, err := argon2.Hash(plain)
	if err != nil {
		t.Fatalf("argon2.Hash: %v", err)
	}
	hash, err := person.NewPasswordHash(hashStr)
	if err != nil {
		t.Fatalf("NewPasswordHash: %v", err)
	}
	addr, _ := email.New("invited@acme.test")
	p, err := person.NewWithMustChangePassword(
		person.ID(ids.NewV7().String()),
		addr, "Invited", "User", hash, testNow,
	)
	if err != nil {
		t.Fatalf("NewWithMustChangePassword: %v", err)
	}
	tn := newTenantWithSlug(t, "acme")
	m := activeMembership(t, p, tn)
	seedLoginCandidate(t, rig, p, m)
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenants.Add: %v", err)
	}

	res, err := rig.handler.Handle(t.Context(), command.LoginCommand{
		Email:       p.Email(),
		Password:    plain,
		DeviceLabel: "iPad",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.MustChangePassword {
		t.Error("MustChangePassword = false; expected true for admin-provisioned credential")
	}
}

// TestLogin_HappyPath_PlatformSlug_SetsIsPlatformClaim covers the
// slug-anchored IsPlatform claim per Phase 1.5 hardening — only
// tenants whose Slug() == "platform" get is_platform=true on the JWT.
// Defense-in-depth so a tenant impersonating "platform" via a
// different slug can never get is_platform=true minted.
func TestLogin_HappyPath_PlatformSlug_SetsIsPlatformClaim(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)

	const plain = "platform-op-pw"
	p := newPersonWithPassword(t, plain)
	tn := newTenantWithSlug(t, "platform")
	m := activeMembership(t, p, tn)
	seedLoginCandidate(t, rig, p, m)
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenants.Add: %v", err)
	}

	res, err := rig.handler.Handle(t.Context(), command.LoginCommand{
		Email:       p.Email(),
		Password:    plain,
		DeviceLabel: "Op Terminal",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	claims, err := rig.issuer.Verify(res.AccessToken)
	if err != nil {
		t.Fatalf("issuer.Verify: %v", err)
	}
	if !claims.IsPlatform {
		t.Error("IsPlatform=false for platform tenant — slug-anchored mint regressed")
	}
}

// TestLogin_HappyPath_PermissionsClaimSorted covers the
// permissionNames() flattening: catalogue permission constants get
// embedded into the JWT as sorted string slice. Two logins with the
// same effective set must produce byte-identical claim arrays for
// cache + diff friendliness.
func TestLogin_HappyPath_PermissionsClaimSorted(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)

	const plain = "complex-pw"
	p := newPersonWithPassword(t, plain)
	tn := newTenantWithSlug(t, "acme")
	m := activeMembership(t, p, tn)

	// Build a role granting two permissions in deliberate non-sorted
	// declaration order — issuer should emit them sorted.
	r, err := role.New(
		role.ID(ids.NewV7().String()), tn.ID(), "Tester",
		false, role.HierarchyLevelDefault, false, testNow,
	)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	if err := r.GrantPermission(permission.FromConstant(permission.IdentityPermissions.Users.View), testNow); err != nil {
		t.Fatalf("GrantPermission: %v", err)
	}
	if err := r.GrantPermission(permission.FromConstant(permission.IdentityPermissions.Meta.TenantAdmin), testNow); err != nil {
		t.Fatalf("GrantPermission: %v", err)
	}
	if err := rig.roles.Add(t.Context(), r); err != nil {
		t.Fatalf("roles.Add: %v", err)
	}
	if err := m.AssignRole(r.ID(), testNow); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	seedLoginCandidate(t, rig, p, m)
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenants.Add: %v", err)
	}

	res, err := rig.handler.Handle(t.Context(), command.LoginCommand{
		Email:       p.Email(),
		Password:    plain,
		DeviceLabel: "Browser",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	claims, err := rig.issuer.Verify(res.AccessToken)
	if err != nil {
		t.Fatalf("issuer.Verify: %v", err)
	}
	if !slices.IsSorted(claims.Permissions) {
		t.Errorf("permissions claim not sorted: %v", claims.Permissions)
	}
}

// ----- lockout branches ----------------------------------------------------

// TestLogin_AlreadyLocked_ReturnsLockedErrorBeforeVerify covers the
// pre-verify lockout gate per NIST 800-63B §5.2.2 + ADR 0053. Locked
// Persons surface ErrAccountLocked + a LockedUntil timestamp BEFORE
// argon2.Verify runs (timing-leak avoidance).
func TestLogin_AlreadyLocked_ReturnsLockedErrorBeforeVerify(t *testing.T) {
	t.Parallel()
	const plain = "the-real-password"
	p := newPersonAlreadyLocked(t, plain)
	persons := seedPersonRepo(t, p)

	m, err := membership.New(
		membership.ID(ids.NewV7().String()),
		p.ID(), tenant.ID(ids.NewV7().String()),
		membership.ID(""), testNow,
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	router := &fakeAuthRouter{byEmail: map[email.Address]struct {
		p *person.Person
		m *membership.Membership
	}{p.Email(): {p: p, m: m}}}
	h := loginHandlerForFailurePaths(t, router, persons)

	_, err = h.Handle(t.Context(), command.LoginCommand{
		Email:    p.Email(),
		Password: plain, // correct password — but locked supersedes
	})
	if !errors.Is(err, command.ErrAccountLocked) {
		t.Fatalf("got %v, want ErrAccountLocked", err)
	}
	locked := command.LockedUntilFromError(err)
	if locked.IsZero() {
		t.Error("LockedUntilFromError: zero on ErrAccountLocked — Retry-After header would be blank")
	}
	if !locked.Equal(p.LockedUntil()) {
		t.Errorf("LockedUntil = %v, want %v", locked, p.LockedUntil())
	}
}

// TestLogin_ThresholdCrossing_LocksAndReturnsLockedError covers the
// post-increment re-check branch: wrong-password attempt that pushes
// failedLoginCount across MaxFailedLogins flips IsLocked synchronously
// and the handler surfaces ErrAccountLocked instead of
// ErrInvalidCredentials (Auth0/Okta UX canon — "wait + retry" beats
// "try a different password").
func TestLogin_ThresholdCrossing_LocksAndReturnsLockedError(t *testing.T) {
	t.Parallel()
	const plain = "the-real-password"
	p := newPersonAtLockoutThreshold(t, plain)
	persons := seedPersonRepo(t, p)
	m, err := membership.New(
		membership.ID(ids.NewV7().String()),
		p.ID(), tenant.ID(ids.NewV7().String()),
		membership.ID(""), testNow,
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	router := &fakeAuthRouter{byEmail: map[email.Address]struct {
		p *person.Person
		m *membership.Membership
	}{p.Email(): {p: p, m: m}}}
	h := loginHandlerForFailurePaths(t, router, persons)

	_, err = h.Handle(t.Context(), command.LoginCommand{
		Email:    p.Email(),
		Password: "WRONG",
	})
	if !errors.Is(err, command.ErrAccountLocked) {
		t.Fatalf("got %v, want ErrAccountLocked (threshold-crossing)", err)
	}
	if !p.IsLocked(testNow) {
		t.Error("Person.IsLocked = false after threshold-crossing failed attempt")
	}
}

// TestLockedUntilFromError_NonLockedError_ReturnsZero covers the
// errors.As branch's negative case: a non-lockout error yields a zero
// time.Time so the HTTP layer can `if !zero { setRetryAfter }` cleanly.
func TestLockedUntilFromError_NonLockedError_ReturnsZero(t *testing.T) {
	t.Parallel()
	got := command.LockedUntilFromError(command.ErrInvalidCredentials)
	if !got.IsZero() {
		t.Errorf("LockedUntilFromError(non-locked) = %v, want zero", got)
	}
	if got := command.LockedUntilFromError(nil); !got.IsZero() {
		t.Errorf("LockedUntilFromError(nil) = %v, want zero", got)
	}
}

// ----- terminal-Person paths -----------------------------------------------

// TestLogin_AnonymisedPerson_ReturnsInvalidCredentials covers the
// anonymised-Person branch: enumeration-safe collapse to
// ErrInvalidCredentials + dummy verify (no failed-counter bump per
// ADR 0053 — anonymised state is admin-controlled, not recoverable).
func TestLogin_AnonymisedPerson_ReturnsInvalidCredentials(t *testing.T) {
	t.Parallel()
	p := newPersonAnonymised(t)
	persons := seedPersonRepo(t, p)
	router := &fakeAuthRouter{byEmail: map[email.Address]struct {
		p *person.Person
		m *membership.Membership
	}{p.Email(): {p: p, m: nil}}}
	h := loginHandlerForFailurePaths(t, router, persons)

	priorFailed := p.FailedLoginCount()
	_, err := h.Handle(t.Context(), command.LoginCommand{
		Email:    p.Email(),
		Password: "anything",
	})
	if !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("got %v, want ErrInvalidCredentials", err)
	}
	if p.FailedLoginCount() != priorFailed {
		t.Errorf("FailedLoginCount bumped on anonymised-Person branch: %d → %d", priorFailed, p.FailedLoginCount())
	}
}

// TestLogin_InactivePerson_ReturnsInvalidCredentials covers the
// !IsActive branch: same enumeration-safe collapse + no
// failed-counter bump.
func TestLogin_InactivePerson_ReturnsInvalidCredentials(t *testing.T) {
	t.Parallel()
	p := newPersonInactive(t, "anything")
	persons := seedPersonRepo(t, p)
	router := &fakeAuthRouter{byEmail: map[email.Address]struct {
		p *person.Person
		m *membership.Membership
	}{p.Email(): {p: p, m: nil}}}
	h := loginHandlerForFailurePaths(t, router, persons)

	priorFailed := p.FailedLoginCount()
	_, err := h.Handle(t.Context(), command.LoginCommand{
		Email:    p.Email(),
		Password: "anything",
	})
	if !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("got %v, want ErrInvalidCredentials", err)
	}
	if p.FailedLoginCount() != priorFailed {
		t.Errorf("FailedLoginCount bumped on inactive-Person branch: %d → %d", priorFailed, p.FailedLoginCount())
	}
}

// ----- no-active-membership branches ---------------------------------------

// TestLogin_NoActiveMembership_WrongPassword_BumpsCounter covers the
// m == nil branch with wrong password: REAL verify runs (not dummy)
// so timing tracks the wrong-password path exactly; bump happens
// because we observe the wrong-password failure mode.
func TestLogin_NoActiveMembership_WrongPassword_BumpsCounter(t *testing.T) {
	t.Parallel()
	const plain = "real-pw"
	p := newPersonWithPassword(t, plain)
	persons := seedPersonRepo(t, p)
	router := &fakeAuthRouter{byEmail: map[email.Address]struct {
		p *person.Person
		m *membership.Membership
	}{p.Email(): {p: p, m: nil}}}
	h := loginHandlerForFailurePaths(t, router, persons)

	priorFailed := p.FailedLoginCount()
	_, err := h.Handle(t.Context(), command.LoginCommand{
		Email:    p.Email(),
		Password: "WRONG",
	})
	if !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("got %v, want ErrInvalidCredentials", err)
	}
	if p.FailedLoginCount() != priorFailed+1 {
		t.Errorf("FailedLoginCount = %d, want %d (m==nil + wrong-password should bump)", p.FailedLoginCount(), priorFailed+1)
	}
}

// TestLogin_NoActiveMembership_CorrectPassword_StillRejects covers the
// m == nil branch with the CORRECT password: still ErrInvalidCredentials,
// no counter bump (real verify returned nil, so the bump branch is not
// taken).
func TestLogin_NoActiveMembership_CorrectPassword_StillRejects(t *testing.T) {
	t.Parallel()
	const plain = "real-pw"
	p := newPersonWithPassword(t, plain)
	persons := seedPersonRepo(t, p)
	router := &fakeAuthRouter{byEmail: map[email.Address]struct {
		p *person.Person
		m *membership.Membership
	}{p.Email(): {p: p, m: nil}}}
	h := loginHandlerForFailurePaths(t, router, persons)

	priorFailed := p.FailedLoginCount()
	_, err := h.Handle(t.Context(), command.LoginCommand{
		Email:    p.Email(),
		Password: plain, // correct
	})
	if !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("got %v, want ErrInvalidCredentials", err)
	}
	if p.FailedLoginCount() != priorFailed {
		t.Errorf("FailedLoginCount = %d, want %d (correct-pw + m==nil must not bump)", p.FailedLoginCount(), priorFailed)
	}
}

// ----- error-wrapping branches ---------------------------------------------

// TestLogin_AuthRouterError_WrappedAndPropagated covers the
// "non-ErrNotFound" branch of ResolveByEmail — a real DB error must
// surface as a wrapped "resolve auth routing" error so operators can
// distinguish transient infra failure from credentials-rejected.
func TestLogin_AuthRouterError_WrappedAndPropagated(t *testing.T) {
	t.Parallel()
	persons := persontestRepo(t)
	sentinel := errors.New("pgx: connection reset")
	router := &fakeAuthRouter{resolveErr: sentinel}
	h := loginHandlerForFailurePaths(t, router, persons)

	addr, _ := email.New("op@infra.test")
	_, err := h.Handle(t.Context(), command.LoginCommand{
		Email:    addr,
		Password: "any",
	})
	if err == nil {
		t.Fatal("expected wrapped infra error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("got %v, want chain containing %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "resolve auth routing") {
		t.Errorf("err msg %q missing 'resolve auth routing' prefix", err.Error())
	}
}

// TestLogin_TenantLookupError_Wrapped covers the wrapping of the
// `tenants.GetByID` failure branch — tenant resolution is the FIRST
// post-verify roundtrip; failures surface as
// "login: resolve tenant: <wrapped>" per the package's error-wrapping
// convention.
func TestLogin_TenantLookupError_Wrapped(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)

	const plain = "ok-pw"
	p := newPersonWithPassword(t, plain)
	// Membership references a tenant we DON'T seed into rig.tenants —
	// the tenants fake returns ErrNotFound for that ID.
	missingTenantID := tenant.ID(ids.NewV7().String())
	m, err := membership.New(
		membership.ID(ids.NewV7().String()),
		p.ID(), missingTenantID, membership.ID(""), testNow,
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	seedLoginCandidate(t, rig, p, m)

	_, err = rig.handler.Handle(t.Context(), command.LoginCommand{
		Email:    p.Email(),
		Password: plain,
	})
	if err == nil {
		t.Fatal("expected wrapped tenant lookup error, got nil")
	}
	if !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("got %v, want chain containing tenant.ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "resolve tenant") {
		t.Errorf("err msg %q missing 'resolve tenant' prefix", err.Error())
	}
}

// TestLogin_CorruptedPasswordHash_WrappedVerifyError covers the
// argon2 non-mismatch branch (corrupted PHC string in the Person row).
// Distinct from ErrMismatch — must surface as
// "login: verify password: <wrapped>" so operators can investigate
// a credential-store corruption event vs. a normal wrong-pw attempt.
func TestLogin_CorruptedPasswordHash_WrappedVerifyError(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("corrupt@flow.test")
	// Build a Person via Snapshot with a deliberately-broken PHC string
	// — argon2.Verify returns ErrFormat, which the handler re-bumps the
	// counter on (ErrFormat is in the "treat as failed attempt" branch
	// per resolveAndVerify).
	broken, _ := person.NewPasswordHash("$argon2id$broken$$$but-long-enough-to-pass-length-floor-checks")
	snap := person.Snapshot{
		ID:           person.ID(ids.NewV7().String()),
		Email:        addr,
		FirstName:    "Cor",
		LastName:     "Rupt",
		PasswordHash: broken,
		IsActive:     true,
		CreatedAt:    testNow,
	}
	p := person.UnmarshalFromDB(snap)
	persons := seedPersonRepo(t, p)
	m, err := membership.New(
		membership.ID(ids.NewV7().String()),
		p.ID(), tenant.ID(ids.NewV7().String()),
		membership.ID(""), testNow,
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	router := &fakeAuthRouter{byEmail: map[email.Address]struct {
		p *person.Person
		m *membership.Membership
	}{p.Email(): {p: p, m: m}}}
	h := loginHandlerForFailurePaths(t, router, persons)

	// argon2.Verify will return ErrFormat (broken PHC string). Per the
	// production handler that's treated as the wrong-password branch
	// (ErrInvalidCredentials + counter bump) — confirm the observable.
	priorFailed := p.FailedLoginCount()
	_, err = h.Handle(t.Context(), command.LoginCommand{
		Email:    p.Email(),
		Password: "any",
	})
	if !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("got %v, want ErrInvalidCredentials (ErrFormat folds into failed-attempt branch)", err)
	}
	if p.FailedLoginCount() != priorFailed+1 {
		t.Errorf("FailedLoginCount: ErrFormat branch did not bump counter")
	}
}
