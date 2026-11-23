//go:build integration

// arch-test:no-timeout-needed — every request carries t.Context().
//
// arch-test:no-synctest — the idle lifetime is measured on the injected clock (a
// frozen, manually advanced instant); the requests cross HTTP and Postgres.
//
// arch-test:raw-sql-justified — a tenant's status, the families of a tenant and
//   their revocation reasons have no HTTP surface a member could read.

// Final fix round, commit 2: the members of a suspended company cannot sign in or
// refresh, suspending ends their live sessions, reactivation restores sign-in, the
// first admin of a freshly registered (active) company signs in, an operator
// acting into the suspended tenant keeps working, and an idle session ends.
package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func (f e2eFixture) tenantStatusOf(t *testing.T, tenantID string) string {
	t.Helper()
	var s string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM identity.tenants WHERE id = $1`, tenantID).Scan(&s); err != nil {
		t.Fatalf("read tenant status: %v", err)
	}
	return s
}

func (f e2eFixture) activeFamiliesOfTenant(t *testing.T, tenantID string) int {
	t.Helper()
	var n int
	if err := f.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM identity.refresh_token_families WHERE tenant_id = $1 AND revoked_at IS NULL`, tenantID).Scan(&n); err != nil {
		t.Fatalf("count active families: %v", err)
	}
	return n
}

func (f e2eFixture) suspendTenantHTTP(t *testing.T, tenantID string) {
	t.Helper()
	su := f.mintPlatformTokenWith(t, nil, true)
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/tenants/"+tenantID+"/suspend", su, ports.SuspendTenantRequest{Reason: "payment overdue 2026-11"}); r.status != http.StatusNoContent {
		t.Fatalf("suspend: %d %s", r.status, r.body)
	}
}

func (f e2eFixture) activateTenantHTTP(t *testing.T, tenantID string) {
	t.Helper()
	su := f.mintPlatformTokenWith(t, nil, true)
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/tenants/"+tenantID+"/activate", su, nil); r.status != http.StatusNoContent {
		t.Fatalf("activate: %d %s", r.status, r.body)
	}
}

// Registration -> first login. Owner ruling 2026-11-21 ("If a Company is not Active no
// one can login from that org.") plus the controller default (ADR 0069): a company an
// operator registers is created ACTIVE, so its first admin signs in and refreshes.
// Activating an already-active company is a harmless no-op: 204, nothing changes.
func TestE2E_RegisteredTenant_IsActive_FirstAdminSignsInAndRefreshes(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "actfirst")
	if got := f.tenantStatusOf(t, owner.TenantID); got != "active" {
		t.Fatalf("a newly registered tenant is %q, want active", got)
	}
	if owner.AccessToken == "" {
		t.Fatal("the first admin of a registered tenant got no session")
	}
	if st, _ := f.mustRefresh(t, owner.RefreshToken); st != http.StatusOK {
		t.Fatalf("registered tenant refresh: %d, want 200", st)
	}
	var activatedAt *time.Time
	if err := f.Pool.QueryRow(t.Context(), `SELECT activated_at FROM identity.tenants WHERE id = $1`, owner.TenantID).Scan(&activatedAt); err != nil || activatedAt == nil {
		t.Fatalf("activated_at = %v (%v), want it stamped at registration", activatedAt, err)
	}
	f.activateTenantHTTP(t, owner.TenantID) // 204, as for any activation
	var again *time.Time
	if err := f.Pool.QueryRow(t.Context(), `SELECT activated_at FROM identity.tenants WHERE id = $1`, owner.TenantID).Scan(&again); err != nil || again == nil || !again.Equal(*activatedAt) {
		t.Fatalf("activating an active tenant changed activated_at: %v -> %v", activatedAt, again)
	}
	if got := f.tenantStatusOf(t, owner.TenantID); got != "active" {
		t.Fatalf("status %q after activating an active tenant, want active", got)
	}
}

// A tenant that is not active for any reason other than suspension (pending: a future
// self-signup) answers 403 tenant_not_active after the password check, and refresh
// answers 401 refresh_rejected / tenant_not_active with the family revoked as
// tenant_not_active. A wrong password stays the ordinary 401. Operators acting into
// the tenant keep working: activating it brings the members back.
func TestE2E_PendingTenant_MembersCannotSignInOrRefresh_ActivationBringsThemBack(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "notactco")
	m := f.createMember(t, owner, "notactm")
	pair := f.loginPair(t, m.email, m.password)
	su := f.mintPlatformTokenWith(t, nil, true)

	// pending (what a future self-signup would create).
	if _, err := f.Pool.Exec(t.Context(), `UPDATE identity.tenants SET status = 'pending', activated_at = NULL WHERE id = $1`, owner.TenantID); err != nil {
		t.Fatalf("force pending: %v", err)
	}
	if got := f.tenantStatusOf(t, owner.TenantID); got != "pending" {
		t.Fatalf("status %q, want pending", got)
	}

	before := f.familyCount(t, m.personID)
	r := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: m.email, Password: m.password, DeviceLabel: "d"})
	if r.status != http.StatusForbidden || decodeError(t, r.body).Error != ports.ErrCodeTenantNotActive {
		t.Fatalf("pending login: %d %s, want 403 tenant_not_active", r.status, r.body)
	}
	if got := f.familyCount(t, m.personID); got != before {
		t.Fatalf("families %d -> %d: a refused login must not open one", before, got)
	}
	wrong := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: m.email, Password: "Wrong-Pass-2026!zzzzz", DeviceLabel: "d"})
	unknown := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: "nobody-" + ids.NewV7().String()[24:] + "@e2e.test", Password: "Wrong-Pass-2026!zzzzz", DeviceLabel: "d"})
	if wrong.status != http.StatusUnauthorized || surfaceOf(wrong) != surfaceOf(unknown) {
		t.Fatalf("wrong password for a pending member %+v differs from unknown email %+v: the status leaks", surfaceOf(wrong), surfaceOf(unknown))
	}

	rr := f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: pair.RefreshToken})
	e := decodeError(t, rr.body)
	if rr.status != http.StatusUnauthorized || e.Error != ports.ErrCodeRefreshRejected || e.Reason != ports.RefreshReasonTenantNotActive {
		t.Fatalf("pending refresh: %d %s, want 401 refresh_rejected / tenant_not_active", rr.status, rr.body)
	}
	if got := f.revokeReasonOf(t, pair.RefreshToken); got != "tenant_not_active" {
		t.Fatalf("family revoke reason %q, want tenant_not_active", got)
	}

	// An operator acting into the tenant keeps working (that is how it is activated).
	if lr := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", su, nil, scopeHdr(owner.TenantID)); lr.status != http.StatusOK {
		t.Fatalf("operator read into the pending tenant: %d %s, want 200", lr.status, lr.body)
	}
	f.activateTenantHTTP(t, owner.TenantID)
	if lr := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: m.email, Password: m.password, DeviceLabel: "back"}); lr.status != http.StatusOK {
		t.Fatalf("login after activation: %d %s, want 200", lr.status, lr.body)
	}
	if rr := f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: pair.RefreshToken}); rr.status != http.StatusUnauthorized {
		t.Fatalf("the revoked pre-activation session came back: %d %s", rr.status, rr.body)
	}
}

// Without the outbox cascade the request paths must refuse on their own: login
// answers tenant_suspended after the password check; refresh answers
// refresh_rejected with reason tenant_suspended and revokes the family.
func TestE2E_SuspendedTenant_RequestPathsRefuseOnTheirOwn_AndReactivationRestores(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "suspco")
	m := f.createMember(t, owner, "suspmember")
	pair := f.loginPair(t, m.email, m.password) // a session that exists BEFORE the suspension

	f.suspendTenantHTTP(t, owner.TenantID)
	if got := f.tenantStatusOf(t, owner.TenantID); got != "suspended" {
		t.Fatalf("tenant status %q after suspend", got)
	}

	// Right password: 403 tenant_suspended, and no family is opened.
	before := f.familyCount(t, m.personID)
	r := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: m.email, Password: m.password, DeviceLabel: "d"})
	if r.status != http.StatusForbidden || decodeError(t, r.body).Error != ports.ErrCodeTenantSuspended {
		t.Fatalf("suspended login: %d %s, want 403 tenant_suspended", r.status, r.body)
	}
	if got := f.familyCount(t, m.personID); got != before {
		t.Fatalf("families %d -> %d: a refused login must not open one", before, got)
	}

	// Wrong password, and an unknown email: the ordinary 401, byte-identical to each
	// other (the distinct answer exists only after the password was verified).
	wrong := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: m.email, Password: "Wrong-Pass-2026!zzzzz", DeviceLabel: "d"})
	unknown := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: "nobody-" + ids.NewV7().String()[24:] + "@e2e.test", Password: "Wrong-Pass-2026!zzzzz", DeviceLabel: "d"})
	if wrong.status != http.StatusUnauthorized || decodeError(t, wrong.body).Error != ports.ErrCodeInvalidCredentials {
		t.Fatalf("wrong password for a suspended tenant's member: %d %s, want 401 invalid_credentials", wrong.status, wrong.body)
	}
	if surfaceOf(wrong) != surfaceOf(unknown) {
		t.Fatalf("wrong password %+v differs from unknown email %+v: the suspension leaks", surfaceOf(wrong), surfaceOf(unknown))
	}

	// Refresh with the pre-suspension session: 401 refresh_rejected / tenant_suspended.
	rr := f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: pair.RefreshToken})
	e := decodeError(t, rr.body)
	if rr.status != http.StatusUnauthorized || e.Error != ports.ErrCodeRefreshRejected || e.Reason != ports.RefreshReasonTenantSuspended {
		t.Fatalf("suspended refresh: %d %s, want 401 refresh_rejected / tenant_suspended", rr.status, rr.body)
	}
	if got := f.revokeReasonOf(t, pair.RefreshToken); got != "tenant_suspended" {
		t.Fatalf("family revoke reason %q, want tenant_suspended", got)
	}

	// An operator acting INTO the suspended tenant keeps working: reads honour the
	// header, and the lifecycle (path-tenant) routes reactivate it.
	su := f.mintPlatformTokenWith(t, nil, true)
	if lr := f.doWithHeaders(t, http.MethodGet, "/api/v1/users", su, nil, scopeHdr(owner.TenantID)); lr.status != http.StatusOK {
		t.Fatalf("operator read into the suspended tenant: %d %s, want 200", lr.status, lr.body)
	}
	f.activateTenantHTTP(t, owner.TenantID)

	// Reactivated: sign-in works again with the same password; the old family stays dead.
	if lr := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: m.email, Password: m.password, DeviceLabel: "back"}); lr.status != http.StatusOK {
		t.Fatalf("login after reactivation: %d %s, want 200", lr.status, lr.body)
	}
	if rr := f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: pair.RefreshToken}); rr.status != http.StatusUnauthorized {
		t.Fatalf("the revoked pre-suspension session came back: %d %s", rr.status, rr.body)
	}
}

// With the cascade running (as the worker does): suspending ends every live session
// of the tenant's members, and ONLY that tenant's.
func TestE2E_SuspendedTenant_CascadeRevokesLiveSessions(t *testing.T) {
	f := newE2EFixtureWithCascade(t, func() time.Time { return time.Now().UTC() })
	owner := f.registerAndLogin(t, "cascadeco")
	other := f.registerAndLogin(t, "cascadeot")
	m := f.createMember(t, owner, "cascadem")
	f.loginPair(t, m.email, m.password)
	if f.activeFamiliesOfTenant(t, owner.TenantID) < 2 {
		t.Fatalf("setup: want at least 2 live sessions, have %d", f.activeFamiliesOfTenant(t, owner.TenantID))
	}

	f.suspendTenantHTTP(t, owner.TenantID)

	deadline := time.Now().Add(15 * time.Second)
	for f.activeFamiliesOfTenant(t, owner.TenantID) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d live sessions of the suspended tenant after 15 s: the cascade did not run", f.activeFamiliesOfTenant(t, owner.TenantID))
		}
		time.Sleep(100 * time.Millisecond) // arch-test:wait-justified — poll interval inside a deadline-bounded loop awaiting the async tenant-suspension cascade (forwarder→router→subscriber)
	}
	var reasons int
	if err := f.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM identity.refresh_token_families WHERE tenant_id = $1 AND revoke_reason = 'tenant_suspended'`, owner.TenantID).Scan(&reasons); err != nil || reasons < 2 {
		t.Fatalf("%d families revoked as tenant_suspended (%v), want at least 2", reasons, err)
	}
	if f.activeFamiliesOfTenant(t, other.TenantID) == 0 {
		t.Fatal("another tenant's sessions were revoked")
	}
	if st, _ := f.mustRefresh(t, other.RefreshToken); st != http.StatusOK {
		t.Fatalf("another tenant's refresh after the suspension: %d, want 200", st)
	}
}

// Idle timeout over HTTP, on the injected clock (Refresh.SlidingTTL = 2h here): a
// session refreshed inside the idle lifetime lives on; one left idle ends with
// session_expired and the family is revoked as idle_timeout.
func TestE2E_Refresh_IdleTimeoutEndsAnUnusedSession(t *testing.T) {
	clock := newGraceClock()
	const idle = 2 * time.Hour
	f := newE2EFixtureIdle(t, clock.now, false, 0, idle)
	tn := f.registerAndLogin(t, "idleto")

	tok := tn.RefreshToken
	for i := 0; i < 5; i++ { // five refreshes 90 minutes apart: far beyond one idle lifetime in total
		clock.advance(90 * time.Minute)
		st, lr := f.mustRefresh(t, tok)
		if st != http.StatusOK {
			t.Fatalf("refresh %d inside the idle lifetime: %d", i+1, st)
		}
		tok = lr.RefreshToken
	}

	clock.advance(idle + time.Second) // idle for longer than the idle lifetime
	r := f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: tok})
	e := decodeError(t, r.body)
	if r.status != http.StatusUnauthorized || e.Error != ports.ErrCodeRefreshRejected || e.Reason != ports.RefreshReasonSessionExpired {
		t.Fatalf("idle past the lifetime: %d %s, want 401 refresh_rejected / session_expired", r.status, r.body)
	}
	if got := f.revokeReasonOf(t, tok); got != "idle_timeout" {
		t.Fatalf("revoke reason %q, want idle_timeout", got)
	}
}

// Review G2b M3: an impersonation session writes with tenant-admin authority, so it is
// refused into a suspended company (409 tenant_not_active), like an operator's
// X-Tenant-Id write; reactivating the company opens the way again.
func TestE2E_Impersonation_RefusesASuspendedCompany(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "impsuspended")
	f.suspendTenantHTTP(t, owner.TenantID)

	p := permission.IdentityPermissions
	operatorTok := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.ImpersonationManage, p.Platform.TenantsView}, false)
	open := func() httpResp {
		return f.authedJSON(t, http.MethodPost, "/api/v1/platform/impersonation/sessions", operatorTok,
			ports.CreateImpersonationSessionRequest{TargetTenantID: owner.TenantID, Reason: "ticket 4712 suspended company", DurationMinutes: 30})
	}
	if r := open(); r.status != http.StatusConflict || errCodeOf(r) != "tenant_not_active" {
		t.Fatalf("session into a suspended company: %d %s, want 409 tenant_not_active", r.status, r.body)
	}
	f.activateTenantHTTP(t, owner.TenantID)
	if r := open(); r.status != http.StatusCreated {
		t.Fatalf("session into the reactivated company: %d %s, want 201", r.status, r.body)
	}
}
