//go:build integration

// arch-test:no-timeout-needed — every request carries t.Context().

// Final fix round, commit 3: what DELETE /api/v1/auth/sessions/{familyId} really
// answers, so the spec and docs/BFF_API_CHANGES.md match the code.
package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func (f e2eFixture) sessionIDs(t *testing.T, token string) []string {
	t.Helper()
	r := f.authedJSON(t, http.MethodGet, "/api/v1/auth/sessions", token, nil)
	if r.status != http.StatusOK {
		t.Fatalf("list sessions: %d %s", r.status, r.body)
	}
	var out ports.ListSessionsResponse
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	var idsOut []string
	for _, s := range out.Sessions {
		idsOut = append(idsOut, s.FamilyID)
	}
	return idsOut
}

// Own session: 204, and 204 again (idempotent for the owner). An unknown id and
// another person's session: 404 session_not_found, the same answer for both (no
// session-id enumeration).
func TestE2E_RevokeSession_OwnIsIdempotent204_UnknownAndForeignAre404(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "sessrev")
	other := f.registerAndLogin(t, "sessrevo")

	mine := f.sessionIDs(t, owner.AccessToken)
	theirs := f.sessionIDs(t, other.AccessToken)
	if len(mine) == 0 || len(theirs) == 0 {
		t.Fatalf("setup: sessions %v / %v", mine, theirs)
	}

	del := func(id string) httpResp {
		return f.authedJSON(t, http.MethodDelete, "/api/v1/auth/sessions/"+id, owner.AccessToken, nil)
	}
	other404 := func(label string, r httpResp) {
		t.Helper()
		if r.status != http.StatusNotFound || decodeError(t, r.body).Error != ports.ErrCodeSessionNotFound {
			t.Fatalf("%s: %d %s, want 404 session_not_found", label, r.status, r.body)
		}
	}

	other404("another person's session", del(theirs[0]))
	unknown := del(ids.NewV7().String())
	other404("an unknown session", unknown)
	if surfaceOf(unknown) != surfaceOf(del(theirs[0])) {
		t.Fatal("an unknown session and another person's session answer differently: session ids can be enumerated")
	}

	if r := del(mine[0]); r.status != http.StatusNoContent {
		t.Fatalf("revoke own session: %d %s, want 204", r.status, r.body)
	}
	// The owner's token is a different matter (the access token outlives its
	// family); the second revoke of the SAME family is a no-op 204.
	if r := del(mine[0]); r.status != http.StatusNoContent {
		t.Fatalf("revoke own session again: %d %s, want the idempotent 204", r.status, r.body)
	}
	// The other person's session was untouched.
	if st, _ := f.mustRefresh(t, other.RefreshToken); st != http.StatusOK {
		t.Fatalf("another person's session after the probes: refresh %d, want 200", st)
	}
}
