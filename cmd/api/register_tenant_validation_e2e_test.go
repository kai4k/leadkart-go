//go:build integration

// arch-test:no-timeout-needed — newE2EFixture → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally; per-request HTTP uses t.Context().

// POST /api/v1/tenants: invalid input never answers 500. A blank or over-long
// company or admin name is 422 under the route's code (tenant_invalid for the
// company, person_invalid for the admin) with errors{<request field>: [<code>]};
// a malformed slug or email keeps its 400 (invalid_slug / invalid_email, the
// code every other route uses for the same rule). Nothing is written in any case.

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_RegisterTenant_InvalidInputIsRefusedNotA500_NothingWritten(t *testing.T) {
	f := newE2EFixture(t)
	p := permission.IdentityPermissions
	op := f.mintPlatformTokenWith(t, nil, true)
	reader := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.UsersView}, false)

	type in struct{ slug, email, legal, display, first, last string }
	good := in{"reg-ok", "reg-ok@e2e.test", "Globex Pvt Ltd", "Globex", "Ann", "Lee"}
	with := func(slug string, mod func(*in)) in {
		c := good
		c.slug, c.email = slug, slug+"@e2e.test"
		mod(&c)
		return c
	}
	register := func(c in) httpResp {
		return f.authedJSON(t, http.MethodPost, "/api/v1/tenants", op, ports.RegisterTenantRequest{
			Slug: c.slug, LegalName: c.legal, DisplayName: c.display, AdminEmail: c.email,
			AdminPassword: "Correct-Horse-Battery-9!x", AdminFirstName: c.first, AdminLastName: c.last,
		})
	}

	cases := []struct {
		name       string
		in         in
		wantStatus int
		wantCode   string
		wantField  string // "" for the 400 answers (no per-field map)
		wantFieldC string
	}{
		{"blank legal_name", with("reg-a", func(c *in) { c.legal = "   " }), 422, "tenant_invalid", "legal_name", "required"},
		{"empty display_name", with("reg-b", func(c *in) { c.display = "" }), 422, "tenant_invalid", "display_name", "required"},
		{"over-long legal_name", with("reg-c", func(c *in) { c.legal = strings.Repeat("x", 201) }), 422, "tenant_invalid", "legal_name", "too_long"},
		{"over-long display_name", with("reg-d", func(c *in) { c.display = strings.Repeat("x", 201) }), 422, "tenant_invalid", "display_name", "too_long"},
		{"blank admin_first_name", with("reg-e", func(c *in) { c.first = " " }), 422, "person_invalid", "admin_first_name", "required"},
		{"empty admin_last_name", with("reg-f", func(c *in) { c.last = "" }), 422, "person_invalid", "admin_last_name", "required"},
		{"over-long admin_first_name", with("reg-g", func(c *in) { c.first = strings.Repeat("x", 101) }), 422, "person_invalid", "admin_first_name", "too_long"},
		{"over-long admin_last_name", with("reg-h", func(c *in) { c.last = strings.Repeat("x", 101) }), 422, "person_invalid", "admin_last_name", "too_long"},
		{"bad slug", with("Bad Slug!", func(c *in) { c.email = "reg-slug@e2e.test" }), 400, "invalid_slug", "slug", "invalid_slug"},
		{"slug of two characters", with("ab", func(c *in) { c.email = "reg-ab@e2e.test" }), 400, "invalid_slug", "slug", "invalid_slug"},
		{"slug with a double hyphen", with("a--b-c", func(c *in) { c.email = "reg-dh@e2e.test" }), 400, "invalid_slug", "slug", "invalid_slug"},
		{"slug of 64 characters", with(strings.Repeat("s", 64), func(c *in) { c.email = "reg-64@e2e.test" }), 400, "invalid_slug", "slug", "invalid_slug"},
		{"bad email", with("reg-i", func(c *in) { c.email = "not-an-email" }), 400, "invalid_email", "admin_email", "invalid_email"},
		{"email with a doubled dot", with("reg-j", func(c *in) { c.email = "a..b@e2e.test" }), 400, "invalid_email", "admin_email", "invalid_email"},
	}
	for _, c := range cases {
		r := register(c.in)
		var er ports.ErrorResponse
		if err := json.Unmarshal(r.body, &er); err != nil {
			t.Fatalf("%s: decode %s: %v", c.name, r.body, err)
		}
		if r.status != c.wantStatus || er.Error != c.wantCode {
			t.Errorf("%s: %d %s; want %d %s", c.name, r.status, r.body, c.wantStatus, c.wantCode)
			continue
		}
		if c.wantField != "" && (len(er.Errors[c.wantField]) != 1 || er.Errors[c.wantField][0] != c.wantFieldC) {
			t.Errorf("%s: errors = %v; want {%s:[%s]}", c.name, er.Errors, c.wantField, c.wantFieldC)
		}
		if c.wantCode != "invalid_slug" {
			if got := f.authedJSON(t, http.MethodGet, "/api/v1/tenants/by-slug/"+c.in.slug, op, nil); got.status != http.StatusNotFound {
				t.Errorf("%s: a refused registration left a tenant behind: %d %s", c.name, got.status, got.body)
			}
		}
		if c.in.email != "not-an-email" && !strings.Contains(c.in.email, "..") {
			if got := f.authedJSON(t, http.MethodGet, "/api/v1/platform/persons?email="+c.in.email, reader, nil); got.status != http.StatusNotFound {
				t.Errorf("%s: a refused registration left a person behind: %d %s", c.name, got.status, got.body)
			}
		}
	}
}
