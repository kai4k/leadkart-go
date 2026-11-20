// tenant_status_session_test.go — final fix round, commit 2: the members of a
// suspended or deleted company cannot sign in or refresh, and an idle session ends.
// The tests drive the real handlers over the per-aggregate fakes; the database-level
// flow (registration -> first login, suspension revoking live sessions) is in
// tenant_status_flow_integration_test.go.
package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken/refreshtokentest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

const tenantStatusPlain = "correct horse battery staple"

// ----- login ---------------------------------------------------------------------

// seedTenantMember seeds one member whose tenant the router reports in status.
func seedTenantMember(t *testing.T, rig wiredLoginRig, status tenant.Status) (*person.Person, func() int) {
	t.Helper()
	p := newPersonWithPassword(t, tenantStatusPlain)
	tn := newTenantWithSlug(t, "acme")
	m := activeMembership(t, p, tn)
	seedLoginCandidate(t, rig, p, m)
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenants.Add: %v", err)
	}
	rig.router.tenantStatus = status
	return p, func() int {
		fams, err := rig.families.ListActiveForPerson(t.Context(), p.ID())
		if err != nil {
			t.Fatal(err)
		}
		return len(fams)
	}
}

func loginAs(t *testing.T, rig wiredLoginRig, p *person.Person, password string) error {
	t.Helper()
	_, err := rig.handler.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: password, DeviceLabel: "d"})
	return err
}

func TestLogin_SuspendedTenant_CorrectPasswordIsRefusedAsTenantSuspended(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)
	p, families := seedTenantMember(t, rig, tenant.StatusSuspended)

	err := loginAs(t, rig, p, tenantStatusPlain)
	if !errors.Is(err, command.ErrTenantSuspended) {
		t.Fatalf("suspended tenant, right password = %v, want ErrTenantSuspended", err)
	}
	if errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatal("tenant_suspended must be its own answer, not invalid_credentials")
	}
	if n := families(); n != 0 {
		t.Fatalf("%d refresh families for a refused login, want 0", n)
	}
}

// The distinct answer is only ever given after the password was verified: a wrong
// password for a member of a suspended tenant answers exactly what a wrong password
// answers anywhere else.
func TestLogin_SuspendedTenant_WrongPasswordIsAnOrdinaryWrongPassword(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)
	p, families := seedTenantMember(t, rig, tenant.StatusSuspended)

	err := loginAs(t, rig, p, "not the password at all")
	if err != command.ErrInvalidCredentials { //nolint:errorlint // identity, not wrapping: the surface must be byte-identical
		t.Fatalf("suspended tenant, wrong password = %v, want exactly ErrInvalidCredentials", err)
	}
	if n := families(); n != 0 {
		t.Fatalf("%d refresh families, want 0", n)
	}
}

func TestLogin_DeletedTenant_IsRefusedLikeABadCredential(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)
	p, families := seedTenantMember(t, rig, tenant.StatusDeleted)

	if err := loginAs(t, rig, p, tenantStatusPlain); !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("deleted tenant = %v, want ErrInvalidCredentials", err)
	}
	if n := families(); n != 0 {
		t.Fatalf("%d refresh families, want 0", n)
	}
}

// A newly registered tenant is PENDING and nothing activates it: its first admin
// must still sign in (registration -> first login).
func TestLogin_PendingTenant_FirstAdminSignsIn(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)
	p, families := seedTenantMember(t, rig, tenant.StatusPending)

	if err := loginAs(t, rig, p, tenantStatusPlain); err != nil {
		t.Fatalf("pending tenant, right password = %v, want a session", err)
	}
	if n := families(); n != 1 {
		t.Fatalf("%d families, want 1", n)
	}
}

func TestLogin_PendingDeletionTenant_StillSignsIn_PendingTheOwnersDecision(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)
	p, _ := seedTenantMember(t, rig, tenant.StatusPendingDeletion)
	if err := loginAs(t, rig, p, tenantStatusPlain); err != nil {
		t.Fatalf("pending_deletion tenant = %v, want a session (open owner question, ADR 0069)", err)
	}
}

func TestLogin_ReactivatedTenant_SignsInAgain(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)
	p, families := seedTenantMember(t, rig, tenant.StatusSuspended)
	if err := loginAs(t, rig, p, tenantStatusPlain); !errors.Is(err, command.ErrTenantSuspended) {
		t.Fatalf("while suspended: %v", err)
	}
	rig.router.tenantStatus = tenant.StatusActive
	if err := loginAs(t, rig, p, tenantStatusPlain); err != nil {
		t.Fatalf("after reactivation: %v", err)
	}
	if n := families(); n != 1 {
		t.Fatalf("%d families, want 1", n)
	}
}

// ----- refresh -------------------------------------------------------------------

func suspendTenantOn(t *testing.T, rig *refreshRig, tn *tenant.Tenant) {
	t.Helper()
	if err := rig.tenants.UpdateByID(t.Context(), tn.ID(), func(loaded *tenant.Tenant) (bool, error) {
		return true, loaded.Suspend("payment overdue", testNow)
	}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
}

func TestRefresh_SuspendedTenant_IsRejectedAndTheFamilyRevoked(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	fx := seedFullRefreshFixture(t, rig)
	suspendTenantOn(t, rig, fx.tenant)

	res, err := rig.handler.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: fx.plaintext})
	if !errors.Is(err, command.ErrRefreshRejected) || !errors.Is(err, command.ErrTenantSuspended) {
		t.Fatalf("suspended tenant refresh = %v, want ErrRefreshRejected + ErrTenantSuspended", err)
	}
	if res.AccessToken != "" || res.RefreshTokenPlain != "" {
		t.Fatalf("a token was minted for a suspended tenant: %+v", res)
	}
	fam, gerr := rig.families.GetByTokenHash(t.Context(), refreshmintHashOf(t, fx.plaintext))
	if gerr != nil {
		t.Fatal(gerr)
	}
	if !fam.IsRevoked() || fam.RevokeReason() != "tenant_suspended" {
		t.Fatalf("family revoked=%v reason=%q, want revoked with tenant_suspended", fam.IsRevoked(), fam.RevokeReason())
	}
}

func TestRefresh_DeletedTenant_IsRejectedAndTheFamilyRevoked(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	fx := seedFullRefreshFixture(t, rig)
	if err := rig.tenants.UpdateByID(t.Context(), fx.tenant.ID(), func(loaded *tenant.Tenant) (bool, error) {
		if err := loaded.Activate(testNow); err != nil {
			return false, err
		}
		if err := loaded.MarkForDeletion("closed", testNow); err != nil {
			return false, err
		}
		return true, loaded.HardDelete(testNow.Add(31 * 24 * time.Hour))
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	_, err := rig.handler.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: fx.plaintext})
	if !errors.Is(err, command.ErrRefreshRejected) || errors.Is(err, command.ErrTenantSuspended) {
		t.Fatalf("deleted tenant refresh = %v, want a plain ErrRefreshRejected", err)
	}
	fam, gerr := rig.families.GetByTokenHash(t.Context(), refreshmintHashOf(t, fx.plaintext))
	if gerr != nil {
		t.Fatal(gerr)
	}
	if !fam.IsRevoked() || fam.RevokeReason() != "tenant_deleted" {
		t.Fatalf("family revoked=%v reason=%q, want revoked with tenant_deleted", fam.IsRevoked(), fam.RevokeReason())
	}
}

func TestRefresh_PendingTenant_StillRefreshes(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	fx := seedFullRefreshFixture(t, rig)
	if fx.tenant.Status() != tenant.StatusPending {
		t.Fatalf("fixture tenant is %s, want pending (what registration creates)", fx.tenant.Status())
	}
	if _, err := rig.handler.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: fx.plaintext}); err != nil {
		t.Fatalf("pending tenant refresh: %v", err)
	}
}

// The family revoked while the tenant was suspended stays dead after the tenant is
// reactivated: the member signs in again, the old session does not come back.
func TestRefresh_ReactivatedTenant_DoesNotResurrectTheRevokedFamily(t *testing.T) {
	t.Parallel()
	rig := newRefreshRig(t)
	fx := seedFullRefreshFixture(t, rig)
	suspendTenantOn(t, rig, fx.tenant)
	if _, err := rig.handler.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: fx.plaintext}); !errors.Is(err, command.ErrTenantSuspended) {
		t.Fatalf("while suspended: %v", err)
	}
	if err := rig.tenants.UpdateByID(t.Context(), fx.tenant.ID(), func(loaded *tenant.Tenant) (bool, error) {
		return true, loaded.Activate(testNow)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.handler.Handle(t.Context(), command.RefreshCommand{RefreshTokenPlain: fx.plaintext}); !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("after reactivation the revoked family must stay dead: %v", err)
	}
}

// ----- idle timeout --------------------------------------------------------------

const (
	idleTestIdle = 2 * time.Hour
	idleTestAbs  = 14 * 24 * time.Hour
)

// newIdleRig builds the rig with a real idle lifetime and a family whose first token
// was minted the way login mints it (idle lifetime).
func newIdleRig(t *testing.T, clock *time.Time) (*refreshRig, refreshFixture) {
	t.Helper()
	rig := newRefreshRigIdleGrace(t, idleTestIdle, 30*time.Second, clock)
	return rig, seedFullRefreshFixtureTTL(t, rig, idleTestIdle)
}

func TestRefresh_Idle_AnUnusedSessionEndsAndIsRevoked(t *testing.T) {
	t.Parallel()
	clock := testNow
	rig, fx := newIdleRig(t, &clock)

	// Used one hour in: allowed, and the new token lives a fresh idle lifetime.
	clock = testNow.Add(time.Hour)
	res, err := graceRefresh(t, rig, fx.plaintext)
	if err != nil {
		t.Fatalf("refresh inside the idle lifetime: %v", err)
	}
	// Idle for a second past the idle lifetime counted from THAT refresh.
	clock = clock.Add(idleTestIdle + time.Second)
	_, err = graceRefresh(t, rig, res.RefreshTokenPlain)
	if !errors.Is(err, command.ErrRefreshRejected) || !errors.Is(err, command.ErrSessionExpired) {
		t.Fatalf("idle past the lifetime: %v, want ErrRefreshRejected + ErrSessionExpired", err)
	}
	fam, gerr := rig.families.GetByTokenHash(t.Context(), refreshmintHashOf(t, res.RefreshTokenPlain))
	if gerr != nil {
		t.Fatal(gerr)
	}
	if !fam.IsRevoked() || fam.RevokeReason() != refreshtoken.RevokeReasonIdleTimeout {
		t.Fatalf("family revoked=%v reason=%q, want idle_timeout", fam.IsRevoked(), fam.RevokeReason())
	}
}

func TestRefresh_Idle_ExactlyAtTheIdleDeadlineStillRefreshes(t *testing.T) {
	t.Parallel()
	clock := testNow
	rig, fx := newIdleRig(t, &clock)
	clock = testNow.Add(idleTestIdle)
	if _, err := graceRefresh(t, rig, fx.plaintext); err != nil {
		t.Fatalf("at the idle deadline: %v", err)
	}
}

func TestRefresh_Idle_ActiveUseNeverPassesTheAbsoluteDeadline(t *testing.T) {
	t.Parallel()
	clock := testNow
	rig, fx := newIdleRig(t, &clock)
	plain := fx.plaintext
	for clock.Add(time.Hour).Before(testNow.Add(idleTestAbs)) {
		clock = clock.Add(time.Hour)
		res, err := graceRefresh(t, rig, plain)
		if err != nil {
			t.Fatalf("hourly refresh at %v: %v", clock, err)
		}
		plain = res.RefreshTokenPlain
	}
	clock = testNow.Add(idleTestAbs + time.Second)
	if _, err := graceRefresh(t, rig, plain); !errors.Is(err, command.ErrSessionExpired) {
		t.Fatalf("past the absolute lifetime: %v, want ErrSessionExpired", err)
	}
	fam, gerr := rig.families.GetByTokenHash(t.Context(), refreshmintHashOf(t, plain))
	if gerr != nil {
		t.Fatal(gerr)
	}
	if fam.RevokeReason() != refreshtoken.RevokeReasonAbsoluteTTL {
		t.Fatalf("reason = %q, want absolute_ttl (the absolute deadline came first)", fam.RevokeReason())
	}
}

func TestRefresh_Idle_AGraceReplayExtendsNothing(t *testing.T) {
	t.Parallel()
	clock := testNow
	rig, fx := newIdleRig(t, &clock)
	clock = testNow.Add(time.Hour)
	first, err := graceRefresh(t, rig, fx.plaintext)
	if err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(20 * time.Second)
	if _, err := graceRefresh(t, rig, fx.plaintext); err != nil {
		t.Fatalf("grace replay: %v", err)
	}
	// The successor still dies one idle lifetime after ITS issue, not after the replay.
	clock = testNow.Add(time.Hour).Add(idleTestIdle + time.Second)
	if _, err := graceRefresh(t, rig, first.RefreshTokenPlain); !errors.Is(err, command.ErrSessionExpired) {
		t.Fatalf("successor after its idle lifetime: %v, want ErrSessionExpired (the replay must not extend it)", err)
	}
}

// ----- MaySignIn on the credential-changing commands (item 4) -----------------------

// A deactivated person is refused by change-password and the email-change request
// exactly like an anonymised or suspended one: all of them ask Person.MaySignIn.
func TestChangePassword_DeactivatedPerson_IsRefusedLikeAWrongPassword(t *testing.T) {
	t.Parallel()
	p := newPersonInactive(t, "real-current-pw")
	repo := seedPersonRepo(t, p)
	h := command.NewChangePasswordHandler(repo, refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID: p.ID(), CurrentPassword: "real-current-pw", NewPassword: "Anything-Stronger-Than-Floor-9!",
	})
	if !errors.Is(err, command.ErrIncorrectCurrentPassword) {
		t.Fatalf("deactivated person = %v, want ErrIncorrectCurrentPassword", err)
	}
}

func TestRequestEmailChange_DeactivatedPerson_Rejected(t *testing.T) {
	t.Parallel()
	p := newPersonInactive(t, "irrelevant")
	repo := seedPersonRepo(t, p)
	h := command.NewRequestEmailChangeHandler(repo, func() time.Time { return testNow })
	addr, _ := email.New("new@example.test")
	if err := h.Handle(t.Context(), command.RequestEmailChangeCommand{PersonID: p.ID(), NewEmail: addr}); !errors.Is(err, command.ErrEmailChangeRejected) {
		t.Fatalf("deactivated person = %v, want ErrEmailChangeRejected", err)
	}
}
