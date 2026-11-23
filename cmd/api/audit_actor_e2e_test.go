//go:build integration

// arch-test:no-timeout-needed — startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context(); the
// activity poll is bounded by its own deadline.
//
// arch-test:no-synctest — the audit row is written by the Watermill router
// after the outbox forwarder publishes; synctest cannot model that path.

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// TestE2E_Audit_ARealCommandAppearsInThePersonsOwnActivityWithItsActor: a
// person changes their password (a real command whose event a subscriber
// handles). The audit row the router writes must carry that person as its
// actor, so /auth/me/activity (filtered on user_id) lists it. Before the fix
// the audit middleware never set the user, the row had no actor and the
// person's feed stayed empty.
func TestE2E_Audit_ARealCommandAppearsInThePersonsOwnActivityWithItsActor(t *testing.T) {
	f := newE2EFixtureWithCascade(t, time.Now)
	reg := f.registerAndLogin(t, "auditactor")
	newPassword := reg.Password + " v2"

	resp := f.authedJSON(t, http.MethodPost, "/api/v1/auth/change-password", reg.AccessToken,
		ports.ChangePasswordRequest{CurrentPassword: reg.Password, NewPassword: newPassword})
	if resp.status != http.StatusNoContent {
		t.Fatalf("change-password: status %d body %s", resp.status, resp.body)
	}
	// The password change rotated the stamp: the old token is stale. A new login
	// reads the person's own activity.
	tok := f.loginPair(t, reg.Email, newPassword).AccessToken

	deadline := time.Now().Add(15 * time.Second)
	var last []byte
	// arch-test:wait-justified — bounded poll for the async forwarder -> router -> audit write; synctest is N/A across HTTP and SQL
	for time.Now().Before(deadline) {
		got := f.authedJSON(t, http.MethodGet, "/api/v1/auth/me/activity", tok, nil)
		last = got.body
		if got.status != http.StatusOK {
			t.Fatalf("activity: status %d body %s", got.status, got.body)
		}
		var page ports.ListAuditEventsResponse
		if err := json.Unmarshal(got.body, &page); err != nil {
			t.Fatalf("decode activity: %v", err)
		}
		for _, ev := range page.Events {
			if strings.Contains(ev.Action, "password") {
				if ev.ActorID != reg.PersonID {
					t.Fatalf("actor_id = %q, want %q", ev.ActorID, reg.PersonID)
				}
				if !ev.Succeeded || ev.FailureReason != "" {
					t.Fatalf("event %+v should be a clean success", ev)
				}
				return
			}
		}
		time.Sleep(50 * time.Millisecond) // arch-test:wait-justified — poll interval inside deadline-bounded loop awaiting the async audit write
	}
	t.Fatalf("the password change never appeared in the person's own activity; last body %s", last)
}
