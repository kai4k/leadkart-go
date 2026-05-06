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
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
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
	"github.com/leadkart/leadkart-go/internal/identity/app/service"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

const refreshTTL = 14 * 24 * time.Hour

func newWiredApp(t *testing.T) (*pgxpool.Pool, command.RegisterTenantHandler, command.LoginHandler, command.RefreshHandler, command.LogoutHandler) {
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

	onboarding := service.NewTenantOnboardingService(tx, tenants, persons, memberships, roles)
	register := command.NewRegisterTenantHandler(onboarding)
	login := command.NewLoginHandler(persons, memberships, families, tenants, issuer, now, refreshTTL, dummyHash)
	refresh := command.NewRefreshHandler(families, persons, memberships, tenants, issuer, now, refreshTTL)
	logout := command.NewLogoutHandler(families)

	return pool, register, login, refresh, logout
}

func TestFlow_RegisterLoginRefreshLogout(t *testing.T) {
	_, register, login, refresh, logout := newWiredApp(t)
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
	_, _, login, _, _ := newWiredApp(t)
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
	_, register, login, _, _ := newWiredApp(t)
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
	_, register, _, _, _ := newWiredApp(t)
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
	if err := goose.SetDialect("postgres"); err != nil {
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
