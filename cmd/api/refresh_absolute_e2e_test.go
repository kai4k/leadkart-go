//go:build integration

// arch-test:no-timeout-needed — every request carries t.Context().
//
// arch-test:no-synctest — the lifetime is measured on the injected clock (a
// frozen, manually advanced instant); the requests cross HTTP and Postgres.
//
// arch-test:raw-sql-justified — the revocation reason of a family has no HTTP
//   surface.

package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/refreshmint"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// Fix round 13, item 12: a session ends at Refresh.AbsoluteTTL (14 days here)
// after its login even though it was refreshed every day; the refusal names the
// reason the BFF maps to "sign in again", and the family is revoked.
func TestE2E_Refresh_AbsoluteLifetimeEndsAnOtherwiseHealthySession(t *testing.T) {
	clock := newGraceClock()
	f := newE2EFixtureGrace(t, clock.now, false, 0)
	tn := f.registerAndLogin(t, "absolute")
	const abs = 14 * 24 * time.Hour
	start := clock.now()

	tok := tn.RefreshToken
	for day := 1; day <= 13; day++ {
		clock.advance(24 * time.Hour)
		st, lr := f.mustRefresh(t, tok)
		if st != http.StatusOK {
			t.Fatalf("day %d: %d", day, st)
		}
		tok = lr.RefreshToken
	}
	clock.advance(24*time.Hour - time.Second) // one second before the deadline
	if got := clock.now().Sub(start); got != abs-time.Second {
		t.Fatalf("test clock at %v, want %v", got, abs-time.Second)
	}
	st, lr := f.mustRefresh(t, tok)
	if st != http.StatusOK {
		t.Fatalf("one second before the absolute lifetime: %d, want 200", st)
	}
	tok = lr.RefreshToken

	clock.advance(2 * time.Second) // one second after
	r := f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: tok})
	e := decodeError(t, r.body)
	if r.status != http.StatusUnauthorized || e.Error != ports.ErrCodeRefreshRejected || e.Reason != ports.RefreshReasonSessionExpired {
		t.Fatalf("after the absolute lifetime: %d %s, want 401 refresh_rejected / session_expired", r.status, r.body)
	}
	var reason string
	if err := f.Pool.QueryRow(t.Context(),
		`SELECT f.revoke_reason FROM identity.refresh_tokens t JOIN identity.refresh_token_families f ON f.id = t.family_id WHERE t.token_hash = $1`,
		refreshmint.HashOf(tok)).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "absolute_ttl" {
		t.Fatalf("revoke reason %q, want absolute_ttl", reason)
	}
	// An unknown token has no reason: the arm names itself only to the session's holder.
	r2 := f.postJSON(t, "/api/v1/auth/refresh", ports.RefreshRequest{RefreshToken: "not-a-token"})
	if e2 := decodeError(t, r2.body); r2.status != http.StatusUnauthorized || e2.Reason != "" {
		t.Fatalf("unknown token: %d %+v, want 401 without a reason", r2.status, e2)
	}
}
