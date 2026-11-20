//go:build integration

// arch-test:no-timeout-needed — every request carries t.Context() or a 60 s
// context; the racing goroutines are joined by a WaitGroup.
//
// arch-test:no-synctest — the grace window is measured on the injected clock
// (a frozen, manually advanced instant), and the races cross the HTTP transport
// and Postgres, which a virtual clock cannot model.
//
// arch-test:raw-sql-justified — the client binding and the replay counter of a
//   consumed token, and the revocation reason of a family, have no HTTP surface;
//   one test nulls a binding to prove a token that predates the migration is strict.

// Fix round 13, item 3 through the real HTTP stack: a grace replay is accepted only
// from the client that rotated the token (the verified client address the limiters
// use, here the X-Forwarded-For of a trusted proxy), at most three times; anything
// else is reuse and revokes the family, leaving the audit event the SIEM
// subscriber reads. Only an HMAC of the client is stored.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/refreshmint"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// refreshFrom presents tok to POST /auth/refresh as the client xff.
func (f e2eFixture) refreshFrom(ctx context.Context, xff, tok string) (int, ports.LoginResponse, error) {
	resp, err := f.tryDo(ctx, http.MethodPost, "/api/v1/auth/refresh", "", ports.RefreshRequest{RefreshToken: tok},
		map[string]string{"X-Forwarded-For": xff})
	if err != nil {
		return 0, ports.LoginResponse{}, err
	}
	var lr ports.LoginResponse
	if resp.status == http.StatusOK {
		_ = json.Unmarshal(resp.body, &lr) // arch-test:ignore-err - a body that does not decode leaves the empty token the caller asserts on
	}
	return resp.status, lr, nil
}

func (f e2eFixture) mustRefreshFrom(t *testing.T, xff, tok string) (int, ports.LoginResponse) {
	t.Helper()
	st, lr, err := f.refreshFrom(t.Context(), xff, tok)
	if err != nil {
		t.Fatalf("refresh from %s: %v", xff, err)
	}
	return st, lr
}

func (f e2eFixture) revokeReasonOf(t *testing.T, tok string) string {
	t.Helper()
	var reason *string
	if err := f.Pool.QueryRow(t.Context(),
		`SELECT f.revoke_reason FROM identity.refresh_tokens t JOIN identity.refresh_token_families f ON f.id = t.family_id WHERE t.token_hash = $1`,
		refreshmint.HashOf(tok)).Scan(&reason); err != nil {
		t.Fatalf("read revoke reason: %v", err)
	}
	if reason == nil {
		return ""
	}
	return *reason
}

func TestE2E_Refresh_GraceReplayIsBoundToTheClientThatRotated(t *testing.T) {
	clock := newGraceClock()
	f := newE2EFixtureGrace(t, clock.now, false, e2eGrace)
	tn := f.registerAndLogin(t, "grace-bind")
	const victim, thief = "198.51.100.10", "203.0.113.99"

	if s, _ := f.mustRefreshFrom(t, victim, tn.RefreshToken); s != http.StatusOK { // the first presentation rotates
		t.Fatalf("rotate: %d", s)
	}
	// The victim's own client replays: same successor.
	s2, b := f.mustRefreshFrom(t, victim, tn.RefreshToken)
	if s2 != http.StatusOK {
		t.Fatalf("replay by the client that rotated: %d, want 200", s2)
	}
	revokedBefore := len(outboxActFor(t, f, "refresh_token_family_revoked"))

	// The thief holds the same just-rotated token, inside the window.
	clock.advance(5 * time.Second)
	if s3, lr := f.mustRefreshFrom(t, thief, tn.RefreshToken); s3 != http.StatusUnauthorized || lr.RefreshToken != "" {
		t.Fatalf("replay by another client inside the window: %d %+v, want 401 and no token", s3, lr)
	}
	// The family is revoked: the victim's successor is dead too, the reason is the SIEM's trigger.
	if s4, _ := f.mustRefreshFrom(t, victim, b.RefreshToken); s4 != http.StatusUnauthorized {
		t.Fatalf("the victim's successor after the theft: %d, want 401", s4)
	}
	if got := f.revokeReasonOf(t, tn.RefreshToken); got != "reuse_detected" {
		t.Fatalf("revoke reason %q, want reuse_detected", got)
	}
	if after := len(outboxActFor(t, f, "refresh_token_family_revoked")); after != revokedBefore+1 {
		t.Fatalf("family-revoked outbox rows %d -> %d, want exactly one more (the audit event of the refused replay)", revokedBefore, after)
	}
}

func TestE2E_Refresh_ReplayBudgetOverrunRevokes(t *testing.T) {
	clock := newGraceClock()
	f := newE2EFixtureGrace(t, clock.now, false, e2eGrace)
	tn := f.registerAndLogin(t, "grace-budget")
	const client = "198.51.100.20"

	_, first := f.mustRefreshFrom(t, client, tn.RefreshToken)
	for i := 1; i <= refreshtoken.MaxGraceReplays; i++ {
		clock.advance(time.Second)
		s, lr := f.mustRefreshFrom(t, client, tn.RefreshToken)
		if s != http.StatusOK || lr.RefreshToken != first.RefreshToken {
			t.Fatalf("replay %d: %d same=%v", i, s, lr.RefreshToken == first.RefreshToken)
		}
	}
	clock.advance(time.Second)
	if s, _ := f.mustRefreshFrom(t, client, tn.RefreshToken); s != http.StatusUnauthorized {
		t.Fatalf("replay %d: %d, want 401", refreshtoken.MaxGraceReplays+1, s)
	}
	if s, _ := f.mustRefreshFrom(t, client, first.RefreshToken); s != http.StatusUnauthorized {
		t.Fatalf("the family must be revoked after the overrun: %d", s)
	}
}

// Only an HMAC of the client is stored, and a token whose binding is NULL (every
// token that predates the migration) is strict.
func TestE2E_Refresh_StoresAnHMACAndNullBindingIsStrict(t *testing.T) {
	clock := newGraceClock()
	f := newE2EFixtureGrace(t, clock.now, false, e2eGrace)
	tn := f.registerAndLogin(t, "grace-hmac")
	const client = "198.51.100.30"

	_, a := f.mustRefreshFrom(t, client, tn.RefreshToken)
	var binding *string
	var replays int
	read := func() {
		t.Helper()
		if err := f.Pool.QueryRow(t.Context(),
			`SELECT consumed_client_binding, grace_replays FROM identity.refresh_tokens WHERE token_hash = $1`,
			refreshmint.HashOf(tn.RefreshToken)).Scan(&binding, &replays); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if binding == nil || len(*binding) != 64 || *binding == client || replays != 0 {
		t.Fatalf("stored binding %v replays %d, want a 64-hex HMAC that is not the address and 0 replays", binding, replays)
	}
	if s, _ := f.mustRefreshFrom(t, client, tn.RefreshToken); s != http.StatusOK {
		t.Fatalf("replay: %d", s)
	}
	read()
	if replays != 1 {
		t.Fatalf("replay counter %d, want 1", replays)
	}

	// A token consumed before the migration: NULL binding. Even its own client is refused.
	if _, err := f.Pool.Exec(t.Context(), `UPDATE identity.refresh_tokens SET consumed_client_binding = NULL WHERE token_hash = $1`, refreshmint.HashOf(tn.RefreshToken)); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.mustRefreshFrom(t, client, tn.RefreshToken); s != http.StatusUnauthorized {
		t.Fatalf("replay of a NULL-bound token: %d, want 401 (strict)", s)
	}
	if s, _ := f.mustRefreshFrom(t, client, a.RefreshToken); s != http.StatusUnauthorized {
		t.Fatalf("the family must be revoked: %d", s)
	}
}

// 1 + MaxGraceReplays simultaneous presentations from one client: all answered 200
// with the same successor, ONE rotation row, and the replay counter is exact.
func TestE2E_Refresh_ConcurrentSameClientCountsReplaysExactly(t *testing.T) {
	clock := newGraceClock()
	f := newE2EFixtureGrace(t, clock.now, false, e2eGrace)
	tn := f.registerAndLogin(t, "grace-exact")
	const client = "198.51.100.40"
	const n = 1 + refreshtoken.MaxGraceReplays

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	start := make(chan struct{})
	statuses := make([]int, n)
	tokens := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			var lr ports.LoginResponse
			statuses[i], lr, errs[i] = f.refreshFrom(ctx, client, tn.RefreshToken)
			tokens[i] = lr.RefreshToken
		})
	}
	close(start)
	wg.Wait()
	for i := range n {
		if errs[i] != nil || statuses[i] != http.StatusOK || tokens[i] == "" || tokens[i] != tokens[0] {
			t.Fatalf("presentation %d: %v status=%d same=%v", i, errs[i], statuses[i], tokens[i] == tokens[0])
		}
	}
	if got := countFamilyTokens(t, f.Pool, tn.RefreshToken); got != 2 {
		t.Fatalf("family has %d tokens, want 2 (one rotation row)", got)
	}
	var replays int
	if err := f.Pool.QueryRow(t.Context(), `SELECT grace_replays FROM identity.refresh_tokens WHERE token_hash = $1`, refreshmint.HashOf(tn.RefreshToken)).Scan(&replays); err != nil {
		t.Fatal(err)
	}
	if replays != refreshtoken.MaxGraceReplays {
		t.Fatalf("replay counter %d, want exactly %d", replays, refreshtoken.MaxGraceReplays)
	}
}

// More simultaneous presentations than the budget: the extras are reuse. How many of
// the first answered before the revocation is a race, but never more than the budget
// allows, and the family is dead afterwards.
func TestE2E_Refresh_ConcurrentOverrunNeverExceedsTheBudget(t *testing.T) {
	clock := newGraceClock()
	f := newE2EFixtureGrace(t, clock.now, false, e2eGrace)
	tn := f.registerAndLogin(t, "grace-over")
	const client = "198.51.100.50"
	const n = 2 * (1 + refreshtoken.MaxGraceReplays)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	start := make(chan struct{})
	statuses := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			<-start
			statuses[i], _, errs[i] = f.refreshFrom(ctx, client, tn.RefreshToken)
		})
	}
	close(start)
	wg.Wait()
	ok := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("presentation %d: %v", i, errs[i])
		}
		if statuses[i] == http.StatusOK {
			ok++
		} else if statuses[i] != http.StatusUnauthorized {
			t.Fatalf("presentation %d: status %d", i, statuses[i])
		}
	}
	if ok > 1+refreshtoken.MaxGraceReplays {
		t.Fatalf("%d presentations answered 200, at most %d are allowed", ok, 1+refreshtoken.MaxGraceReplays)
	}
	if got := f.revokeReasonOf(t, tn.RefreshToken); got != "reuse_detected" {
		t.Fatalf("revoke reason %q, want reuse_detected after the overrun", got)
	}
}
