//go:build integration

// End-to-end test that closes the full PUB/SUB + cache-invalidation
// loop for the SecurityStampCache:
//
//  1. Register tenant + login → access token T1 with security_stamp S1.
//  2. GET /api/v1/auth/sessions with T1 → 200. This warms the
//     SecurityStampCache with S1.
//  3. POST /api/v1/auth/change-password with T1 → 204. The handler
//     rotates the Person's stamp to S2 + writes
//     PersonPasswordChangedV1 to the outbox.
//  4. The outbox forwarder publishes to the gochannel pubsub. The
//     subscriber router routes to RevokeFamiliesOnSecurityChange,
//     which (a) invalidates the SecurityStampCache for the Person and
//     (b) revokes every active refresh-token family.
//  5. GET /api/v1/auth/sessions with the SAME T1 → 401 stale_token.
//     The middleware reads the cache (now empty), falls through to
//     Postgres, gets S2, sees S1 ≠ S2, and fails closed.
//
// The "fast path" assertion: step 5 must hit 401 well within the
// SecurityStampCache's 30s TTL. The TTL is the safety-net fallback;
// the subscriber-driven invalidation is the canonical close. Per the
// audit-checklist.md §12b "proof-of-cache test" canon, a test that
// only proves the TTL fallback also passes when the invalidation is
// silently broken — so we tighten the assertion.

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
	"github.com/leadkart/leadkart-go/internal/identity/ports/subscribers"
	"github.com/leadkart/leadkart-go/internal/platform/audit"
	"github.com/leadkart/leadkart-go/internal/platform/config"
	"github.com/leadkart/leadkart-go/internal/platform/messaging"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// cascadeBudget is the wall-clock budget for the outbox-forwarder
// + subscriber-router pipeline to complete: pickup + publish +
// idempotency check + audit row + Invalidate + family revoke. Under
// -race + busy CI Docker daemons this can climb past a naive 5s
// estimate; 15s is comfortable on the slowest CI runners observed
// while still being well under the 30s SecurityStampCache TTL — i.e.
// the assertion still distinguishes "invalidation cascade ran" from
// "TTL fallback masked the failure."
const cascadeBudget = 15 * time.Second

func TestSecurityStampInvalidation_PasswordChange_Returns401WithinFastPath(t *testing.T) {
	pool := startWiredPostgresForHTTP(t)
	hybrid := newTestHybridCache(t)

	cfg := config.AppConfig{
		JWT: config.JWTConfig{
			KeyID:      "test-k1",
			SigningKey: "0123456789abcdef0123456789abcdef",
		},
		Refresh: config.RefreshConfig{AbsoluteTTL: 14 * 24 * time.Hour},
	}
	now := func() time.Time { return time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC) }

	wiring, err := buildIdentityApp(pool, hybrid, cfg, now)
	if err != nil {
		t.Fatalf("buildIdentityApp: %v", err)
	}

	// Wire the same outbox-forwarder + router + subscribers stack
	// cmd/api/main.go runs in production. Without this, the
	// PersonPasswordChangedV1 event the ChangePassword handler emits
	// to the outbox would never reach the cascade subscriber.
	tx := pg.NewTransactor(pool)
	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(silentLogger()))
	t.Cleanup(func() { _ = pubsub.Close() })

	forwarder := adapters.NewOutboxForwarder(pool, tx, pubsub, integrationevents.Topic, 0)
	router, err := messaging.NewRouter(messaging.Deps{
		Subscriber:       pubsub,
		Logger:           silentLogger(),
		IdempotencyInbox: messaging.NewIdempotentReceiver(pool),
		AuditWriter:      audit.NewWriter(pool, silentLogger()),
		CloseTimeout:     2 * time.Second,
		Retry: messaging.RetryConfig{
			MaxRetries:      1,
			InitialInterval: 10 * time.Millisecond,
			MaxInterval:     50 * time.Millisecond,
			Multiplier:      2.0,
		},
	})
	if err != nil {
		t.Fatalf("messaging.NewRouter: %v", err)
	}
	subscribers.Register(router, wiring.Families, wiring.StampCache, silentLogger())

	stackCtx, stackCancel := context.WithCancel(t.Context())
	t.Cleanup(stackCancel)

	go forwarder.Run(stackCtx, 50*time.Millisecond, 10*time.Millisecond, func(err error) {
		t.Logf("forwarder: %v", err)
	})

	routerDone := make(chan struct{})
	go func() {
		defer close(routerDone)
		if err := router.Run(stackCtx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("router.Run: %v", err)
		}
	}()
	t.Cleanup(func() {
		stackCancel()
		select {
		case <-routerDone:
		case <-time.After(3 * time.Second):
			t.Error("router did not stop within 3s")
		}
	})
	select {
	case <-router.Running():
	case <-time.After(2 * time.Second):
		t.Fatal("router did not start within 2s")
	}

	srv := httptest.NewServer(newServer(silentLogger(), wiring.App, wiring.Issuer, wiring.StampValidator))
	t.Cleanup(srv.Close)

	// 1. Register tenant.
	full := ids.NewV7().String()
	regSlug := "stamp-flow-" + full[len(full)-8:]
	regEmail := "stamp-admin-" + full[len(full)-8:] + "@flow.test"
	const password = "correct horse battery staple"
	if r := postJSON(t, srv.URL+"/api/v1/tenants", ports.RegisterTenantRequest{
		Slug:           regSlug,
		LegalName:      "Stamp Flow Pharma Pvt Ltd",
		DisplayName:    "Stamp Flow",
		AdminEmail:     regEmail,
		AdminPassword:  password,
		AdminFirstName: "Stamp",
		AdminLastName:  "Admin",
	}); r.status != http.StatusCreated {
		t.Fatalf("register: status %d body %s", r.status, r.body)
	}

	// 2. Login → access token T1, security_stamp S1.
	loginResp := postJSON(t, srv.URL+"/api/v1/auth/login", ports.LoginRequest{
		Email:       regEmail,
		Password:    password,
		DeviceLabel: "Stamp Test",
	})
	if loginResp.status != http.StatusOK {
		t.Fatalf("login: status %d body %s", loginResp.status, loginResp.body)
	}
	var login ports.LoginResponse
	if err := json.Unmarshal(loginResp.body, &login); err != nil {
		t.Fatalf("login decode: %v", err)
	}
	if login.AccessToken == "" {
		t.Fatal("login returned empty access token")
	}

	// 3. Warm the SecurityStampCache by hitting an authenticated route.
	if got := getWithBearer(t, srv.URL+"/api/v1/auth/sessions", login.AccessToken); got.status != http.StatusOK {
		t.Fatalf("warm sessions: status %d body %s (cache should warm to S1)", got.status, got.body)
	}

	// 4. ChangePassword rotates the Person's stamp + emits
	//    PersonPasswordChangedV1 to the outbox. Per the EDA cascade,
	//    forwarder → router → subscriber.Invalidate(cache) → revoke
	//    families. Both halves complete well under the 30s cache TTL.
	cpResp := postJSONWithBearer(t, srv.URL+"/api/v1/auth/change-password", login.AccessToken,
		ports.ChangePasswordRequest{
			CurrentPassword: password,
			NewPassword:     password + " v2",
		})
	if cpResp.status != http.StatusNoContent {
		t.Fatalf("change-password: status %d body %s", cpResp.status, cpResp.body)
	}

	// 5. Wait for the cascade to complete via the deterministic DB-level
	//    signal (Person's stamp rotated in the persons table). The
	//    subscriber's Invalidate happens BEFORE family revocation in
	//    revokeAll, so once we observe the DB stamp != warm stamp the
	//    cache invalidation has already run. Polling /sessions for an
	//    INFERRED state ({"sessions":[]} = families revoked but cache
	//    unobservable) was racy on busy CI runners — the polled request
	//    could race with an in-flight cascade and observe families
	//    revoked while the freshness check still hit a yet-to-be-
	//    invalidated cache entry.
	personID := decodeJWTSubject(t, login.AccessToken)
	waitForCascadeComplete(t, pool, personID, cascadeBudget)

	// 6. Now make ONE /sessions request with the original T1. After the
	//    cascade is fully drained, the cache MUST be invalidated and
	//    the next Get repopulates from Postgres which now holds S2.
	//    IsFresh(S1, S2) → false → 401 stale_token. This is the actual
	//    contract assertion.
	got := getWithBearer(t, srv.URL+"/api/v1/auth/sessions", login.AccessToken)
	if got.status != http.StatusUnauthorized {
		t.Fatalf("post-cascade /sessions: status %d body %s — expected 401 stale_token "+
			"(invalidation cascade broken: cache returned a stale entry that should have been evicted)",
			got.status, got.body)
	}
	var er ports.ErrorResponse
	if err := json.Unmarshal(got.body, &er); err != nil {
		t.Fatalf("decode error response: %v body %s", err, got.body)
	}
	if er.Error != "stale_token" {
		t.Fatalf("error code: got %q want stale_token (body %s)", er.Error, got.body)
	}
}

// decodeJWTSubject pulls the `sub` claim out of a JWT without
// signature verification. Tests need the PersonID for direct DB
// queries; round-tripping through JSON-decoding the JWT body is
// simpler than threading the registered PersonID through the test.
func decodeJWTSubject(t *testing.T, token string) string {
	t.Helper()
	parts := bytes.Split([]byte(token), []byte("."))
	if len(parts) != 3 {
		t.Fatalf("decodeJWTSubject: token has %d parts, want 3", len(parts))
	}
	body, err := base64URLDecode(parts[1])
	if err != nil {
		t.Fatalf("decodeJWTSubject: base64: %v", err)
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(body, &claims); err != nil {
		t.Fatalf("decodeJWTSubject: json: %v", err)
	}
	if claims.Sub == "" {
		t.Fatalf("decodeJWTSubject: empty sub claim")
	}
	return claims.Sub
}

// base64URLDecode handles JWT's RFC 7515 base64url-without-padding.
func base64URLDecode(s []byte) ([]byte, error) {
	// Add padding back — JWT spec strips it but encoding/base64
	// requires it for non-RawURLEncoding decoders.
	pad := (4 - len(s)%4) % 4
	for range pad {
		s = append(s, '=')
	}
	return base64.URLEncoding.DecodeString(string(s))
}

// waitForCascadeComplete polls the refresh_token_families table for
// personID until every active family has been revoked — that's the
// LAST step in revokeAll, so once we observe zero active families
// the cascade has fully drained: the change-password tx committed,
// the outbox row was forwarded, the subscriber processed it,
// SecurityStampCache.Invalidate ran (BEFORE family revocation per
// revokeAll's cascade order), and each family was Revoke()'d.
//
// The earlier shape of this test polled /sessions and inferred state
// from the response body. That conflated two signals (cache state +
// family state) and was racy on busy CI runners — the polled request
// could observe families revoked while the freshness check still hit
// a yet-to-be-invalidated cache entry, since the cache invalidate
// and the family revoke are sequential but the next request lands
// concurrently with the cascade.
//
// budget caps the wait — under -race + busy CI Docker daemons the
// cascade can take 5-10s; 15s is generous while still well under the
// 30s SecurityStampCache TTL (which would mask the failure if we
// waited that long).
func waitForCascadeComplete(t *testing.T, pool *pgxpool.Pool, personID string, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		var activeFamilies int
		if err := pool.QueryRow(t.Context(), `
			SELECT count(*)
			FROM   identity.refresh_token_families
			WHERE  person_id = $1
			  AND  revoked_at IS NULL
		`, personID).Scan(&activeFamilies); err != nil {
			t.Fatalf("read family count: %v", err)
		}
		if activeFamilies == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waitForCascadeComplete: cascade did not revoke families within %s", budget)
}

// getWithBearer issues a GET with the bearer token and returns the
// status + body for assertion.
func getWithBearer(t *testing.T, url, bearer string) httpResp {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return httpResp{status: resp.StatusCode, body: body}
}

// postJSONWithBearer mirrors postJSON with an Authorization header.
func postJSONWithBearer(t *testing.T, url, bearer string, body any) httpResp {
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
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return httpResp{status: resp.StatusCode, body: respBody}
}
