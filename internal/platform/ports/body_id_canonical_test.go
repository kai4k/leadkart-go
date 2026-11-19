package ports_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
	"github.com/leadkart/leadkart-go/internal/platform/ports"
)

// tenant_id in the topup body reaches the command in canonical form: a client
// that spells it in upper case credits the SAME wallet, not a second one.
func TestTopupLeadCredits_TenantIDReachesCommandCanonical(t *testing.T) {
	t.Parallel()
	tenantID := ids.NewV7().String()
	a, _, credits := buildApp(t)
	mux := wireMux(t, platformClaims([]string{permission.IdentityPermissions.PlatformLeadCredits.Topup}), a)
	for _, spelled := range []string{strings.ToUpper(tenantID), "urn:uuid:" + tenantID, "{" + tenantID + "}", tenantID} {
		rec := doRequest(t, mux, "POST", "/api/v1/platform/lead-credits/topup", ports.TopupLeadCreditsRequest{TenantID: spelled, Delta: 10, Reason: "test"})
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tenantID) {
			t.Fatalf("%q: %d %s, want 200 echoing the canonical tenant id", spelled, rec.Code, rec.Body.String())
		}
	}
	c, err := credits.GetByTenant(t.Context(), leadcredit.TenantID(tenantID))
	if err != nil || c.Balance() != 40 {
		t.Fatalf("canonical wallet balance = %v, %v; want 40 (one wallet for all four spellings)", c, err)
	}
}

func TestTopupLeadCredits_PaddedAndNilTenantIDAreRejected(t *testing.T) {
	t.Parallel()
	tenantID := ids.NewV7().String()
	a, _, _ := buildApp(t)
	mux := wireMux(t, platformClaims([]string{permission.IdentityPermissions.PlatformLeadCredits.Topup}), a)
	for name, bad := range map[string]string{"leading": " " + tenantID, "trailing": tenantID + " ", "wrapped": " " + tenantID + " ", "nil": "00000000-0000-0000-0000-000000000000"} {
		rec := doRequest(t, mux, "POST", "/api/v1/platform/lead-credits/topup", ports.TopupLeadCreditsRequest{TenantID: bad, Delta: 10, Reason: "test"})
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), ports.ErrCodeInvalidTenantID) {
			t.Errorf("%s: %d %s, want 400 %s", name, rec.Code, rec.Body.String(), ports.ErrCodeInvalidTenantID)
		}
	}
}
