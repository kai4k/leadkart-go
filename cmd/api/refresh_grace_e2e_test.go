//go:build integration

// arch-test:no-timeout-needed — every request carries t.Context() or a 60 s
// context; the racing goroutines are joined by a WaitGroup.
//
// arch-test:no-synctest — the grace window is measured on the injected clock
// (a frozen, manually advanced instant), and the races cross the HTTP transport
// and Postgres, which a virtual clock cannot model.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/identity/app/refreshmint"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/identitytest"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// graceClock is a frozen instant the test advances by hand: the grace window is
// decided on it, so no test sleeps.
type graceClock struct{ ns atomic.Int64 }

func newGraceClock() *graceClock {
	c := &graceClock{}
	c.ns.Store(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *graceClock) now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *graceClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

const e2eGrace = 30 * time.Second

// refreshWith presents tok to POST /auth/refresh and returns the status and the
// decoded body of a 200. It never fails the test, so goroutines can use it.
func (f e2eFixture) refreshWith(ctx context.Context, tok string) (int, ports.LoginResponse, error) {
	resp, err := f.tryDo(ctx, http.MethodPost, "/api/v1/auth/refresh", "", ports.RefreshRequest{RefreshToken: tok}, nil)
	if err != nil {
		return 0, ports.LoginResponse{}, err
	}
	var lr ports.LoginResponse
	if resp.status == http.StatusOK {
		_ = json.Unmarshal(resp.body, &lr) // arch-test:ignore-err - a body that does not decode leaves the empty token the caller asserts on
	}
	return resp.status, lr, nil
}

func (f e2eFixture) mustRefresh(t *testing.T, tok string) (int, ports.LoginResponse) {
	t.Helper()
	st, lr, err := f.refreshWith(t.Context(), tok)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	return st, lr
}

// countFamilyTokens counts the rows of the family that holds refreshToken.
func countFamilyTokens(t *testing.T, pool *pgxpool.Pool, refreshToken string) int {
	t.Helper()
	return identitytest.CountFamilyTokens(t, pool, refreshmint.HashOf(refreshToken))
}

func TestE2E_Refresh_GraceReplayThenReuseRevokes(t *testing.T) {
	clock := newGraceClock()
	f := newE2EFixtureGrace(t, clock.now, false, e2eGrace)
	tn := f.registerAndLogin(t, "grace")

	s1, a := f.mustRefresh(t, tn.RefreshToken)
	s2, b := f.mustRefresh(t, tn.RefreshToken) // concurrent BFF refresh with the same token
	if s1 != http.StatusOK || s2 != http.StatusOK || a.RefreshToken != b.RefreshToken || a.RefreshToken == tn.RefreshToken {
		t.Fatalf("grace replay: %d %d same=%v", s1, s2, a.RefreshToken == b.RefreshToken)
	}
	if n := countFamilyTokens(t, f.Pool, tn.RefreshToken); n != 2 {
		t.Fatalf("a replay must write nothing: family has %d tokens, want 2", n)
	}
	s3, c := f.mustRefresh(t, a.RefreshToken) // successor used: chain moves on
	if s3 != http.StatusOK {
		t.Fatalf("rotate successor: %d", s3)
	}
	if s4, _ := f.mustRefresh(t, tn.RefreshToken); s4 != http.StatusUnauthorized { // two generations old: reuse
		t.Fatalf("stale replay: %d, want 401", s4)
	}
	if s5, _ := f.mustRefresh(t, c.RefreshToken); s5 != http.StatusUnauthorized { // family revoked
		t.Fatalf("after reuse the family must be revoked: %d", s5)
	}
}

func TestE2E_Refresh_ReplayOutsideWindowIsReuse(t *testing.T) {
	clock := newGraceClock()
	f := newE2EFixtureGrace(t, clock.now, false, e2eGrace)
	tn := f.registerAndLogin(t, "grace-late")

	s1, a := f.mustRefresh(t, tn.RefreshToken)
	if s1 != http.StatusOK {
		t.Fatalf("rotate: %d", s1)
	}
	clock.advance(e2eGrace + time.Second)
	if s2, _ := f.mustRefresh(t, tn.RefreshToken); s2 != http.StatusUnauthorized {
		t.Fatalf("late replay: %d, want 401", s2)
	}
	if s3, _ := f.mustRefresh(t, a.RefreshToken); s3 != http.StatusUnauthorized {
		t.Fatalf("the late replay must have revoked the family: %d", s3)
	}
}

func TestE2E_Refresh_GraceNeverResurrectsARevokedFamily(t *testing.T) {
	for _, tc := range []struct {
		name   string
		revoke func(t *testing.T, f e2eFixture, tn registeredTenant, successor string)
	}{
		{"logout", func(t *testing.T, f e2eFixture, _ registeredTenant, successor string) {
			if r := f.postJSON(t, "/api/v1/auth/logout", ports.LogoutRequest{RefreshToken: successor}); r.status >= 300 {
				t.Fatalf("logout: %d %s", r.status, r.body)
			}
		}},
		{"password change", func(t *testing.T, f e2eFixture, tn registeredTenant, _ string) {
			if r := f.authedJSON(t, http.MethodPost, "/api/v1/auth/change-password", tn.AccessToken,
				ports.ChangePasswordRequest{CurrentPassword: tn.Password, NewPassword: "Rotated-Pass-2026!grace"}); r.status != http.StatusNoContent {
				t.Fatalf("change-password: %d %s", r.status, r.body)
			}
		}},
	} {
		// arch-test:serial — each case boots its own Postgres container; running them side by side only slows Docker Desktop.
		t.Run(tc.name, func(t *testing.T) {
			clock := newGraceClock()
			f := newE2EFixtureGrace(t, clock.now, false, e2eGrace)
			tn := f.registerAndLogin(t, "grace-rev")
			s1, a := f.mustRefresh(t, tn.RefreshToken)
			if s1 != http.StatusOK {
				t.Fatalf("rotate: %d", s1)
			}
			tc.revoke(t, f, tn, a.RefreshToken)
			// Inside the window the replay's successor is the unconsumed head, but the family is dead.
			if s, lr := f.mustRefresh(t, tn.RefreshToken); s != http.StatusUnauthorized || lr.AccessToken != "" || lr.RefreshToken != "" {
				t.Fatalf("replay on a revoked family: %d %+v, want 401 and no token", s, lr)
			}
			if s, _ := f.mustRefresh(t, a.RefreshToken); s != http.StatusUnauthorized {
				t.Fatalf("successor on a revoked family: %d, want 401", s)
			}
		})
	}
}

// Simultaneous presentations of one token from one client: all get 200 and the SAME
// successor, and exactly one rotation row is written. One of them rotates and the
// others are replays, so at most 1 + MaxGraceReplays (4) can be answered: changed
// deliberately in fix round 13 (it ran 8 before replays were bounded; the 5th and
// later are reuse, see TestE2E_Refresh_ReplayBudgetOverrunRevokes).
func TestE2E_Refresh_ConcurrentPresentationsShareOneSuccessor(t *testing.T) {
	clock := newGraceClock()
	f := newE2EFixtureGrace(t, clock.now, false, e2eGrace)
	tn := f.registerAndLogin(t, "grace-conc")

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
			statuses[i], lr, errs[i] = f.refreshWith(ctx, tn.RefreshToken)
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
	if s, _ := f.mustRefresh(t, tokens[0]); s != http.StatusOK {
		t.Fatalf("the shared successor must be usable: %d", s)
	}
}

// Replays race a password change and a logout: nothing deadlocks, afterwards no
// refresh token of the family works, and no access token handed out in the race
// passes.
func TestE2E_Refresh_GraceRacesPasswordChangeAndLogout(t *testing.T) {
	clock := newGraceClock()
	f := newE2EFixtureGrace(t, clock.now, false, e2eGrace)
	tn := f.registerAndLogin(t, "grace-pwrace")
	s1, a := f.mustRefresh(t, tn.RefreshToken)
	if s1 != http.StatusOK {
		t.Fatalf("rotate: %d", s1)
	}

	const n = 6
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	start := make(chan struct{})
	var wg sync.WaitGroup
	access := make([]string, n)
	statuses := make([]int, n)
	errs := make([]error, n)
	var cpStatus, loStatus int
	var cpErr, loErr error
	for i := range n {
		wg.Go(func() {
			<-start
			var lr ports.LoginResponse
			statuses[i], lr, errs[i] = f.refreshWith(ctx, tn.RefreshToken)
			access[i] = lr.AccessToken
		})
	}
	wg.Go(func() {
		<-start
		r, err := f.tryDo(ctx, http.MethodPost, "/api/v1/auth/change-password", tn.AccessToken,
			ports.ChangePasswordRequest{CurrentPassword: tn.Password, NewPassword: "Rotated-Pass-2026!race"}, nil)
		cpStatus, cpErr = r.status, err
	})
	wg.Go(func() {
		<-start
		r, err := f.tryDo(ctx, http.MethodPost, "/api/v1/auth/logout", "", ports.LogoutRequest{RefreshToken: a.RefreshToken}, nil)
		loStatus, loErr = r.status, err
	})
	close(start)
	wg.Wait()

	if cpErr != nil || cpStatus != http.StatusNoContent || loErr != nil || loStatus >= 300 {
		t.Fatalf("change-password %d %v, logout %d %v", cpStatus, cpErr, loStatus, loErr)
	}
	f.evictStamp(t, tn.PersonID)
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("replay %d: %v", i, errs[i])
		}
		if statuses[i] != http.StatusOK && statuses[i] != http.StatusUnauthorized {
			t.Fatalf("replay %d: status %d", i, statuses[i])
		}
		if statuses[i] == http.StatusOK {
			if r := f.authedJSON(t, http.MethodGet, "/api/v1/auth/sessions", access[i], nil); r.status != http.StatusUnauthorized {
				t.Fatalf("replay %d handed out a token that still works after the change: %d", i, r.status)
			}
		}
	}
	for _, tok := range []string{tn.RefreshToken, a.RefreshToken} {
		if s, _ := f.mustRefresh(t, tok); s != http.StatusUnauthorized {
			t.Fatalf("a refresh token of the revoked family works: %d", s)
		}
	}
}
