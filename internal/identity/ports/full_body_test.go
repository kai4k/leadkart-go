package ports

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeFull[T any](t *testing.T, body string) (T, *httptest.ResponseRecorder, bool) {
	t.Helper()
	var dst T
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPatch, "/x", strings.NewReader(body))
	ok := decodeFullBody(rec, req, &dst)
	return dst, rec, ok
}

func TestDecodeFullBody_RequiresEveryField(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		body        string
		wantOK      bool
		wantMissing []string
	}{
		{"all present", `{"legal_name":"A","display_name":"B"}`, true, nil},
		{"empty strings are present", `{"legal_name":"","display_name":""}`, true, nil},
		{"unknown keys ignored", `{"legal_name":"A","display_name":"B","extra":1}`, true, nil},
		{"one missing", `{"legal_name":"A"}`, false, []string{"display_name"}},
		{"all missing", `{}`, false, []string{"display_name", "legal_name"}},
		{"null counts as missing", `{"legal_name":null,"display_name":"B"}`, false, []string{"legal_name"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, rec, ok := decodeFull[UpdateTenantProfileRequest](t, tc.body)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (status %d %s)", ok, tc.wantOK, rec.Code, rec.Body)
			}
			if tc.wantOK {
				if got.LegalName != "A" && got.LegalName != "" {
					t.Errorf("decoded %+v", got)
				}
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			var resp ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Error != ErrCodeInvalidBody {
				t.Errorf("error code = %q, want %q", resp.Error, ErrCodeInvalidBody)
			}
			if len(resp.Errors) != len(tc.wantMissing) {
				t.Errorf("errors = %v, want exactly %v", resp.Errors, tc.wantMissing)
			}
			for _, f := range tc.wantMissing {
				if len(resp.Errors[f]) == 0 {
					t.Errorf("field %q has no error: %v", f, resp.Errors)
				}
			}
		})
	}
}

func TestDecodeFullBody_NestedObjectsAreRequiredToo(t *testing.T) {
	t.Parallel()
	_, rec, ok := decodeFull[UpdateTenantAdminContactRequest](t, `{"phone":"x","address":{"street":"s","city":"c"}}`)
	if ok {
		t.Fatal("accepted an address missing four fields")
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, f := range []string{"address.district", "address.pincode", "address.state", "address.state_code"} {
		if len(resp.Errors[f]) == 0 {
			t.Errorf("missing %q not reported: %v", f, resp.Errors)
		}
	}
	if _, _, ok := decodeFull[UpdateTenantAdminContactRequest](t, `{"phone":"x","address":{"street":"s","city":"c","district":"d","state":"s","state_code":"09","pincode":"1"}}`); !ok {
		t.Error("rejected a complete body")
	}
	if _, _, ok := decodeFull[UpdateTenantAdminContactRequest](t, `{"phone":"x","address":"nope"}`); ok {
		t.Error("accepted a non-object address")
	}
}

func TestDecodeFullBody_RejectsNonObjectAndGarbage(t *testing.T) {
	t.Parallel()
	for _, body := range []string{``, `[]`, `"x"`, `null`, `{`} {
		if _, rec, ok := decodeFull[UpdateTenantProfileRequest](t, body); ok || rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: ok=%v status=%d, want a 400", body, ok, rec.Code)
		}
	}
}

func TestDecodeFullBody_EmptyArraysArePresent(t *testing.T) {
	t.Parallel()
	got, _, ok := decodeFull[ReplaceUserPermissionOverridesRequest](t, `{"granted":[],"revoked":[],"overrides_version":"v"}`)
	if !ok || len(got.Granted) != 0 {
		t.Fatalf("empty arrays must be accepted (they clear the overlays): ok=%v %+v", ok, got)
	}
	if _, _, ok := decodeFull[ReplaceUserPermissionOverridesRequest](t, `{"granted":["a"],"overrides_version":"v"}`); ok {
		t.Error("a missing `revoked` must be rejected, not treated as clear")
	}
	if _, _, ok := decodeFull[ReplaceUserPermissionOverridesRequest](t, `{"granted":null,"revoked":[],"overrides_version":"v"}`); ok {
		t.Error("null `granted` must be rejected")
	}
	if _, _, ok := decodeFull[ReplaceUserPermissionOverridesRequest](t, `{"granted":[],"revoked":[]}`); ok {
		t.Error("a missing `overrides_version` must be rejected: a save without a precondition is the bug")
	}
}

func TestDecodeFullBody_RejectsDuplicateKeysAndTrailingData(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body string }{
		{"case-variant duplicate", `{"legal_name":"A","display_name":"B","Legal_Name":null}`},
		{"case-variant duplicate with a value", `{"legal_name":"A","display_name":"B","LEGAL_NAME":"C"}`},
		{"exact duplicate", `{"legal_name":"A","legal_name":"C","display_name":"B"}`},
		{"trailing value", `{"legal_name":"A","display_name":"B"} {"x":1}`},
		{"trailing garbage", `{"legal_name":"A","display_name":"B"} x`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, rec, ok := decodeFull[UpdateTenantProfileRequest](t, tc.body)
			if ok || rec.Code != http.StatusBadRequest {
				t.Fatalf("accepted (ok=%v status=%d): %s", ok, rec.Code, tc.body)
			}
		})
	}
}

func TestDecodeFullBody_ATypeErrorIsReportedAsATypeError(t *testing.T) {
	t.Parallel()
	_, rec, ok := decodeFull[UpdateTenantProfileRequest](t, `{"legal_name":5,"display_name":"B"}`)
	if ok || rec.Code != http.StatusBadRequest {
		t.Fatalf("ok=%v status=%d", ok, rec.Code)
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if got := resp.Errors["legal_name"]; len(got) != 1 || got[0] != "invalid_type" {
		t.Errorf("errors = %v, want legal_name: [invalid_type] (not \"required\")", resp.Errors)
	}
	if strings.Contains(rec.Body.String(), "required") {
		t.Errorf("a wrong-typed value was reported as missing: %s", rec.Body)
	}
}

func TestDecodeFullBody_ANonObjectNestedValueIsATypeErrorNotRequired(t *testing.T) {
	t.Parallel()
	_, rec, ok := decodeFull[UpdateTenantAdminContactRequest](t, `{"phone":"x","address":"nope"}`)
	if ok {
		t.Fatal("accepted a non-object address")
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if got := resp.Errors["address"]; len(got) != 1 || got[0] != "invalid_type" {
		t.Errorf("errors = %v, want address: [invalid_type]", resp.Errors)
	}
}

func TestDecodeFullBody_AnonymiseRequiresAReason(t *testing.T) {
	t.Parallel()
	for _, body := range []string{``, `{}`, `{"reason":null}`} {
		if _, rec, ok := decodeFull[AnonymiseRequest](t, body); ok || rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: ok=%v status=%d, want 400", body, ok, rec.Code)
		}
	}
	if got, _, ok := decodeFull[AnonymiseRequest](t, `{"reason":"erasure request"}`); !ok || got.Reason != "erasure request" {
		t.Errorf("valid body rejected: %+v", got)
	}
}

func decodeOptional[T any](t *testing.T, body string) (T, *httptest.ResponseRecorder, bool) {
	t.Helper()
	var dst T
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/x", strings.NewReader(body))
	ok := decodeOptionalBody(rec, req, &dst)
	return dst, rec, ok
}

// An optional body may be absent, but a present one is read strictly: a misspelt
// key must never read as "not sent".
func TestDecodeOptionalBody_AbsentIsFineAPresentBodyIsStrict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		body   string
		wantOK bool
	}{
		{"empty body", ``, true},
		{"a known key", `{"decision_reason":"ok"}`, true},
		{"an empty object", `{}`, true},
		{"a misspelt key", `{"decisionReason":"ok"}`, false},
		{"a known key beside an unknown one", `{"decision_reason":"ok","x":1}`, false},
		{"not JSON", `nope`, false},
		{"the wrong type", `{"decision_reason":5}`, false},
		{"a second JSON value", `{}{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, rec, ok := decodeOptional[ApprovePermissionRequestRequest](t, tc.body)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (status %d %s)", ok, tc.wantOK, rec.Code, rec.Body)
			}
			if !tc.wantOK {
				var resp ErrorResponse
				if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &resp) != nil || resp.Error != ErrCodeInvalidBody {
					t.Errorf("answer = %d %s, want 400 invalid_body", rec.Code, rec.Body)
				}
			}
		})
	}
}
