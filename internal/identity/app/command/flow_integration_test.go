//go:build integration

// End-to-end contract test for the Identity Week-5 command surface.
//
// The four handlers compose the auth flow that the .NET LeadKart side
// has shipped for years; this test pins the same shape against real
// Postgres + the four pgx repository adapters. No HTTP layer yet —
// once routes.go ships, the same flow runs through net/http with the
// integration assertions adapting to JSON DTOs.
//
// Contract:
//
//	RegisterTenant(slug, admin) → tenantID + personID + membershipID
//	Login(email, password)      → access JWT + refresh plaintext
//	Refresh(plaintext)          → fresh JWT + new refresh plaintext
//	                               (old plaintext now consumed)
//	Refresh(consumed plaintext) → ErrRefreshRejected + family revoked
//	                               (RFC 9700 §4.13 reuse detection)
//	Logout(plaintext)           → idempotent revoke

package command_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
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
	"github.com/leadkart/leadkart-go/internal/common/cache"
	"github.com/leadkart/leadkart-go/internal/common/pg"
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

func TestFlow_LoginUnknownEmail_GenericFailure(t *testing.T) {
	login := newWiredApp(t).login
	ctx := t.Context()

	addr, _ := email.New("nobody@example.test")
	_, err := login.Handle(ctx, command.LoginCommand{
		Email:    addr,
		Password: "anything",
	})
	if !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestFlow_LoginWrongPassword_GenericFailure(t *testing.T) {
	app := newWiredApp(t)
	register, login := app.register, app.login
	ctx := t.Context()

	full := ids.NewV7().String()
	tenantSlug, _ := slug.New("wp-" + full[len(full)-8:])
	addr, _ := email.New("wp@flow.test")
	if _, err := register.Handle(ctx, command.RegisterTenantCommand{
		Slug:           tenantSlug,
		LegalName:      "Wrong Password Pharma",
		DisplayName:    "WP",
		AdminEmail:     addr,
		AdminPassword:  "right-password",
		AdminFirstName: "WP",
		AdminLastName:  "Admin",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := login.Handle(ctx, command.LoginCommand{
		Email:    addr,
		Password: "wrong-password",
	})
	if !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestFlow_RegisterDuplicateActiveEmail_Blocked(t *testing.T) {
	register := newWiredApp(t).register
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
	if !errors.Is(err, command.ErrEmailHasActiveMembership) {
		t.Fatalf("expected ErrEmailHasActiveMembership, got %v", err)
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

// startWiredPostgres mirrors repoFixture in the adapters package: spins
// an ephemeral Postgres, applies migrations, provisions the non-superuser
// `leadkart_app` role, returns a pgxpool connected as that role.
//
// Duplicated rather than imported because the adapters_test package is
// `adapters_test` and exporting its helper would require widening the
// Test* visibility surface unnecessarily.
func startWiredPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	c, err := postgres.Run(ctx,
		"postgres:17-alpine",
		postgres.WithDatabase("leadkart_test"),
		postgres.WithUsername("leadkart"),
		postgres.WithPassword("leadkart_test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(ctx)
	})

	ownerDSN, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}

	gooseDB, err := goose.OpenDBWithDriver("pgx", ownerDSN)
	if err != nil {
		t.Fatalf("goose open: %v", err)
	}
	if err := pg.EnsureGooseDialect(); err != nil {
		_ = gooseDB.Close()
		t.Fatalf("set dialect: %v", err)
	}
	migrationsDir := resolveMigrationsDir(t)
	if err := goose.UpContext(ctx, gooseDB, migrationsDir); err != nil {
		_ = gooseDB.Close()
		t.Fatalf("goose up: %v", err)
	}
	for _, s := range []string{
		`CREATE ROLE leadkart_app LOGIN PASSWORD 'leadkart_app_pw' NOSUPERUSER NOINHERIT NOCREATEROLE NOCREATEDB`,
		`GRANT USAGE ON SCHEMA app, identity TO leadkart_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA identity TO leadkart_app`,
		`GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA app TO leadkart_app`,
	} {
		if _, err := gooseDB.ExecContext(ctx, s); err != nil {
			_ = gooseDB.Close()
			t.Fatalf("provision leadkart_app: %s: %v", s, err)
		}
	}
	_ = gooseDB.Close()

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	appDSN := "postgres://leadkart_app:leadkart_app_pw@" + host + ":" + port.Port() + "/leadkart_test?sslmode=disable"

	pool, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func resolveMigrationsDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// here = .../internal/identity/app/command/flow_integration_test.go
	return filepath.Join(filepath.Dir(here), "..", "..", "..", "..", "migrations")
}
