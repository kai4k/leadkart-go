//go:build integration

// arch-test:no-timeout-needed — startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context(); the poll
// is bounded by its own deadline.
//
// arch-test:no-synctest — the audit row is written by the router after the forwarder
// publishes; synctest cannot model that path.

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// Review B5: an operator acts for a company under an impersonation session; the
// company owner reads the tenant audit feed and sees THAT support acted, never the
// operator person id.
func TestE2E_Audit_TheTenantFeedNeverShowsAnOperatorPersonId(t *testing.T) {
	f := newE2EFixtureWithCascade(t, time.Now)
	owner := f.registerAndLogin(t, "auditsupport")
	member := f.createMember(t, owner, "auditsupport-m")
	operatorTok, sess := f.openImpersonation(t, owner.TenantID)
	claims, err := f.Issuer.Verify(operatorTok)
	if err != nil {
		t.Fatalf("verify the operator token: %v", err)
	}
	op := claims.Subject

	r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+member.membershipID+"/deactivate", sess.AccessToken,
		ports.DeactivateUserRequest{Reason: "support ticket 4711 deactivation"})
	if r.status != http.StatusNoContent && r.status != http.StatusOK {
		t.Fatalf("deactivate under the session: %d %s", r.status, r.body)
	}

	deadline := time.Now().Add(15 * time.Second)
	var last []byte
	// arch-test:wait-justified — bounded poll for the async forwarder -> router -> audit write
	for time.Now().Before(deadline) {
		got := f.authedJSON(t, http.MethodGet, "/api/v1/tenants/"+owner.TenantID+"/audit/events?page_size=200", owner.AccessToken, nil)
		last = got.body
		if got.status != http.StatusOK {
			t.Fatalf("tenant feed: %d %s", got.status, got.body)
		}
		if strings.Contains(string(got.body), op) {
			t.Fatalf("the operator person id %s is visible to the tenant: %s", op, got.body)
		}
		var page ports.ListAuditEventsResponse
		if err := json.Unmarshal(got.body, &page); err != nil {
			t.Fatal(err)
		}
		for _, ev := range page.Events {
			if strings.Contains(ev.Action, "deactivated") && ev.ChangedBySupport {
				if ev.ActorID != "" {
					t.Fatalf("an impersonated row carries an actor: %+v", ev)
				}
				return
			}
		}
		time.Sleep(50 * time.Millisecond) // arch-test:wait-justified — poll interval inside a deadline-bounded loop
	}
	t.Fatalf("no support-attributed deactivation row on the tenant feed: %s", last)
}
