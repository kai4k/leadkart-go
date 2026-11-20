// refresh_test.go — handler-unit tests for the RefreshHandler. Covers
// every branch of refresh.go::Handle per ADR 0062 §6 (handler-unit
// MANY). The reuse-detection branch (RFC 9700 §4.13) is security-load-
// bearing: covered both by integration (flow_integration_test.go) AND
// here at the unit layer per the doctrine that security invariants
// MUST have unit-layer coverage that exercises the orchestration
// directly, not just the end-to-end flow.
//
// Wired against the per-aggregate fakes (refreshtokentest,
// persontest, membershiptest, tenanttest, roletest) + a real test-
// signed JWT issuer. No SQL contract is exercised — per TDL canon §6
// + ADR 0062 the orchestration coverage belongs at the fake-backed
// unit layer.

package command_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/app/permissions"
	"github.com/leadkart/leadkart-go/internal/identity/app/refreshmint"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken/refreshtokentest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role/roletest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

// refreshTestSigningKey is the deterministic HS256 secret used by
// refresh-handler tests. Scoped to refresh_test.go so it doesn't
// collide with login_test.go's loginTestSigningKey.
//
// nolint:gochecknoglobals // test fixture constant
var refreshTestSigningKey = jwt.SigningKey{
	KeyID:  "refresh-test-key",
	Secret: []byte("refresh-test-secret-bytes-32-plus-bytes"),
}

// ----- error-injecting decorators ------------------------------------------
//
// The per-aggregate fakes don't expose error-injection seams — they're
// faithful happy-path mirrors of the SQL adapter. For the wrapping +
// best-effort-revoke branches in refresh.go we need targeted error
// injection at GetByTokenHash / GetByID / UpdateByID / GetActiveForPerson.
// Defined inline (per coordination warning) so this file owns the
// injection surface without touching the shared fakes.

type rtErrFamilies struct {
	*refreshtokentest.FakeRepository
	getByHashErr  error
	updateByIDErr error
}

func (r *rtErrFamilies) GetByTokenHash(ctx context.Context, hash refreshtoken.TokenHash) (*refreshtoken.Family, error) {
	if r.getByHashErr != nil {
		return nil, r.getByHashErr
	}
	return r.FakeRepository.GetByTokenHash(ctx, hash)
}

func (r *rtErrFamilies) UpdateByID(ctx context.Context, id refreshtoken.FamilyID, fn func(*refreshtoken.Family) (bool, error)) error {
	if r.updateByIDErr != nil {
		return r.updateByIDErr
	}
	return r.FakeRepository.UpdateByID(ctx, id, fn)
}

type personErrRepo struct {
	*persontest.FakeRepository
	getByIDErr error
	// afterGetByID runs after a successful read: it stands in for a writer that
	// commits right after the handler read the person.
	afterGetByID func()
}

func (r *personErrRepo) GetByID(ctx context.Context, id person.ID) (*person.Person, error) {
	if r.getByIDErr != nil {
		return nil, r.getByIDErr
	}
	p, err := r.FakeRepository.GetByID(ctx, id)
	if err == nil && r.afterGetByID != nil {
		r.afterGetByID()
	}
	return p, err
}

type tenantErrRepo struct {
	*tenanttest.FakeRepository
	getByIDErr error
}

func (r *tenantErrRepo) GetByID(ctx context.Context, id tenant.ID) (*tenant.Tenant, error) {
	if r.getByIDErr != nil {
		return nil, r.getByIDErr
	}
	return r.FakeRepository.GetByID(ctx, id)
}

type membershipErrRepo struct {
	*membershiptest.FakeRepository
	getActiveForPersonErr error
	overrideActive        *membership.Membership // if set, returned by GetActiveForPerson
}

func (r *membershipErrRepo) GetActiveForPerson(ctx context.Context, pid person.ID) (*membership.Membership, error) {
	if r.getActiveForPersonErr != nil {
		return nil, r.getActiveForPersonErr
	}
	if r.overrideActive != nil {
		return r.overrideActive, nil
	}
	return r.FakeRepository.GetActiveForPerson(ctx, pid)
}

// ----- rig builder ---------------------------------------------------------

type refreshRig struct {
	handler     command.RefreshHandler
	families    *rtErrFamilies
	persons     *personErrRepo
	memberships *membershipErrRepo
	tenants     *tenantErrRepo
	roles       *roletest.FakeRepository
	issuer      *jwt.Issuer
}

func newRefreshRig(t *testing.T) *refreshRig {
	t.Helper()
	clock := testNow
	return newRefreshRigGrace(t, 0, &clock)
}

// refreshTestSuccessorKey is the successor HMAC key the rigs wire (32 bytes).
var refreshTestSuccessorKey = []byte("0123456789abcdef0123456789abcdef")

// newRefreshRigGrace builds the rig with a grace window and a clock the test
// advances through the pointer. Grace 0 is the strict rotation.
func newRefreshRigGrace(t *testing.T, grace time.Duration, clock *time.Time) *refreshRig {
	t.Helper()
	return newRefreshRigIdleGrace(t, 14*24*time.Hour, grace, clock)
}

// newRefreshRigIdleGrace is the rig with an explicit idle lifetime (SlidingTTL); the
// absolute lifetime is 14 days. An idle lifetime equal to the absolute one is the
// pre-idle-timeout behaviour the older tests were written against.
func newRefreshRigIdleGrace(t *testing.T, idle, grace time.Duration, clock *time.Time) *refreshRig {
	t.Helper()
	now := func() time.Time { return *clock }
	issuer, err := jwt.NewIssuer(refreshTestSigningKey, nil, now)
	if err != nil {
		t.Fatalf("jwt.NewIssuer: %v", err)
	}
	families := &rtErrFamilies{FakeRepository: refreshtokentest.NewFakeRepository()}
	persons := &personErrRepo{FakeRepository: persontest.NewFakeRepository()}
	memberships := &membershipErrRepo{FakeRepository: membershiptest.NewFakeRepository()}
	tenants := &tenantErrRepo{FakeRepository: tenanttest.NewFakeRepository()}
	roles := roletest.NewFakeRepository()
	resolver := permissions.NewResolver(memberships, roles, now)

	h := command.NewRefreshHandler(
		families, persons, memberships, tenants, resolver, issuer,
		now, 14*24*time.Hour, idle, grace, refreshTestSuccessorKey,
	)
	return &refreshRig{
		handler: h, families: families, persons: persons,
		memberships: memberships, tenants: tenants, roles: roles, issuer: issuer,
	}
}

// seedFamilyForPerson mints a fresh refresh family for the supplied
// Person + Tenant + returns the plaintext token (caller presents this
// to the handler). Mirrors what LoginHandler does so the rig matches
// production wiring.
func seedFamilyForPerson(t *testing.T, rig *refreshRig, p *person.Person, tn *tenant.Tenant) string {
	t.Helper()
	return seedFamilyForPersonTTL(t, rig, p, tn, 14*24*time.Hour)
}

// seedFamilyForPersonTTL is seedFamilyForPerson with the first token's lifetime
// (login mints it with the idle lifetime).
func seedFamilyForPersonTTL(t *testing.T, rig *refreshRig, p *person.Person, tn *tenant.Tenant, ttl time.Duration) string {
	t.Helper()
	pair, err := refreshmint.Mint()
	if err != nil {
		t.Fatalf("refreshmint.Mint: %v", err)
	}
	fam, err := refreshtoken.NewFamily(
		refreshtoken.FamilyID(ids.NewV7().String()),
		p.ID(), tn.ID(),
		"test-device", pair.Hash,
		ttl, testNow,
	)
	if err != nil {
		t.Fatalf("NewFamily: %v", err)
	}
	if err := rig.families.Add(t.Context(), fam); err != nil {
		t.Fatalf("families.Add: %v", err)
	}
	return pair.Plaintext
}

// seedFullRefreshFixture seeds the rig with everything needed for a
// happy-path Refresh: Person + Tenant + active Membership + a refresh
// family. Returns the plaintext token + the seeded entities for
// post-Handle assertions.
type refreshFixture struct {
	plaintext string
	person    *person.Person
	tenant    *tenant.Tenant
	mem       *membership.Membership
}

func seedFullRefreshFixture(t *testing.T, rig *refreshRig) refreshFixture {
	t.Helper()
	return seedFullRefreshFixtureTTL(t, rig, 14*24*time.Hour)
}

func seedFullRefreshFixtureTTL(t *testing.T, rig *refreshRig, ttl time.Duration) refreshFixture {
	t.Helper()
	p := newPersonWithPassword(t, "anything")
	if err := rig.persons.Add(t.Context(), p); err != nil {
		t.Fatalf("persons.Add: %v", err)
	}
	tn := newTenantWithSlug(t, "refresh-acme")
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenants.Add: %v", err)
	}
	m := activeMembership(t, p, tn)
	if err := rig.memberships.Add(t.Context(), m); err != nil {
		t.Fatalf("memberships.Add: %v", err)
	}
	pt := seedFamilyForPersonTTL(t, rig, p, tn, ttl)
	return refreshFixture{plaintext: pt, person: p, tenant: tn, mem: m}
}

// ----- happy path ----------------------------------------------------------

// TestRefresh_HappyPath_RotatesTokensAndReissuesJWT covers the full
// success branch: hash lookup, rotation closure runs without error,
// downstream Person/Tenant/Membership resolved, JWT reissued. State
// assertions: new tokens are different from old; presented hash is
// CONSUMED in the family; new hash is now the current token.
func TestRefresh_HappyPath_RotatesTokensAndReissuesJWT(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	fx := seedFullRefreshFixture(t, rig)

	res, err := rig.handler.Handle(t.Context(), command.RefreshCommand{
		RefreshTokenPlain: fx.plaintext,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.AccessToken == "" {
		t.Error("AccessToken empty")
	}
	if res.RefreshTokenPlain == "" {
		t.Error("RefreshTokenPlain empty")
	}
	if res.RefreshTokenPlain == fx.plaintext {
		t.Error("RefreshTokenPlain unchanged — rotation did not run")
	}
	if res.PersonID != fx.person.ID() {
		t.Errorf("PersonID = %q, want %q", res.PersonID, fx.person.ID())
	}
	if res.TenantID != fx.tenant.ID() {
		t.Errorf("TenantID = %q, want %q", res.TenantID, fx.tenant.ID())
	}
	if res.MembershipID != fx.mem.ID() {
		t.Errorf("MembershipID = %q, want %q", res.MembershipID, fx.mem.ID())
	}

	// JWT claim-shape lock.
	claims, err := rig.issuer.Verify(res.AccessToken)
	if err != nil {
		t.Fatalf("issuer.Verify: %v", err)
	}
	if claims.TenantSlug != fx.tenant.Slug().String() {
		t.Errorf("tenant_slug = %q", claims.TenantSlug)
	}
	if claims.IsPlatform {
		t.Error("IsPlatform=true for non-platform tenant — refresh regression")
	}
}

// TestRefresh_MustChangePassword_SurvivesRefreshAndClearsAfterChange: refresh mints
// from the stored Person like login (plan D8), so the flag is in the new token and
// in the result while the credential is temporary, and is gone once the Person has
// changed it.
func TestRefresh_MustChangePassword_SurvivesRefreshAndClearsAfterChange(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	hash, err := person.NewPasswordHash("$argon2id$v=19$m=19456,t=2,p=1$c2FsdHkx$abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatalf("NewPasswordHash: %v", err)
	}
	addr, _ := email.New("invited-refresh@acme.test")
	p, err := person.NewWithMustChangePassword(person.ID(ids.NewV7().String()), addr, "Invited", "User", hash, testNow)
	if err != nil {
		t.Fatalf("NewWithMustChangePassword: %v", err)
	}
	if err := rig.persons.Add(t.Context(), p); err != nil {
		t.Fatalf("persons.Add: %v", err)
	}
	tn := newTenantWithSlug(t, "refresh-flagged")
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenants.Add: %v", err)
	}
	if err := rig.memberships.Add(t.Context(), activeMembership(t, p, tn)); err != nil {
		t.Fatalf("memberships.Add: %v", err)
	}
	plain := seedFamilyForPerson(t, rig, p, tn)

	res, err := rig.handler.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: plain})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	claims, err := rig.issuer.Verify(res.AccessToken)
	if err != nil {
		t.Fatalf("issuer.Verify: %v", err)
	}
	if !res.MustChangePassword || !claims.MustChangePassword {
		t.Fatalf("flagged person: result=%v claim=%v, want both true", res.MustChangePassword, claims.MustChangePassword)
	}

	// The person changes the password: the next refresh carries no flag.
	newHash, err := person.NewPasswordHash("$argon2id$v=19$m=19456,t=2,p=1$c2FsdHky$abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatalf("NewPasswordHash: %v", err)
	}
	if err := rig.persons.UpdateByID(t.Context(), p.ID(), func(stored *person.Person) (bool, error) {
		return true, stored.ChangePassword(newHash, testNow)
	}); err != nil {
		t.Fatalf("persons.UpdateByID: %v", err)
	}
	res, err = rig.handler.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: res.RefreshTokenPlain})
	if err != nil {
		t.Fatalf("Handle after change: %v", err)
	}
	claims, err = rig.issuer.Verify(res.AccessToken)
	if err != nil {
		t.Fatalf("issuer.Verify: %v", err)
	}
	if res.MustChangePassword || claims.MustChangePassword {
		t.Fatalf("after change: result=%v claim=%v, want both false", res.MustChangePassword, claims.MustChangePassword)
	}
}

// TestRefresh_RankClaim_MintedLikeLogin: refresh re-mints the rank claim from
// the member's CURRENT roles (ADR 0069), so a promotion or demotion shows at
// the next refresh; no role gives RankNone (99).
func TestRefresh_RankClaim_MintedLikeLogin(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	fx := seedFullRefreshFixture(t, rig)
	r, err := role.New(role.ID(ids.NewV7().String()), fx.tenant.ID(), "Mgr", false, 20, false, testNow)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	if err := rig.roles.Add(t.Context(), r); err != nil {
		t.Fatalf("roles.Add: %v", err)
	}
	if err := fx.mem.AssignRole(r.ID(), testNow); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	res, err := rig.handler.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: fx.plaintext})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	claims, err := rig.issuer.Verify(res.AccessToken)
	if err != nil {
		t.Fatalf("issuer.Verify: %v", err)
	}
	if claims.Rank == nil || *claims.Rank != 20 {
		t.Fatalf("rank claim = %v, want 20", claims.Rank)
	}
}

// ----- malformed / unknown / lookup-error branches -------------------------

// TestRefresh_MalformedPlaintext_RejectsCleanly covers the
// refreshtoken.NewTokenHash error branch — refreshmint.HashOf always
// returns a non-empty hex (SHA-256 of even an empty string is 64
// chars), so the only way to trigger the hash-construction error
// branch is via the empty-plaintext case where HashOf still returns
// a valid hex hash and the lookup misses → ErrRefreshRejected. We
// exercise this to lock the surface even though hash construction
// itself never fails on a non-empty plaintext.
func TestRefresh_MalformedPlaintext_RejectsCleanly(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)

	_, err := rig.handler.Handle(t.Context(), command.RefreshCommand{
		RefreshTokenPlain: "", // empty — hash construction succeeds (HashOf("") is valid hex); lookup misses.
	})
	if !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("got %v, want ErrRefreshRejected", err)
	}
}

// TestRefresh_UnknownHash_ReturnsErrRefreshRejected covers the
// ErrNotFound branch on GetByTokenHash: a forged / stale-from-other-
// session token surfaces as the single ErrRefreshRejected.
func TestRefresh_UnknownHash_ReturnsErrRefreshRejected(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)

	_, err := rig.handler.Handle(t.Context(), command.RefreshCommand{
		RefreshTokenPlain: "some-not-in-fake-token-value",
	})
	if !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("got %v, want ErrRefreshRejected", err)
	}
}

// TestRefresh_LookupError_WrappedAndPropagated covers the non-
// ErrNotFound branch of GetByTokenHash — a real DB error surfaces
// as a wrapped "refresh: lookup family" error.
func TestRefresh_LookupError_WrappedAndPropagated(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	sentinel := errors.New("pgx: connection reset")
	rig.families.getByHashErr = sentinel

	_, err := rig.handler.Handle(t.Context(), command.RefreshCommand{
		RefreshTokenPlain: "any",
	})
	if err == nil {
		t.Fatal("expected wrapped lookup error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("got %v, want chain containing %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "lookup family") {
		t.Errorf("err msg %q missing 'lookup family' prefix", err.Error())
	}
}

// ----- rotation branches ---------------------------------------------------

// TestRefresh_ReuseDetected_RevokesFamilyAndReturnsRejected is the
// security-critical RFC 9700 §4.13 branch. Pre-seed a family +
// rotate once so the original token is consumed; presenting the
// ORIGINAL plaintext again triggers reuse detection. Assertions:
//   - handler returns ErrRefreshRejected (enumeration-safe)
//   - the family is REVOKED in the families fake (the post-Rotate
//     side effect persisted)
func TestRefresh_ReuseDetected_RevokesFamilyAndReturnsRejected(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	fx := seedFullRefreshFixture(t, rig)

	// First refresh — legitimate rotation.
	first, err := rig.handler.Handle(t.Context(), command.RefreshCommand{
		RefreshTokenPlain: fx.plaintext,
	})
	if err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	_ = first

	// Replay original plaintext — should trigger reuse detection.
	_, err = rig.handler.Handle(t.Context(), command.RefreshCommand{
		RefreshTokenPlain: fx.plaintext,
	})
	if !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("got %v, want ErrRefreshRejected on reuse", err)
	}
	// Family revoked side effect persisted.
	fams, err := rig.families.ListActiveForPerson(t.Context(), fx.person.ID())
	if err != nil {
		t.Fatalf("ListActiveForPerson: %v", err)
	}
	if len(fams) != 0 {
		t.Fatalf("ListActiveForPerson: got %d families, want 0 (family must be revoked)", len(fams))
	}
}

// TestRefresh_OtherRotateError_NoPersistAndReturnsRejected covers
// the rotate-error-but-not-reuse branch: revoked family, expired
// token, unknown-token-in-family. Per refresh.go: no state change to
// persist; surface ErrRefreshRejected directly. We exercise the
// "family already revoked" path — revoke the family out-of-band,
// then attempt a refresh with the still-valid plaintext.
func TestRefresh_OtherRotateError_NoPersistAndReturnsRejected(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	fx := seedFullRefreshFixture(t, rig)

	// Out-of-band revoke the family to put Rotate into the ErrRevoked
	// branch.
	fam, err := rig.families.GetByTokenHash(t.Context(), refreshmintHashOf(t, fx.plaintext))
	if err != nil {
		t.Fatalf("GetByTokenHash: %v", err)
	}
	if err := rig.families.UpdateByID(t.Context(), fam.ID(), func(f *refreshtoken.Family) (bool, error) {
		return true, f.Revoke("admin-out-of-band", testNow)
	}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	_, err = rig.handler.Handle(t.Context(), command.RefreshCommand{
		RefreshTokenPlain: fx.plaintext,
	})
	if !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("got %v, want ErrRefreshRejected on revoked-family rotate", err)
	}
}

// TestRefresh_UpdateByIDError_Wrapped covers the families.UpdateByID
// plumbing-error branch — surfaces as a wrapped "refresh: persist
// rotation" error.
func TestRefresh_UpdateByIDError_Wrapped(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	fx := seedFullRefreshFixture(t, rig)

	sentinel := errors.New("pgx: serialization failure")
	rig.families.updateByIDErr = sentinel

	_, err := rig.handler.Handle(t.Context(), command.RefreshCommand{
		RefreshTokenPlain: fx.plaintext,
	})
	if err == nil {
		t.Fatal("expected wrapped UpdateByID error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("got %v, want chain containing %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "persist rotation") {
		t.Errorf("err msg %q missing 'persist rotation' prefix", err.Error())
	}
}

// ----- post-rotate aggregate-state branches -------------------------------

// TestRefresh_PersonDeactivated_RevokesFamilyAndRejects covers the
// post-rotate !IsActive() branch — operator deactivated the Person
// AFTER family creation; the refresh kills the family with reason
// "person-no-longer-active" + returns ErrRefreshRejected.
func TestRefresh_PersonDeactivated_RevokesFamilyAndRejects(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	// Build Person as deactivated FROM THE START — refresh resolves
	// Person AFTER rotation, so the inactive flag triggers the revoke
	// branch.
	plain := "anything"
	p := newPersonInactive(t, plain)
	if err := rig.persons.Add(t.Context(), p); err != nil {
		t.Fatalf("persons.Add: %v", err)
	}
	tn := newTenantWithSlug(t, "deactivated-acme")
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenants.Add: %v", err)
	}
	m := activeMembership(t, p, tn)
	if err := rig.memberships.Add(t.Context(), m); err != nil {
		t.Fatalf("memberships.Add: %v", err)
	}
	pt := seedFamilyForPerson(t, rig, p, tn)

	_, err := rig.handler.Handle(t.Context(), command.RefreshCommand{
		RefreshTokenPlain: pt,
	})
	if !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("got %v, want ErrRefreshRejected for deactivated Person", err)
	}
	// Family revoked side effect persisted.
	fams, err := rig.families.ListActiveForPerson(t.Context(), p.ID())
	if err != nil {
		t.Fatalf("ListActiveForPerson: %v", err)
	}
	if len(fams) != 0 {
		t.Fatalf("ListActiveForPerson: got %d, want 0 (family must be revoked)", len(fams))
	}
}

// TestRefresh_PersonAnonymised_RevokesFamilyAndRejects covers the
// post-rotate IsAnonymised() branch — DPDP/GDPR right-to-erasure
// applied; refresh kills the family + rejects.
func TestRefresh_PersonAnonymised_RevokesFamilyAndRejects(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	p := newPersonAnonymised(t)
	if err := rig.persons.Add(t.Context(), p); err != nil {
		t.Fatalf("persons.Add: %v", err)
	}
	tn := newTenantWithSlug(t, "anon-acme")
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenants.Add: %v", err)
	}
	m := activeMembership(t, p, tn)
	if err := rig.memberships.Add(t.Context(), m); err != nil {
		t.Fatalf("memberships.Add: %v", err)
	}
	pt := seedFamilyForPerson(t, rig, p, tn)

	_, err := rig.handler.Handle(t.Context(), command.RefreshCommand{
		RefreshTokenPlain: pt,
	})
	if !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("got %v, want ErrRefreshRejected for anonymised Person", err)
	}
	fams, err := rig.families.ListActiveForPerson(t.Context(), p.ID())
	if err != nil {
		t.Fatalf("ListActiveForPerson: %v", err)
	}
	if len(fams) != 0 {
		t.Fatalf("ListActiveForPerson: got %d, want 0", len(fams))
	}
}

// TestRefresh_PersonLookupError_Wrapped covers the persons.GetByID
// error wrap: "refresh: resolve person: <wrapped>".
func TestRefresh_PersonLookupError_Wrapped(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	fx := seedFullRefreshFixture(t, rig)
	sentinel := errors.New("pgx: timeout")
	rig.persons.getByIDErr = sentinel

	_, err := rig.handler.Handle(t.Context(), command.RefreshCommand{
		RefreshTokenPlain: fx.plaintext,
	})
	if err == nil {
		t.Fatal("expected wrapped person error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("got %v, want chain containing %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "resolve person") {
		t.Errorf("err msg %q missing 'resolve person' prefix", err.Error())
	}
}

// TestRefresh_TenantLookupError_Wrapped covers the tenants.GetByID
// error wrap: "refresh: resolve tenant: <wrapped>".
func TestRefresh_TenantLookupError_Wrapped(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	fx := seedFullRefreshFixture(t, rig)
	sentinel := errors.New("pgx: connection failure")
	rig.tenants.getByIDErr = sentinel

	_, err := rig.handler.Handle(t.Context(), command.RefreshCommand{
		RefreshTokenPlain: fx.plaintext,
	})
	if err == nil {
		t.Fatal("expected wrapped tenant error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("got %v, want chain containing %v", err, sentinel)
	}
	if !strings.Contains(err.Error(), "resolve tenant") {
		t.Errorf("err msg %q missing 'resolve tenant' prefix", err.Error())
	}
}

// TestRefresh_NoActiveMembership_RevokesFamilyAndRejects covers the
// memberships.GetActiveForPerson error branch — Person changed jobs
// and the new tenant's onboarding hasn't completed; family killed
// with reason "no-active-membership".
func TestRefresh_NoActiveMembership_RevokesFamilyAndRejects(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	fx := seedFullRefreshFixture(t, rig)
	// Force the lookup to fail — caller's branch revokes + rejects.
	rig.memberships.getActiveForPersonErr = membership.ErrNotFound

	_, err := rig.handler.Handle(t.Context(), command.RefreshCommand{
		RefreshTokenPlain: fx.plaintext,
	})
	if !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("got %v, want ErrRefreshRejected", err)
	}
	// Toggle off the error so we can inspect the family state.
	rig.memberships.getActiveForPersonErr = nil
	fams, err := rig.families.ListActiveForPerson(t.Context(), fx.person.ID())
	if err != nil {
		t.Fatalf("ListActiveForPerson: %v", err)
	}
	if len(fams) != 0 {
		t.Fatalf("ListActiveForPerson: got %d, want 0", len(fams))
	}
}

// TestRefresh_MembershipChangedTenant_RevokesFamilyAndRejects covers
// the post-rotate active-membership-vs-family-tenant mismatch branch
// — operator's job change moved the active membership to a different
// tenant; old family must die.
func TestRefresh_MembershipChangedTenant_RevokesFamilyAndRejects(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	fx := seedFullRefreshFixture(t, rig)

	// Build a Membership in a DIFFERENT tenant + force it as the active
	// one via the override seam.
	otherTenant := newTenantWithSlug(t, "other-tenant")
	otherMem, err := membership.New(
		membership.ID(ids.NewV7().String()),
		fx.person.ID(), otherTenant.ID(),
		membership.ID(""), testNow,
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	rig.memberships.overrideActive = otherMem

	_, err = rig.handler.Handle(t.Context(), command.RefreshCommand{
		RefreshTokenPlain: fx.plaintext,
	})
	if !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("got %v, want ErrRefreshRejected", err)
	}
	// Toggle off so we can inspect.
	rig.memberships.overrideActive = nil
	fams, err := rig.families.ListActiveForPerson(t.Context(), fx.person.ID())
	if err != nil {
		t.Fatalf("ListActiveForPerson: %v", err)
	}
	if len(fams) != 0 {
		t.Fatalf("ListActiveForPerson: got %d, want 0 (changed-tenant revoke must persist)", len(fams))
	}
}

// TestRefresh_ResolverError_Wrapped covers the resolver.ResolveAuth
// error branch. We force the membership-lookup path inside the
// resolver to fail by deleting the role the membership references —
// the resolver itself never errors on missing roles (it silently
// drops them), so to hit the resolver error branch we substitute a
// roles repo that fails GetByIDs. Inline via a tiny decorator.
func TestRefresh_ResolverError_Wrapped(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	// Seed a role assignment on the membership so the resolver's
	// GetByIDs path is reached.
	fx := seedFullRefreshFixture(t, rig)
	roleID := mustAssignNewRole(t, fx.mem)

	// Replace rig.roles with an error-injecting decorator by re-wiring
	// a fresh handler — the rig's roles handle isn't used by the
	// handler directly (resolver holds it). Build a fresh handler with
	// the injected roles.
	failRoles := &rolesErrRepo{getByIDsErr: errors.New("pgx: roles down")}
	resolver := permissions.NewResolver(rig.memberships, failRoles, func() time.Time { return testNow })
	h := command.NewRefreshHandler(
		rig.families, rig.persons, rig.memberships, rig.tenants,
		resolver, rig.issuer, func() time.Time { return testNow }, 14*24*time.Hour, 14*24*time.Hour,
		0, refreshTestSuccessorKey,
	)
	// Sanity — silence unused.
	_ = roleID

	_, err := h.Handle(t.Context(), command.RefreshCommand{
		RefreshTokenPlain: fx.plaintext,
	})
	if err == nil {
		t.Fatal("expected wrapped resolver error, got nil")
	}
	if !strings.Contains(err.Error(), "resolve permissions") {
		t.Errorf("err msg %q missing 'resolve permissions' prefix", err.Error())
	}
}

// Documented gap — RefreshHandler's `"refresh: issue jwt"` wrap is
// covered by integration tests in flow_integration_test.go, NOT by a
// handler-unit test. Reason: *jwt.Issuer is a concrete struct (not an
// interface) so Issue() failure cannot be deterministically injected
// without rewriting the handler signature. The branch is reachable
// only through programmer error (e.g. empty PersonID, which the
// aggregate ctors reject upstream). Adding a no-op test here just to
// claim a stub-name would trip TestArch_TestsHaveAtLeastOneAssertion.
//
// TODO(future): if jwt.Issuer becomes an interface (single-impl is
// often a smell), add direct unit coverage for the issue-error wrap.

// ----- helpers -------------------------------------------------------------

// refreshmintHashOf re-derives the hash from plaintext via the
// production helper. Used to look up the family by hash for out-of-
// band state manipulation in tests.
func refreshmintHashOf(t *testing.T, plaintext string) refreshtoken.TokenHash {
	t.Helper()
	h, err := refreshtoken.NewTokenHash(refreshmint.HashOf(plaintext))
	if err != nil {
		t.Fatalf("NewTokenHash: %v", err)
	}
	return h
}

// rolesErrRepo is the minimal error-injecting decorator over the role
// repository contract used by the resolver-error wrap test. Implements
// the [role.Repository] interface from the domain package.
type rolesErrRepo struct {
	getByIDsErr error
}

func (r *rolesErrRepo) GetByIDs(_ context.Context, _ tenant.ID, _ []role.ID) ([]*role.Role, error) {
	return nil, r.getByIDsErr
}
func (r *rolesErrRepo) GetByIDsIncludingDeleted(context.Context, tenant.ID, []role.ID) ([]*role.Role, error) {
	return nil, r.getByIDsErr
}
func (r *rolesErrRepo) Add(context.Context, *role.Role) error { return nil }
func (r *rolesErrRepo) UpdateByID(context.Context, tenant.ID, role.ID, func(*role.Role) (bool, error)) error {
	return nil
}
func (r *rolesErrRepo) GetByID(context.Context, tenant.ID, role.ID) (*role.Role, error) {
	return nil, role.ErrNotFound
}
func (r *rolesErrRepo) GetByTenantAndName(context.Context, tenant.ID, string) (*role.Role, error) {
	return nil, role.ErrNotFound
}
func (r *rolesErrRepo) ListByTenant(context.Context, tenant.ID) ([]*role.Role, error) {
	return nil, nil
}

// mustAssignNewRole grants a fresh test role to the membership and
// returns the role ID. Wraps role.New + Membership.AssignRole for the
// resolver-error test.
func mustAssignNewRole(t *testing.T, m *membership.Membership) role.ID {
	t.Helper()
	r, err := role.New(
		role.ID(ids.NewV7().String()), m.TenantID(), "test-role-refresh",
		false, role.HierarchyLevelDefault, false, testNow,
	)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	if err := m.AssignRole(r.ID(), testNow); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	return r.ID()
}

// TestRefresh_PersonGloballySuspended_RevokesFamilyAndRejects: the silent refresh
// must not mint a fresh token for a globally suspended person while the
// suspension's cascade lags (hierarchy round 6).
func TestRefresh_PersonGloballySuspended_RevokesFamilyAndRejects(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	base := newPersonWithPassword(t, "suspended-pw")
	p := person.UnmarshalFromDB(person.Snapshot{
		ID: base.ID(), Email: base.Email(), FirstName: base.FirstName(), LastName: base.LastName(),
		PasswordHash: base.PasswordHash(), SecurityStamp: base.SecurityStamp(), IsActive: true,
		IsGloballySuspended: true, GlobalSuspensionReason: "fraud review", GloballySuspendedAt: testNow, CreatedAt: base.CreatedAt(),
	})
	if err := rig.persons.Add(t.Context(), p); err != nil {
		t.Fatalf("persons.Add: %v", err)
	}
	tn := newTenantWithSlug(t, "susp-acme")
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenants.Add: %v", err)
	}
	if err := rig.memberships.Add(t.Context(), activeMembership(t, p, tn)); err != nil {
		t.Fatalf("memberships.Add: %v", err)
	}
	pt := seedFamilyForPerson(t, rig, p, tn)

	res, err := rig.handler.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: pt})
	if !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("got %v (token %q), want ErrRefreshRejected for a suspended Person", err, res.AccessToken)
	}
	fams, err := rig.families.ListActiveForPerson(t.Context(), p.ID())
	if err != nil || len(fams) != 0 {
		t.Fatalf("ListActiveForPerson: %d families, %v; want the family revoked", len(fams), err)
	}
}

// ----- grace window (ADR 0068 D10) -------------------------------------------

// graceRefresh presents plain from the client "client-A".
func graceRefresh(t *testing.T, rig *refreshRig, plain string) (command.RefreshResult, error) {
	t.Helper()
	return graceRefreshFrom(t, rig, "client-A", plain)
}

// graceRefreshFrom presents plain from the named client (the request context's
// client key; "" is a request with no client).
func graceRefreshFrom(t *testing.T, rig *refreshRig, client, plain string) (command.RefreshResult, error) {
	t.Helper()
	return rig.handler.Handle(argon2.WithClient(t.Context(), client), command.RefreshCommand{RefreshTokenPlain: plain})
}

func TestRefresh_Grace_SameTokenTwiceGetsTheSameSuccessor(t *testing.T) {
	t.Parallel()
	clock := testNow
	rig := newRefreshRigGrace(t, 30*time.Second, &clock)
	fx := seedFullRefreshFixture(t, rig)

	first, err := graceRefresh(t, rig, fx.plaintext)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	clock = clock.Add(10 * time.Second)
	second, err := graceRefresh(t, rig, fx.plaintext)
	if err != nil {
		t.Fatalf("replay inside the window: %v", err)
	}
	if first.RefreshTokenPlain != second.RefreshTokenPlain || first.RefreshTokenPlain == fx.plaintext {
		t.Fatalf("replay must return the same, new successor: %q vs %q", first.RefreshTokenPlain, second.RefreshTokenPlain)
	}
	fams, err := rig.families.ListActiveForPerson(t.Context(), fx.person.ID())
	if err != nil || len(fams) != 1 {
		t.Fatalf("family must stay active: %d %v", len(fams), err)
	}
	if n := len(fams[0].AllTokens()); n != 2 {
		t.Fatalf("a replay writes no rotation: %d tokens, want 2", n)
	}
	// The returned successor is the real chain head: it rotates on.
	if _, err := graceRefresh(t, rig, first.RefreshTokenPlain); err != nil {
		t.Fatalf("successor must be usable: %v", err)
	}
}

func TestRefresh_Grace_OutsideTheWindowIsReuse(t *testing.T) {
	t.Parallel()
	clock := testNow
	rig := newRefreshRigGrace(t, 30*time.Second, &clock)
	fx := seedFullRefreshFixture(t, rig)
	if _, err := graceRefresh(t, rig, fx.plaintext); err != nil {
		t.Fatalf("first: %v", err)
	}
	clock = clock.Add(31 * time.Second)
	if _, err := graceRefresh(t, rig, fx.plaintext); !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("late replay: got %v, want ErrRefreshRejected", err)
	}
	if fams, _ := rig.families.ListActiveForPerson(t.Context(), fx.person.ID()); len(fams) != 0 {
		t.Fatalf("a late replay must revoke the family: %d active", len(fams))
	}
}

func TestRefresh_Grace_TwoGenerationsOldIsReuseInsideTheWindow(t *testing.T) {
	t.Parallel()
	clock := testNow
	rig := newRefreshRigGrace(t, 30*time.Second, &clock)
	fx := seedFullRefreshFixture(t, rig)
	first, err := graceRefresh(t, rig, fx.plaintext)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := graceRefresh(t, rig, first.RefreshTokenPlain); err != nil {
		t.Fatalf("second: %v", err)
	}
	clock = clock.Add(time.Second)
	if _, err := graceRefresh(t, rig, fx.plaintext); !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("a token two generations old: got %v, want ErrRefreshRejected", err)
	}
	if fams, _ := rig.families.ListActiveForPerson(t.Context(), fx.person.ID()); len(fams) != 0 {
		t.Fatalf("family must be revoked: %d active", len(fams))
	}
}

func TestRefresh_Grace_NeverReturnsATokenForARevokedFamily(t *testing.T) {
	t.Parallel()
	clock := testNow
	rig := newRefreshRigGrace(t, 30*time.Second, &clock)
	fx := seedFullRefreshFixture(t, rig)
	first, err := graceRefresh(t, rig, fx.plaintext)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	fam, err := rig.families.GetByTokenHash(t.Context(), refreshmintHashOf(t, fx.plaintext))
	if err != nil {
		t.Fatalf("GetByTokenHash: %v", err)
	}
	if err := rig.families.UpdateByID(t.Context(), fam.ID(), func(f *refreshtoken.Family) (bool, error) {
		return true, f.Revoke("password-changed", clock)
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	res, err := graceRefresh(t, rig, fx.plaintext)
	if !errors.Is(err, command.ErrRefreshRejected) || res.AccessToken != "" || res.RefreshTokenPlain != "" {
		t.Fatalf("replay on a revoked family: %+v %v", res, err)
	}
	if _, err := graceRefresh(t, rig, first.RefreshTokenPlain); !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("successor on a revoked family: %v", err)
	}
	cur, _ := rig.families.GetByID(t.Context(), fam.ID())
	if cur.RevokeReason() != "password-changed" {
		t.Fatalf("revocation reason overwritten: %q", cur.RevokeReason())
	}
}

func TestNewRefreshHandler_ShortSuccessorKeyPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("a successor key under 32 bytes must panic at construction")
		}
	}()
	_ = command.NewRefreshHandler(nil, nil, nil, nil, nil, nil, nil, time.Hour, time.Hour, time.Second, []byte("short")) // arch-test:ignore-err - test fixture setup
}

// ----- absolute lifetime (fix round 13, item 12) -------------------------------

// A session ends at its absolute lifetime, counted from the login, however often it
// was rotated: a family refreshed every day is refused on the first refresh after
// day 14, answers 401 with the session-expired reason, and is revoked.
func TestRefresh_AbsoluteLifetime_EndsAnOtherwiseHealthySession(t *testing.T) {
	t.Parallel()
	const abs = 14 * 24 * time.Hour
	clock := testNow
	rig := newRefreshRigGrace(t, 0, &clock)
	fx := seedFullRefreshFixture(t, rig)

	plain := fx.plaintext
	for day := 1; day <= 13; day++ {
		clock = testNow.Add(time.Duration(day) * 24 * time.Hour)
		res, err := graceRefresh(t, rig, plain)
		if err != nil {
			t.Fatalf("day %d: %v", day, err)
		}
		plain = res.RefreshTokenPlain
	}
	// One second before the deadline: still allowed.
	clock = testNow.Add(abs - time.Second)
	res, err := graceRefresh(t, rig, plain)
	if err != nil {
		t.Fatalf("one second before the absolute lifetime: %v", err)
	}
	plain = res.RefreshTokenPlain

	// One second after: refused, with the distinct reason, and the family is dead.
	clock = testNow.Add(abs + time.Second)
	_, err = graceRefresh(t, rig, plain)
	if !errors.Is(err, command.ErrRefreshRejected) || !errors.Is(err, command.ErrSessionExpired) {
		t.Fatalf("one second after: %v, want ErrRefreshRejected + ErrSessionExpired", err)
	}
	fam, gerr := rig.families.GetByTokenHash(t.Context(), refreshmintHashOf(t, plain))
	if gerr != nil {
		t.Fatal(gerr)
	}
	if !fam.IsRevoked() || fam.RevokeReason() != refreshtoken.RevokeReasonAbsoluteTTL {
		t.Fatalf("family revoked=%v reason=%q, want revoked with absolute_ttl", fam.IsRevoked(), fam.RevokeReason())
	}
	// The reason is only the session's own: a plain unknown token has none.
	if _, err := graceRefresh(t, rig, "unknown-token-plaintext"); errors.Is(err, command.ErrSessionExpired) {
		t.Fatal("an unknown token must not answer session_expired")
	}
}

// ----- grace replay is bound to the client that consumed the token (fix round 13, item 3) --

func TestRefresh_Grace_AnotherClientIsReuse_AndTheFamilyIsRevoked(t *testing.T) {
	t.Parallel()
	clock := testNow
	rig := newRefreshRigGrace(t, 30*time.Second, &clock)
	fx := seedFullRefreshFixture(t, rig)
	first, err := graceRefreshFrom(t, rig, "victim", fx.plaintext)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	clock = clock.Add(2 * time.Second)
	res, err := graceRefreshFrom(t, rig, "thief", fx.plaintext)
	if !errors.Is(err, command.ErrRefreshRejected) || res.RefreshTokenPlain != "" || res.AccessToken != "" {
		t.Fatalf("another client inside the window: %+v %v, want ErrRefreshRejected and no token", res, err)
	}
	// The victim's successor is dead too: the family is revoked.
	if _, err := graceRefreshFrom(t, rig, "victim", first.RefreshTokenPlain); !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("the victim's successor after the theft: %v, want ErrRefreshRejected", err)
	}
	if fams, _ := rig.families.ListActiveForPerson(t.Context(), fx.person.ID()); len(fams) != 0 {
		t.Fatalf("family must be revoked: %d active", len(fams))
	}
}

func TestRefresh_Grace_ThreeReplaysThenReuse(t *testing.T) {
	t.Parallel()
	clock := testNow
	rig := newRefreshRigGrace(t, 30*time.Second, &clock)
	fx := seedFullRefreshFixture(t, rig)
	first, err := graceRefresh(t, rig, fx.plaintext)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	for i := 1; i <= refreshtoken.MaxGraceReplays; i++ {
		clock = clock.Add(time.Second)
		again, err := graceRefresh(t, rig, fx.plaintext)
		if err != nil || again.RefreshTokenPlain != first.RefreshTokenPlain {
			t.Fatalf("replay %d: %v same=%v", i, err, again.RefreshTokenPlain == first.RefreshTokenPlain)
		}
	}
	clock = clock.Add(time.Second)
	if _, err := graceRefresh(t, rig, fx.plaintext); !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("replay %d: %v, want ErrRefreshRejected", refreshtoken.MaxGraceReplays+1, err)
	}
	if fams, _ := rig.families.ListActiveForPerson(t.Context(), fx.person.ID()); len(fams) != 0 {
		t.Fatalf("the budget overrun must revoke the family: %d active", len(fams))
	}
}

// A request that carries no client (a worker, a test without the middleware) can
// rotate, but its token is replayable by nobody.
func TestRefresh_Grace_NoClientMeansNoReplay(t *testing.T) {
	t.Parallel()
	clock := testNow
	rig := newRefreshRigGrace(t, 30*time.Second, &clock)
	fx := seedFullRefreshFixture(t, rig)
	if _, err := graceRefreshFrom(t, rig, "", fx.plaintext); err != nil {
		t.Fatalf("rotate without a client: %v", err)
	}
	if _, err := graceRefreshFrom(t, rig, "", fx.plaintext); !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("replay of a token consumed without a client: %v, want ErrRefreshRejected", err)
	}
}

// Only a keyed hash of the client reaches storage, and it differs per family.
func TestRefresh_Grace_StoresAnHMACOfTheClientNeverTheClient(t *testing.T) {
	t.Parallel()
	clock := testNow
	rig := newRefreshRigGrace(t, 30*time.Second, &clock)
	fx := seedFullRefreshFixture(t, rig)
	const client = "203.0.113.77"
	if _, err := graceRefreshFrom(t, rig, client, fx.plaintext); err != nil {
		t.Fatal(err)
	}
	fam, err := rig.families.GetByTokenHash(t.Context(), refreshmintHashOf(t, fx.plaintext))
	if err != nil {
		t.Fatal(err)
	}
	stored := fam.AllTokens()[0].ConsumedClient()
	bindingKey, err := refreshmint.DeriveClientBindingKey(refreshTestSuccessorKey)
	if err != nil {
		t.Fatal(err)
	}
	if want := refreshmint.ClientBinding(bindingKey, fam.ID(), client); stored != want || stored == "" || strings.Contains(stored, client) {
		t.Fatalf("stored binding %q, want the HMAC %q and no trace of the address", stored, want)
	}
	if other := refreshmint.ClientBinding(bindingKey, refreshtoken.FamilyID("another-family"), client); other == stored {
		t.Fatal("the binding must differ across families")
	}
}

// Every grace presentation is logged (a WARN when refused) with the family and the
// outcome and nothing about the client; the counter is a no-op without a provider.
func TestRefresh_Grace_LogsEveryReplayWithoutTheClient(t *testing.T) {
	t.Parallel()
	clock := testNow
	rig := newRefreshRigGrace(t, 30*time.Second, &clock)
	var buf strings.Builder
	rig.handler = rig.handler.WithLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	fx := seedFullRefreshFixture(t, rig)
	if _, err := graceRefreshFrom(t, rig, "198.51.100.9", fx.plaintext); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("a first rotation is not a replay, got a log line: %s", buf.String())
	}
	if _, err := graceRefreshFrom(t, rig, "198.51.100.9", fx.plaintext); err != nil {
		t.Fatal(err)
	}
	if _, err := graceRefreshFrom(t, rig, "198.51.100.66", fx.plaintext); !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatal("the other client must be refused")
	}
	out := buf.String()
	for _, want := range []string{"outcome=same_client", "level=INFO", "outcome=refused_other_client", "level=WARN", "family_id="} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q in: %s", want, out)
		}
	}
	for _, leak := range []string{"198.51.100", fx.plaintext} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaks %q in: %s", leak, out)
		}
	}
}
