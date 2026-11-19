package ports

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/dispatch/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// order_id (body and query) goes through authn.CanonicalID like every other id:
// whitespace and the nil UUID are rejected, never trimmed or accepted. An empty
// app.Application proves the request is refused before any command runs.
func TestOrderID_RejectsWhitespaceAndNilUUID(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	const tenantID = "0190a000-0000-7000-8000-00000000000a"
	const orderID = "0190a000-0000-7000-8000-0000000000ab"
	ctx := func(r *http.Request) *http.Request {
		c := &jwt.Claims{TenantID: tenantID, MembershipID: "0190a000-0000-7000-8000-0000000000cc"}
		return r.WithContext(tenancy.WithID(authn.WithClaims(r.Context(), c), tenancy.ID(tenantID)))
	}
	for name, bad := range map[string]string{
		"leading space":  " " + orderID,
		"trailing space": orderID + " ",
		"nil uuid":       "00000000-0000-0000-0000-000000000000",
	} {
		t.Run("body "+name, func(t *testing.T) {
			t.Parallel()
			body := `{"order_id":"` + bad + `","carrier_name":"c","box_count":1,"weight_grams":1}`
			r := ctx(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", bytes.NewBufferString(body)))
			rec := httptest.NewRecorder()
			handleCreate(log, app.Application{}).ServeHTTP(rec, r)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), errCodeInvalidOrderID) {
				t.Fatalf("%q: %d %s, want 400 %s", bad, rec.Code, rec.Body.String(), errCodeInvalidOrderID)
			}
		})
		t.Run("query "+name, func(t *testing.T) {
			t.Parallel()
			r := ctx(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/?order_id="+strings.ReplaceAll(bad, " ", "%20"), http.NoBody))
			rec := httptest.NewRecorder()
			handleGetByOrder(log, app.Application{}).ServeHTTP(rec, r)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), errCodeInvalidOrderID) {
				t.Fatalf("%q: %d %s, want 400 %s", bad, rec.Code, rec.Body.String(), errCodeInvalidOrderID)
			}
		})
	}
}
