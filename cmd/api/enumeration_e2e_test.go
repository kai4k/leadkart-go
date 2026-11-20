//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

package main

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// loginSurface is what a client can observe of one failed login.
type loginSurface struct {
	status     int
	code       string
	retryAfter string
}

func (f e2eFixture) failedLogins(t *testing.T, emailAddr string, n int) []loginSurface {
	t.Helper()
	out := make([]loginSurface, 0, n)
	for range n {
		resp := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{
			Email: emailAddr, Password: "Wrong-Password-1!", DeviceLabel: "enum",
		})
		out = append(out, loginSurface{
			status:     resp.status,
			code:       decodeError(t, resp.body).Error,
			retryAfter: resp.header.Get("Retry-After"),
		})
	}
	return out
}

// Real Postgres counter against the phantom counter, through the real HTTP stack:
// ten wrong passwords lock a real account and an unknown address on the same
// attempt, with the same status, error code and Retry-After.
func TestE2E_Login_LockoutSurfaceIdenticalForRealAndUnknownEmail(t *testing.T) {
	f := newE2EFixture(t)
	real := f.registerAndLogin(t, "enum")
	realSeq := f.failedLogins(t, real.Email, 11)
	unknownSeq := f.failedLogins(t, fmt.Sprintf("nobody-here-%s@e2e.test", ids.NewV7().String()), 11)
	if !slices.Equal(realSeq, unknownSeq) {
		t.Fatalf("observable surface differs:\n real:    %+v\n unknown: %+v", realSeq, unknownSeq)
	}
	if realSeq[8].status != http.StatusUnauthorized || realSeq[9].status != http.StatusLocked {
		t.Fatalf("attempts 9 and 10: %d, %d; want 401 then 423", realSeq[8].status, realSeq[9].status)
	}
	if realSeq[9].retryAfter == "" {
		t.Fatal("423 without Retry-After")
	}
}

// A locked real account stays locked for the CORRECT password too, with the same
// answer an unknown address gets for any password.
func TestE2E_Login_LockedAccountAnswersTheSameForTheCorrectPassword(t *testing.T) {
	f := newE2EFixture(t)
	real := f.registerAndLogin(t, "enumlock")
	f.failedLogins(t, real.Email, 10)
	resp := f.postJSON(t, "/api/v1/auth/login", ports.LoginRequest{Email: real.Email, Password: real.Password, DeviceLabel: "enum"})
	if resp.status != http.StatusLocked || resp.header.Get("Retry-After") == "" {
		t.Fatalf("locked account with the correct password: %d (Retry-After %q), want 423", resp.status, resp.header.Get("Retry-After"))
	}
}

// Known and unknown address give the same answer on request-password-reset, and
// neither leaves the server before the response floor (a lower bound only:
// timers never fire early, so this cannot flake).
func TestE2E_RequestPasswordReset_SameAnswerAndAFloorForKnownAndUnknownEmail(t *testing.T) {
	f := newE2EFixture(t)
	real := f.registerAndLogin(t, "resetenum")
	ask := func(addr string) (httpResp, time.Duration) {
		start := time.Now()
		resp := f.postJSON(t, "/api/v1/auth/request-password-reset", ports.RequestPasswordResetRequest{Email: addr})
		return resp, time.Since(start)
	}
	known, knownTook := ask(real.Email)
	unknown, unknownTook := ask(fmt.Sprintf("never-registered-%s@nowhere.test", ids.NewV7().String()))
	if known.status != http.StatusNoContent || unknown.status != http.StatusNoContent {
		t.Fatalf("statuses %d / %d, want 204 / 204", known.status, unknown.status)
	}
	if string(known.body) != string(unknown.body) {
		t.Fatalf("bodies differ: %q vs %q", known.body, unknown.body)
	}
	for _, h := range []string{"Content-Type", "Content-Length"} {
		if known.header.Get(h) != unknown.header.Get(h) {
			t.Errorf("header %s differs: %q vs %q", h, known.header.Get(h), unknown.header.Get(h))
		}
	}
	const floor = 300 * time.Millisecond
	if knownTook < floor || unknownTook < floor {
		t.Fatalf("answered after %v (known) / %v (unknown), want at least the %v floor on both", knownTook, unknownTook, floor)
	}
}
