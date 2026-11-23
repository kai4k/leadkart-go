//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"

	csports "github.com/leadkart/leadkart-go/internal/companysettings/ports"
)

// Impersonated changes: the settings row and the history name no membership the
// tenant does not have, and the history shows that support made the change
// WITHOUT handing the raw operator person id to tenant readers (review M5, L5).
func TestE2E_CompanySettings_AnImpersonatedChangeIsAttributedToSupportNotToAFakeMember(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerAndLogin(t, "cs-imp")
	_, sess := f.openImpersonation(t, reg.TenantID)

	patched := f.authedJSON(t, http.MethodPatch, csSettings, sess.AccessToken,
		csPatch(0, "", map[string]map[string]any{"expiry": {"stop_months_before": 2}}))
	if patched.status != http.StatusOK {
		t.Fatalf("impersonated patch: %d %s", patched.status, patched.body)
	}
	got := csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodGet, csSettings, reg.AccessToken, nil), http.StatusOK)
	if got.UpdatedByMembershipID != "" {
		t.Fatalf("updated_by_membership_id = %q: an impersonation session is not a membership of the tenant", got.UpdatedByMembershipID)
	}
	hist := csDecode[csports.ListHistoryResponse](t, f.authedJSON(t, http.MethodGet, csHistory, reg.AccessToken, nil), http.StatusOK)
	if len(hist.Items) == 0 {
		t.Fatal("no history")
	}
	last := hist.Items[0]
	if last.ActorMembershipID != "" {
		t.Fatalf("history actor_membership_id = %q: not a membership of the tenant", last.ActorMembershipID)
	}
	if !last.ChangedBySupport {
		t.Fatal("the history must show that LeadKart support made the change")
	}
}

// The tax gate on a GST rate edit: a caller with the manage permission only is
// refused (review M7).
func TestE2E_CompanySettings_EditingAGSTRateNeedsTheTaxPermission(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerAndLogin(t, "cs-ratepatch")
	p := permission.IdentityPermissions.CompanySettings
	rate := csDecode[csports.GSTRateChangedDto](t, f.authedJSON(t, http.MethodPost, csRates, reg.AccessToken,
		map[string]any{"rate_bps": 500, "label": "GST 5%", "confirmed_by": "A. Mehta", "confirmed_on": "2026-10-30", "reason": "per CA"}), http.StatusCreated)

	manageOnly := csTokenAt(t, f, reg, 10, p.Read, p.Manage)
	r := f.authedJSON(t, http.MethodPatch, csRates+"/"+rate.ID, manageOnly, map[string]any{"label": "GST five", "reason": "rename"})
	if r.status != http.StatusForbidden {
		t.Fatalf("a manage-only caller editing a rate: %d %s, want 403", r.status, r.body)
	}
	after := csDecode[csports.ListGSTRatesResponse](t, f.authedJSON(t, http.MethodGet, csRates, reg.AccessToken, nil), http.StatusOK)
	if after.Items[0].Label != "GST 5%" {
		t.Fatalf("the refused edit changed the rate: %+v", after.Items[0])
	}
}

// A rate edit that names a field it cannot change is refused, not a 200 that
// does nothing; a settings group named twice is refused too (review M4).
func TestE2E_CompanySettings_BodiesAreStrict(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerAndLogin(t, "cs-strict")
	rate := csDecode[csports.GSTRateChangedDto](t, f.authedJSON(t, http.MethodPost, csRates, reg.AccessToken,
		map[string]any{"rate_bps": 500, "label": "GST 5%", "confirmed_by": "A. Mehta", "confirmed_on": "2026-10-30", "reason": "per CA"}), http.StatusCreated)

	r := f.authedJSON(t, http.MethodPatch, csRates+"/"+rate.ID, reg.AccessToken, map[string]any{"rate_bps": 1800, "reason": "rate moved to 18"})
	if r.status != http.StatusBadRequest || errorCode(t, r.body) != "invalid_body" {
		t.Fatalf("an edit naming rate_bps: %d %s, want 400 invalid_body", r.status, r.body)
	}
	dup := f.authedJSON(t, http.MethodPatch, csSettings, reg.AccessToken,
		json.RawMessage(`{"expected_version":1,"changes":{"expiry":{"stop_months_before":5},"expiry":{"stop_months_before":2}}}`))
	if dup.status != http.StatusBadRequest || errorCode(t, dup.body) != "invalid_body" {
		t.Fatalf("a duplicate group: %d %s, want 400 invalid_body", dup.status, dup.body)
	}
}

// Two saves based on the same version of an EXISTING policy row: exactly one
// wins and the rest are 409, never a 500 (the row lock serialises them; review
// M7: the shipped test raced at version 0, where the INSERT serialises savers).
func TestE2E_CompanySettings_ConcurrentSavesOnASeededRowAreConflictsNotErrors(t *testing.T) {
	f := newE2EFixture(t)
	reg := f.registerAndLogin(t, "cs-race")
	version := csDecode[csports.SettingsDto](t, f.authedJSON(t, http.MethodPatch, csSettings, reg.AccessToken,
		csPatch(0, "", map[string]map[string]any{"expiry": {"stop_months_before": 2}})), http.StatusOK).Version

	// Several rounds: without the row lock the losers of one round fail on the
	// history primary key only when the saves truly overlap.
	const savers, rounds = 8, 12
	for round := range rounds {
		statuses := make(chan int, savers)
		var wg sync.WaitGroup
		for i := range savers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := f.authedJSON(t, http.MethodPatch, csSettings, reg.AccessToken,
					csPatch(version, "", map[string]map[string]any{"expiry": {"warning_days": []int{100 + round*savers + i}}}))
				statuses <- r.status
			}()
		}
		wg.Wait()
		close(statuses)
		ok, conflict := 0, 0
		for s := range statuses {
			switch s {
			case http.StatusOK:
				ok++
			case http.StatusConflict:
				conflict++
			default:
				t.Errorf("round %d: a concurrent save answered %d, want 200 or 409", round, s)
			}
		}
		if ok != 1 || conflict != savers-1 {
			t.Fatalf("round %d: 200s = %d, 409s = %d, want exactly one winner and %d conflicts", round, ok, conflict, savers-1)
		}
		version++
	}
}
