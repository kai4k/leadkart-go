//go:build integration

// HTTP-level contract test for the Identity Week-5 surface.
//
// Composes the same auth flow as command/flow_integration_test.go but
// through net/http via httptest.Server — proves the JSON wire shape
// agrees with the .NET LeadKart Web API for blob-compatible client
// reuse.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
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

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
	"github.com/leadkart/leadkart-go/internal/common/cache"
	"github.com/leadkart/leadkart-go/internal/common/config"
	platformapp "github.com/leadkart/leadkart-go/internal/platform/app"
)

// newTestHybridCache spins an in-process miniredis + wires the
// LeadKart [cache.HybridCache] against it. Lets cmd/api integration
// tests exercise the same composition root [buildIdentityApp] uses in
// production (HybridCache + SecurityStampCache + Validator) without
// needing a real Redis container.
func newTestHybridCache(t *testing.T) *cache.HybridCache {
	t.Helper()
	store := miniredis.RunT(t)
	cli := redis.NewClient(&redis.Options{Addr: store.Addr()})
	t.Cleanup(func() { _ = cli.Close() })
	hc, err := cache.New(cache.Config{
		L1MaxItems: 1000,
		L2:         cli,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("cache.New: %v", err)
	}
	t.Cleanup(hc.Close)
	return hc
}

func TestHTTPFlow_RegisterLoginRefreshLogout(t *testing.T) {
	t.Parallel()
	pool := startWiredPostgresForHTTP(t)

	cfg := config.AppConfig{
		JWT: config.JWTConfig{
			KeyID:      "test-k1",
			SigningKey: "0123456789abcdef0123456789abcdef",
		},
		Refresh: config.RefreshConfig{
			AbsoluteTTL: 14 * 24 * time.Hour,
		},
	}
	now := func() time.Time { return time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC) }

	hybrid := newTestHybridCache(t)
	wiring, err := buildIdentityApp(pool, hybrid, cfg, now)
	if err != nil {
		t.Fatalf("buildIdentityApp: %v", err)
	}

	srv := httptest.NewServer(newServer(silentLogger(), wiring.App, platformapp.Application{}, buildInventoryApp(pool), wiring.Issuer, wiring.StampValidator))
	t.Cleanup(srv.Close)

	full := ids.NewV7().String()
	registerSlug := "http-acme-" + full[len(full)-8:]

	// 1. Register tenant.
	reg := postJSON(t, srv.URL+"/api/v1/tenants", ports.RegisterTenantRequest{
		Slug:           registerSlug,
		LegalName:      "HTTP Acme Pharma Pvt Ltd",
		DisplayName:    "HTTP Acme",
		AdminEmail:     "http-admin@flow.test",
		AdminPassword:  "correct horse battery staple",
		AdminFirstName: "HTTP",
		AdminLastName:  "Admin",
	})
	if reg.status != http.StatusCreated {
		t.Fatalf("Register: status %d body %s", reg.status, reg.body)
	}
	var regResp ports.RegisterTenantResponse
	if err := json.Unmarshal(reg.body, &regResp); err != nil {
		t.Fatalf("Register decode: %v", err)
	}
	if regResp.TenantID == "" || regResp.PersonID == "" || regResp.MembershipID == "" {
		t.Fatalf("Register empty IDs: %+v", regResp)
	}

	// 2. Login.
	login := postJSON(t, srv.URL+"/api/v1/auth/login", ports.LoginRequest{
		Email:       "http-admin@flow.test",
		Password:    "correct horse battery staple",
		DeviceLabel: "iPhone 15 / Safari",
	})
	if login.status != http.StatusOK {
		t.Fatalf("Login: status %d body %s", login.status, login.body)
	}
	var loginResp ports.LoginResponse
	if err := json.Unmarshal(login.body, &loginResp); err != nil {
		t.Fatalf("Login decode: %v", err)
	}
	if loginResp.AccessToken == "" || loginResp.RefreshToken == "" {
		t.Fatalf("Login empty tokens: %+v", loginResp)
	}
	if loginResp.TokenType != "Bearer" {
		t.Fatalf("Login token_type: got %q want Bearer", loginResp.TokenType)
	}

	// 3. Refresh.
	refresh := postJSON(t, srv.URL+"/api/v1/auth/refresh", ports.RefreshRequest{
		RefreshToken: loginResp.RefreshToken,
	})
	if refresh.status != http.StatusOK {
		t.Fatalf("Refresh: status %d body %s", refresh.status, refresh.body)
	}
	var refreshResp ports.LoginResponse
	if err := json.Unmarshal(refresh.body, &refreshResp); err != nil {
		t.Fatalf("Refresh decode: %v", err)
	}
	if refreshResp.AccessToken == loginResp.AccessToken {
		t.Fatal("Refresh returned identical access token")
	}
	if refreshResp.RefreshToken == loginResp.RefreshToken {
		t.Fatal("Refresh returned identical refresh token")
	}

	// 4. Replay original (consumed) token → 401 refresh_rejected,
	//    revokes family per RFC 9700 §4.13.
	replay := postJSON(t, srv.URL+"/api/v1/auth/refresh", ports.RefreshRequest{
		RefreshToken: loginResp.RefreshToken,
	})
	if replay.status != http.StatusUnauthorized {
		t.Fatalf("Replay status: got %d want 401", replay.status)
	}
	var replayErr ports.ErrorResponse
	if err := json.Unmarshal(replay.body, &replayErr); err != nil {
		t.Fatalf("Replay decode: %v", err)
	}
	if replayErr.Error != "refresh_rejected" {
		t.Fatalf("Replay error: got %q want refresh_rejected", replayErr.Error)
	}

	// 5. Logout — idempotent on now-revoked family.
	logout := postJSON(t, srv.URL+"/api/v1/auth/logout", ports.LogoutRequest{
		RefreshToken: refreshResp.RefreshToken,
		Reason:       "user-logout",
	})
	if logout.status != http.StatusNoContent {
		t.Fatalf("Logout status: got %d want 204 body %s", logout.status, logout.body)
	}
}

func TestHTTPFlow_LoginInvalidCredentials_Returns401(t *testing.T) {
	t.Parallel()
	pool := startWiredPostgresForHTTP(t)
	cfg := config.AppConfig{
		JWT: config.JWTConfig{
			KeyID:      "test-k1",
			SigningKey: "0123456789abcdef0123456789abcdef",
		},
		Refresh: config.RefreshConfig{
			AbsoluteTTL: 14 * 24 * time.Hour,
		},
	}
	now := func() time.Time { return time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC) }
	hybrid := newTestHybridCache(t)
	wiring, err := buildIdentityApp(pool, hybrid, cfg, now)
	if err != nil {
		t.Fatalf("buildIdentityApp: %v", err)
	}
	srv := httptest.NewServer(newServer(silentLogger(), wiring.App, platformapp.Application{}, buildInventoryApp(pool), wiring.Issuer, wiring.StampValidator))
	t.Cleanup(srv.Close)

	resp := postJSON(t, srv.URL+"/api/v1/auth/login", ports.LoginRequest{
		Email:    "nobody@nowhere.test",
		Password: "anything",
	})
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401 body %s", resp.status, resp.body)
	}
	var errResp ports.ErrorResponse
	if err := json.Unmarshal(resp.body, &errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errResp.Error != "invalid_credentials" {
		t.Fatalf("error code: got %q want invalid_credentials", errResp.Error)
	}
}

// ----- HTTP test helpers -----------------------------------------------------

type httpResp struct {
	status int
	body   []byte
}

func postJSON(t *testing.T, url string, body interface{}) httpResp {
	t.Helper()
	buf := &bytes.Buffer{}
	if err := json.NewEncoder(buf).Encode(body); err != nil {
		t.Fatalf("encode body: %v", err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return httpResp{status: resp.StatusCode, body: respBody}
}

// startWiredPostgresForHTTP duplicates the adapters_test fixture for
// the cmd/api package — testcontainers Postgres + non-superuser app
// role — without exporting the helper across package boundaries.
func startWiredPostgresForHTTP(t *testing.T) *pgxpool.Pool {
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
	if err := goose.UpContext(ctx, gooseDB, migrationsDir(t)); err != nil {
		_ = gooseDB.Close()
		t.Fatalf("goose up: %v", err)
	}
	for _, s := range []string{
		`CREATE ROLE leadkart_app LOGIN PASSWORD 'leadkart_app_pw' NOSUPERUSER NOINHERIT NOCREATEROLE NOCREATEDB`,
		`GRANT USAGE ON SCHEMA app, identity, platform TO leadkart_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA identity TO leadkart_app`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA platform TO leadkart_app`,
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

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// here = .../cmd/api/http_flow_integration_test.go
	return filepath.Join(filepath.Dir(here), "..", "..", "migrations")
}

