package ports_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/refreshmint"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken/refreshtokentest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// "Sign out my other devices" keeps the session the access token names, counts
// only the sessions it ended, and never quietly becomes "sign out everything".
func TestRevokeAllSessions_ExceptCurrentKeepsTheTokensOwnSession(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	claims := azClaims()
	const mine, other1, other2 = "0190a000-0000-7000-8000-00000000f001", "0190a000-0000-7000-8000-00000000f002", "0190a000-0000-7000-8000-00000000f003"
	setup := func(t *testing.T) (app.Application, *refreshtokentest.FakeRepository) {
		t.Helper()
		repo := refreshtokentest.NewFakeRepository()
		for _, id := range []string{mine, other1, other2} {
			pair, err := refreshmint.Mint()
			if err != nil {
				t.Fatal(err)
			}
			f, err := refreshtoken.NewFamily(refreshtoken.FamilyID(id), person.ID(claims.Subject), tenant.ID(azTenant), "device", pair.Hash, time.Hour, now)
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.Add(t.Context(), f); err != nil {
				t.Fatal(err)
			}
		}
		var a app.Application
		a.Commands.RevokeAllSessions = command.NewRevokeAllSessionsHandler(repo, func() time.Time { return now })
		return a, repo
	}
	active := func(t *testing.T, repo *refreshtokentest.FakeRepository) map[string]bool {
		t.Helper()
		fams, err := repo.ListActiveForPerson(t.Context(), person.ID(claims.Subject))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, f := range fams {
			out[f.ID().String()] = true
		}
		return out
	}
	count := func(t *testing.T, body []byte) int {
		t.Helper()
		var r struct {
			RevokedCount int `json:"revoked_count"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
		return r.RevokedCount
	}

	for name, body := range map[string]string{
		"a misspelt flag":          `{"exceptCurrent":true}`,
		"a flag next to a typo":    `{"except_current":true,"reasn":"x"}`,
		"a second JSON value":      `{"except_current":true}{"except_current":false}`,
		"a string where a bool is": `{"except_current":"true"}`,
	} {
		t.Run("refused and revokes nothing: "+name, func(t *testing.T) {
			t.Parallel()
			a, repo := setup(t)
			c := *claims
			c.FamilyID = mine
			rec := azServeApp(t, a, &c, http.MethodDelete, "/api/v1/auth/sessions", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d body %s, want 400", rec.Code, rec.Body.String())
			}
			if len(active(t, repo)) != 3 {
				t.Errorf("a refused body revoked sessions: %v", active(t, repo))
			}
		})
	}
	t.Run("an empty body is still the documented revoke-everything", func(t *testing.T) {
		t.Parallel()
		a, repo := setup(t)
		c := *claims
		c.FamilyID = mine
		if rec := azServeApp(t, a, &c, http.MethodDelete, "/api/v1/auth/sessions", ""); rec.Code != http.StatusOK || len(active(t, repo)) != 0 {
			t.Fatalf("empty body: status %d, active %v, want 200 and none left", rec.Code, active(t, repo))
		}
	})

	t.Run("except_current keeps the token's own session and counts the others", func(t *testing.T) {
		t.Parallel()
		a, repo := setup(t)
		c := *claims
		c.FamilyID = mine
		rec := azServeApp(t, a, &c, http.MethodDelete, "/api/v1/auth/sessions", `{"except_current":true}`)
		if rec.Code != http.StatusOK || count(t, rec.Body.Bytes()) != 2 {
			t.Fatalf("status %d body %s, want 200 with revoked_count 2 (the kept session is not counted)", rec.Code, rec.Body.String())
		}
		if got := active(t, repo); len(got) != 1 || !got[mine] {
			t.Errorf("active sessions after = %v, want only the current one", got)
		}
	})
	t.Run("without the flag everything goes, as before", func(t *testing.T) {
		t.Parallel()
		c := *claims
		c.FamilyID = mine
		for _, body := range []string{``, `{}`, `{"except_current":false}`} {
			a, repo := setup(t)
			rec := azServeApp(t, a, &c, http.MethodDelete, "/api/v1/auth/sessions", body)
			if rec.Code != http.StatusOK || count(t, rec.Body.Bytes()) != 3 || len(active(t, repo)) != 0 {
				t.Errorf("body %q: status %d %s, active %v; want all 3 revoked", body, rec.Code, rec.Body.String(), active(t, repo))
			}
		}
	})
	t.Run("a token that cannot name its session is refused, nothing is revoked", func(t *testing.T) {
		t.Parallel()
		a, repo := setup(t)
		rec := azServeApp(t, a, claims, http.MethodDelete, "/api/v1/auth/sessions", `{"except_current":true}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status %d %s, want 409 current_session_unknown", rec.Code, rec.Body.String())
		}
		if got := active(t, repo); len(got) != 3 {
			t.Errorf("a refused call revoked sessions: %v", got)
		}
	})
	t.Run("a body that does not parse is refused, never read as 'everything'", func(t *testing.T) {
		t.Parallel()
		a, repo := setup(t)
		c := *claims
		c.FamilyID = mine
		for _, body := range []string{`{"except_current":"true"}`, `{"except_current":`, `[]`, `7`} {
			rec := azServeApp(t, a, &c, http.MethodDelete, "/api/v1/auth/sessions", body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("body %q: status %d %s, want 400", body, rec.Code, rec.Body.String())
			}
		}
		if got := active(t, repo); len(got) != 3 {
			t.Errorf("a refused call revoked sessions: %v", got)
		}
	})
}
