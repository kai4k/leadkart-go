//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// Hierarchy round 6, I2: an approval and the requester's cancel of the same
// request serialise. After every round the request is either approved WITH its
// grant or cancelled WITHOUT it, and every caller was told the truth.

package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_PermissionRequests_ApproveAndCancelNeverMix(t *testing.T) {
	w := newAttackWorld(t)
	x := permission.IdentityPermissions.Inventory.Catalog.Manage
	exec2 := w.mid(t, "exec2")
	grants := func() int { return strings.Count(w.overridesOf(t, w.owner.TenantID, exec2), x+"@") }
	var approvedRounds, cancelledRounds int
	for round := range raceRounds {
		w.relogin(t, "exec2")
		id := w.submitFor(t, "exec2", x)
		res := w.raceOnce(t,
			w.call(http.MethodPost, "/api/v1/permission-requests/"+id+"/approve", w.tok(t, "admin"), ports.ApprovePermissionRequestRequest{DecisionReason: "race"}),
			w.call(http.MethodPost, "/api/v1/permission-requests/"+id+"/cancel", w.tok(t, "exec2"), nil))
		onlyStatuses(t, "approve", res[0], http.StatusNoContent, http.StatusConflict)
		onlyStatuses(t, "cancel", res[1], http.StatusNoContent, http.StatusConflict, http.StatusUnauthorized)
		state, granted := w.requestState(t, id), grants()
		switch {
		case state == "approved" && granted == 1 && res[0].status == http.StatusNoContent && res[1].status != http.StatusNoContent:
			approvedRounds++
		case state == "cancelled" && granted == 0 && res[1].status == http.StatusNoContent && res[0].status != http.StatusNoContent:
			cancelledRounds++
		default:
			t.Fatalf("round %d: request %s with %d grants; approve answered %d, cancel answered %d: a request is approved with its grant or cancelled without it, and each caller is told which", round, state, granted, res[0].status, res[1].status)
		}
		w.resetGrants(t, "exec2")
		w.checkInvariants(t, "approve vs cancel")
	}
	t.Logf("approve won %d rounds, cancel won %d rounds", approvedRounds, cancelledRounds)
}
