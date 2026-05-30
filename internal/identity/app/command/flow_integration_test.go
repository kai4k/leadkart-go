//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
//   Per-test context.WithTimeout would be belt-and-suspenders against the
//   shared-pool + parallel-with-RLS canon shape.
//
// arch-test:parallel-safe — every Test* uses the shared pgtest container
//   + a fresh tenant_id per test; RLS isolates rows by tenant so parallel
//   runs cannot see each others state. Brandur "Postgres at scale" +
//   TDL Wild Workouts canon.
//
// arch-test:raw-sql-justified — TestFlow_RegisterDuplicateActiveEmail_
//   Blocked intentionally bypasses the adapter with a direct SELECT
//   to assert the pg.UnitOfWork ROLLED BACK after the second register
//   failed (no tenant B row in identity.tenants). Per ADR 0062 §6 the
//   rollback is the SQL-specific contract — the observable error is
//   already mirror-able by the fake.

package command_test

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/leadkart/leadkart-go/internal/common/cache"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/app/permissions"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

const refreshTTL = 14 * 24 * time.Hour

// wiredApp groups the Identity composition outputs the integration
// tests need. Returned as a struct (not a positional tuple) because
// the post-A.7 surface — login + refresh + permission gate + stamp
// validator — pushed the tuple past readable arity.
type wiredApp struct {
	pool     *pgxpool.Pool
	register command.RegisterTenantHandler
	login    command.LoginHandler
	refresh  command.RefreshHandler
	logout   command.LogoutHandler
	issuer   *jwt.Issuer
	stamps   *adapters.SecurityStampValidator
}

func newWiredApp(t *testing.T) wiredApp {
	t.Helper()
	pool := startWiredPostgres(t)
	tx := pg.NewTransactor(pool)

	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)
	families := adapters.NewRefreshTokenFamilyRepository(pool, tx)
	roles := adapters.NewRoleRepository(pool, tx)

	now := func() time.Time { return time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC) }
	signingKey := jwt.SigningKey{
		KeyID:  "test-k1",
		Secret: []byte("0123456789abcdef0123456789abcdef"), // 32 bytes — HS256 floor
	}
	issuer, err := jwt.NewIssuer(signingKey, nil, now)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	dummyHash, err := argon2.Hash("dummy")
	if err != nil {
		t.Fatalf("dummy hash: %v", err)
	}

	// Real HybridCache + SecurityStampCache + Validator wired against
	// miniredis. Required by the post-A.7 [authn.RequirePermission]
	// surface (which composes RequireFreshStamp internally).
	store := miniredis.RunT(t)
	redisCli := redis.NewClient(&redis.Options{Addr: store.Addr()})
	t.Cleanup(func() { _ = redisCli.Close() })
	hc, err := cache.New(cache.Config{
		L1MaxItems: 1000,
		L2:         redisCli,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(hc.Close)
	stampCache := adapters.NewSecurityStampCache(hc, persons)
	stamps := adapters.NewSecurityStampValidator(stampCache)

	permResolver := permissions.NewResolver(memberships, roles, now)
	register := command.NewRegisterTenantHandler(
		tx, tenants, persons, memberships, roles, now,
		func() tenant.ID { return tenant.ID(ids.NewV7().String()) },
		func() person.ID { return person.ID(ids.NewV7().String()) },
		func() membership.ID { return membership.ID(ids.NewV7().String()) },
	)
	authRouter := adapters.NewAuthRouterPG(pool, tx)
	login := command.NewLoginHandler(
		authRouter, families, tenants, persons, permResolver, issuer, now, refreshTTL, dummyHash,
		func() refreshtoken.FamilyID { return refreshtoken.FamilyID(ids.NewV7().String()) },
	)
	refresh := command.NewRefreshHandler(families, persons, memberships, tenants, permResolver, issuer, now, refreshTTL)
	logout := command.NewLogoutHandler(families, now)

	return wiredApp{
		pool:     pool,
		register: register,
		login:    login,
		refresh:  refresh,
		logout:   logout,
		issuer:   issuer,
		stamps:   stamps,
	}
}

func TestFlow_RegisterLoginRefreshLogout(t *testing.T) {
	t.Parallel()
	app := newWiredApp(t)
	register, login, refresh, logout := app.register, app.login, app.refresh, app.logout
	ctx := t.Context()

	// 1. Register a new tenant + admin person + membership.
	full := ids.NewV7().String()
	registerSlug, err := slug.New("acme-flow-" + full[len(full)-8:])
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	adminEmail, _ := email.New("alice@flow.test")
	regOut, err := register.Handle(ctx, command.RegisterTenantCommand{
		Slug:           registerSlug,
		LegalName:      "Acme Flow Pharma Pvt Ltd",
		DisplayName:    "Acme Flow",
		AdminEmail:     adminEmail,
		AdminPassword:  "correct horse battery staple",
		AdminFirstName: "Alice",
		AdminLastName:  "Admin",
	})
	if err != nil {
		t.Fatalf("RegisterTenant: %v", err)
	}
	if regOut.TenantID.IsZero() || regOut.PersonID.IsZero() || regOut.MembershipID.IsZero() {
		t.Fatal("RegisterTenant returned zero IDs")
	}

	// 2. Login with the registered admin.
	loginOut, err := login.Handle(ctx, command.LoginCommand{
		Email:       adminEmail,
		Password:    "correct horse battery staple",
		DeviceLabel: "iPhone 15 / Safari",
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if loginOut.AccessToken == "" || loginOut.RefreshTokenPlain == "" {
		t.Fatal("Login returned empty tokens")
	}
	if loginOut.PersonID != regOut.PersonID {
		t.Fatalf("Login PersonID: got %q want %q", loginOut.PersonID, regOut.PersonID)
	}
	if loginOut.TenantID != regOut.TenantID {
		t.Fatalf("Login TenantID: got %q want %q", loginOut.TenantID, regOut.TenantID)
	}

	// 3. Refresh — old plaintext gets a fresh ⟨access, refresh⟩ pair.
	refreshOut, err := refresh.Handle(ctx, command.RefreshCommand{
		RefreshTokenPlain: loginOut.RefreshTokenPlain,
	})
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if refreshOut.AccessToken == loginOut.AccessToken {
		t.Fatal("Refresh returned the same access token (no rotation)")
	}
	if refreshOut.RefreshTokenPlain == loginOut.RefreshTokenPlain {
		t.Fatal("Refresh returned the same refresh plaintext (no rotation)")
	}

	// 4. Replay the consumed plaintext — RFC 9700 §4.13 reuse detection
	//    revokes the family + rejects the request.
	_, err = refresh.Handle(ctx, command.RefreshCommand{
		RefreshTokenPlain: loginOut.RefreshTokenPlain,
	})
	if !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("expected ErrRefreshRejected on consumed token replay, got %v", err)
	}

	// 5. The new refresh token is now ALSO rejected (family revoked).
	_, err = refresh.Handle(ctx, command.RefreshCommand{
		RefreshTokenPlain: refreshOut.RefreshTokenPlain,
	})
	if !errors.Is(err, command.ErrRefreshRejected) {
		t.Fatalf("expected ErrRefreshRejected on revoked-family token, got %v", err)
	}

	// 6. Logout is idempotent — accepts the now-revoked plaintext.
	if err := logout.Handle(ctx, command.LogoutCommand{
		RefreshTokenPlain: refreshOut.RefreshTokenPlain,
		Reason:            "user-logout",
	}); err != nil {
		t.Fatalf("Logout: %v", err)
	}
}

// Note: TestFlow_LoginUnknownEmail_GenericFailure +
// TestFlow_LoginWrongPassword_GenericFailure were pruned per ADR 0062
// strict redundancy audit (2026-05-26). Both branches are pure handler
// orchestration over the AuthRouter contract + argon2 verify; no SQL
// contract is exercised. Equivalent assertions now live in
// login_test.go as handler-unit tests against persontest.FakeRepository
// + fakeAuthRouter — running in <100ms each vs. the full pgtest +
// miniredis + JWT boot the integration version paid for the same
// observable.

// TestFlow_RegisterDuplicateActiveEmail_Blocked covers the
// cross-aggregate UnitOfWork rollback when the membership Add fails
// with ErrAlreadyActive. Sharpened per ADR 0062: the observable
// (`ErrEmailHasActiveMembership`) is mirror-able from the fake-backed
// handler layer; the SQL-specific contract is that the FAILED Add
// rolls back the ENTIRE pg.UnitOfWork — tenant B's row, the
// (reused) person update, the failed membership — atomically.
//
// Two halves of the SQL contract:
//
//  1. Observable: second Register returns ErrEmailHasActiveMembership.
//  2. Physical: direct SELECT for tenant B's slug returns zero rows
//     — proves the pg.UnitOfWork rolled back the tenant insert when
//     the membership Add fired ErrAlreadyActive partway through.
//
// Only the SQL adapter can prove (2); the fake has no UoW or pg.Tx
// semantics to roll back.
func TestFlow_RegisterDuplicateActiveEmail_Blocked(t *testing.T) {
	t.Parallel()
	wired := newWiredApp(t)
	register := wired.register
	ctx := t.Context()

	addr, _ := email.New("dup-active@flow.test")
	full := ids.NewV7().String()
	slugA, _ := slug.New("a-" + full[len(full)-8:])

	if _, err := register.Handle(ctx, command.RegisterTenantCommand{
		Slug:           slugA,
		LegalName:      "Tenant A Pharma",
		DisplayName:    "A",
		AdminEmail:     addr,
		AdminPassword:  "pw",
		AdminFirstName: "Alice",
		AdminLastName:  "A",
	}); err != nil {
		t.Fatalf("Register A: %v", err)
	}

	full2 := ids.NewV7().String()
	slugB, _ := slug.New("b-" + full2[len(full2)-8:])
	_, err := register.Handle(ctx, command.RegisterTenantCommand{
		Slug:           slugB,
		LegalName:      "Tenant B Pharma",
		DisplayName:    "B",
		AdminEmail:     addr, // same email — already an Active Membership
		AdminPassword:  "pw2",
		AdminFirstName: "Alice",
		AdminLastName:  "B",
	})
	// SQL-contract part 1: observable error.
	if !errors.Is(err, command.ErrEmailHasActiveMembership) {
		t.Fatalf("expected ErrEmailHasActiveMembership, got %v", err)
	}

	// SQL-contract part 2: pg.UnitOfWork rollback proof. Tenant B's
	// row must NOT exist — the failed membership Add rolled back the
	// entire register flow's tx. Direct SELECT bypassing the adapter.
	var count int
	if err := wired.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM identity.tenants WHERE slug = $1`,
		slugB.String(),
	).Scan(&count); err != nil {
		t.Fatalf("direct SELECT for tenant B rollback proof: %v", err)
	}
	if count != 0 {
		t.Fatalf("tenant B row count after failed register: got %d want 0 — pg.UnitOfWork did NOT roll back the tenant insert", count)
	}
}

// TestE2E_LoginThenRequirePermissionGate is the Task 26 closing test:
// the full chain — onboard a tenant (CompanyOwner role auto-seeded with
// Meta.TenantAdmin permission) → login → take the JWT → call a handler
// guarded by [authn.RequirePermission] → assert the gate passes for
// the seeded permission and rejects an unrelated one.
//
// Proves the load-bearing claim of Phase 1: TenantOnboardingService +
// PermissionResolver + JWT issuer + authn middleware compose into a
// working end-to-end authorization flow with no test-only shortcuts.
func TestE2E_LoginThenRequirePermissionGate(t *testing.T) {
	t.Parallel()
	app := newWiredApp(t)
	register, login, issuer, stamps := app.register, app.login, app.issuer, app.stamps
	ctx := t.Context()

	// 1. Onboard. CompanyOwner auto-assigned, carries Meta.TenantAdmin.
	full := ids.NewV7().String()
	registerSlug, _ := slug.New("e2e-gate-" + full[len(full)-8:])
	adminEmail, _ := email.New("e2e-gate@flow.test")
	if _, err := register.Handle(ctx, command.RegisterTenantCommand{
		Slug:           registerSlug,
		LegalName:      "E2E Gate Pharma Pvt Ltd",
		DisplayName:    "E2E",
		AdminEmail:     adminEmail,
		AdminPassword:  "correct horse battery staple",
		AdminFirstName: "Eve",
		AdminLastName:  "Admin",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// 2. Login → real JWT signed by the wired Issuer.
	loginOut, err := login.Handle(ctx, command.LoginCommand{
		Email:       adminEmail,
		Password:    "correct horse battery staple",
		DeviceLabel: "E2E Test Browser",
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if loginOut.AccessToken == "" {
		t.Fatal("Login returned empty access token")
	}

	// 3. Build a sentinel handler — proves middleware passed through.
	called := false
	sentinel := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// 4. Guarded by Meta.TenantAdmin — the permission CompanyOwner carries.
	gateGranted := authn.RequirePermission(issuer, stamps,
		permission.IdentityPermissions.Meta.TenantAdmin)(sentinel)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+loginOut.AccessToken)
	rec := httptest.NewRecorder()
	gateGranted.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("granted gate: got %d want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if !called {
		t.Fatal("granted gate: sentinel did not run")
	}

	// 5. Guarded by a permission CompanyOwner does NOT carry (Tenants.Delete
	//    is a platform-tier permission). 403, sentinel never runs.
	called = false
	gateForbidden := authn.RequirePermission(issuer, stamps,
		permission.IdentityPermissions.Tenants.Delete)(sentinel)
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+loginOut.AccessToken)
	rec = httptest.NewRecorder()
	gateForbidden.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("forbidden gate: got %d want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatal("forbidden gate: sentinel ran despite missing permission")
	}

	// 6. Tampered token → 401 (not 403). Mutating one byte of the
	//    signature segment invalidates the HMAC; the verifier rejects
	//    before claim inspection.
	//
	//    The previous form (`[:len-2] + "XX"`) is non-deterministic — if
	//    the random signature happens to end with "XX" (≈1 in 4096) the
	//    tamper is a no-op and the verifier accepts the token. Split on
	//    "." so we know we're touching the signature segment, then flip
	//    one char to a guaranteed-different base64url value. This makes
	//    the test robust without weakening the HMAC-rejection intent.
	parts := strings.Split(loginOut.AccessToken, ".")
	if len(parts) != 3 || len(parts[2]) == 0 {
		t.Fatalf("token shape: want header.payload.signature, got %d parts", len(parts))
	}
	sig := []byte(parts[2])
	if sig[0] == 'A' {
		sig[0] = 'B'
	} else {
		sig[0] = 'A'
	}
	tampered := parts[0] + "." + parts[1] + "." + string(sig)
	called = false
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+tampered)
	rec = httptest.NewRecorder()
	gateGranted.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered token: got %d want 401", rec.Code)
	}
	if called {
		t.Fatal("tampered token: sentinel ran")
	}
}

// Shared bootstrap (startWiredPostgres / TestMain / migrations / role
// provisioning) lives in fixture_integration_test.go per the Brandur /
// TDL canon — ONE container per package, shared pool, per-test
// isolation via fresh tenant_id + RLS.
