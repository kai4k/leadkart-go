package ports_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// readUser is GET /users/{id} decoded, plus the raw keys so a test can tell an
// absent field from an empty one.
func readUser(t *testing.T, rec *httptest.ResponseRecorder) (ports.UserDto, map[string]json.RawMessage) {
	t.Helper()
	body := rec.Body.Bytes()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET user: %d %s", rec.Code, body)
	}
	var dto ports.UserDto
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &dto); err != nil {
		t.Fatalf("decode user: %v", err)
	}
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatalf("decode keys: %v", err)
	}
	return dto, keys
}

// The single-user read hands the overrides' version to exactly the callers it
// hands the overrides to; the save applies only with the version of the live
// state, and every save changes it.
func TestUserPermissionOverrides_TheSaveNeedsTheVersionOfTheRead(t *testing.T) {
	t.Parallel()
	pm := permission.IdentityPermissions
	a := selfTargetApp(t)
	admin := bodyAdminClaims(pm.Meta.TenantAdmin)
	userPath := "/api/v1/users/" + azOther
	get := func() (ports.UserDto, map[string]json.RawMessage) {
		rec := azServeApp(t, a, admin, "GET", userPath, "")
		return readUser(t, rec)
	}
	save := func(version string) (int, ports.ErrorResponse) {
		body, err := json.Marshal(ports.ReplaceUserPermissionOverridesRequest{Granted: []string{pm.Tasks.WorkItems.Reassign}, Revoked: []string{}, OverridesVersion: version})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		rec := azServeApp(t, a, admin, "PATCH", userPath+"/permission-overrides", string(body))
		var er ports.ErrorResponse
		if rec.Body.Len() > 0 {
			if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
				t.Fatalf("decode %s: %v", rec.Body.String(), err)
			}
		}
		return rec.Code, er
	}

	before, keys := get()
	if before.PermissionOverrides == nil || before.OverridesVersion == "" {
		t.Fatalf("the read gave overrides %+v and version %q to a caller who may change them, want both (keys %v)", before.PermissionOverrides, before.OverridesVersion, keys)
	}
	if code, er := save("ov1-not-the-version-of-anything"); code != http.StatusConflict || er.Error != ports.ErrCodeOverridesChanged {
		t.Fatalf("a save with another state's version: %d %q, want 409 %s", code, er.Error, ports.ErrCodeOverridesChanged)
	}
	if now, _ := get(); now.OverridesVersion != before.OverridesVersion || len(now.PermissionOverrides.Granted) != 0 {
		t.Fatalf("the refused save changed the overrides: %+v (version %q, was %q)", now.PermissionOverrides, now.OverridesVersion, before.OverridesVersion)
	}
	if code, er := save(before.OverridesVersion); code != http.StatusNoContent {
		t.Fatalf("a save with the version of the read: %d %q, want 204", code, er.Error)
	}
	after, _ := get()
	if after.OverridesVersion == "" || after.OverridesVersion == before.OverridesVersion {
		t.Fatalf("the version after a save is %q (was %q), want a new one", after.OverridesVersion, before.OverridesVersion)
	}
	// the same form saved twice: its version is now the previous state's
	if code, er := save(before.OverridesVersion); code != http.StatusConflict || er.Error != ports.ErrCodeOverridesChanged {
		t.Fatalf("the same form saved a second time: %d %q, want 409 %s", code, er.Error, ports.ErrCodeOverridesChanged)
	}
}

// A caller who is not shown the overrides is not shown their version either.
func TestUserPermissionOverrides_TheVersionIsAbsentWithTheOverrides(t *testing.T) {
	t.Parallel()
	pm := permission.IdentityPermissions
	a := selfTargetApp(t)
	for name, tc := range map[string]struct {
		path   string
		claims []string
	}{
		"a caller without the permission that changes them": {"/api/v1/users/" + azOther, []string{pm.Users.View}},
		"a caller reading themself":                         {"/api/v1/users/" + azSelf, []string{pm.Meta.TenantAdmin}},
	} {
		rec := azServeApp(t, a, bodyAdminClaims(tc.claims...), "GET", tc.path, "")
		_, keys := readUser(t, rec)
		for _, k := range []string{"permission_overrides", "overrides_version"} {
			if _, present := keys[k]; present {
				t.Errorf("%s: the read carries %s (%s)", name, k, rec.Body.String())
			}
		}
	}
}

// overrides_version is a required field of the save: absent, null or empty it
// is a 400 naming the field, and nothing is decided.
func TestUserPermissionOverrides_TheVersionIsRequired(t *testing.T) {
	t.Parallel()
	pm := permission.IdentityPermissions
	a := selfTargetApp(t)
	for name, body := range map[string]string{
		"absent": `{"granted":[],"revoked":[]}`,
		"null":   `{"granted":[],"revoked":[],"overrides_version":null}`,
		"empty":  `{"granted":[],"revoked":[],"overrides_version":""}`,
		"blank":  `{"granted":[],"revoked":[],"overrides_version":"  "}`,
	} {
		rec := azServeApp(t, a, bodyAdminClaims(pm.Meta.TenantAdmin), "PATCH", "/api/v1/users/"+azOther+"/permission-overrides", body)
		var er ports.ErrorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
			t.Fatalf("%s: decode %s: %v", name, rec.Body.String(), err)
		}
		if rec.Code != http.StatusBadRequest || er.Error != ports.ErrCodeInvalidBody {
			t.Errorf("%s: %d %q, want 400 %s", name, rec.Code, er.Error, ports.ErrCodeInvalidBody)
		}
		if got := er.Errors["overrides_version"]; len(got) != 1 || got[0] != "required" {
			t.Errorf("%s: errors[overrides_version] = %v, want [required]", name, got)
		}
	}
}
