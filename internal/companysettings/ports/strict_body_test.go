package ports

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decodeBody[T any](t *testing.T, body string) (T, int) {
	t.Helper()
	var dst T
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPatch, "/", strings.NewReader(body))
	ok := decodeStrict(rec, req, &dst)
	if ok {
		return dst, http.StatusOK
	}
	return dst, rec.Code
}

func TestDecodeStrict_RefusesWhatTheRequestCannotTake(t *testing.T) {
	t.Parallel()
	// A rate edit naming rate_bps (a field a rate edit never changes) used to answer 200 and do nothing.
	if _, code := decodeBody[PatchGSTRateRequest](t, `{"rate_bps":1800,"reason":"rate moved to 18"}`); code != http.StatusBadRequest {
		t.Errorf("unknown key: status %d, want 400", code)
	}
	if _, code := decodeBody[PatchGSTRateRequest](t, `{"reason":"x"} {"reason":"y"}`); code != http.StatusBadRequest {
		t.Errorf("trailing value: status %d, want 400", code)
	}
	if _, code := decodeBody[PatchGSTRateRequest](t, `{"reason":"x"}garbage`); code != http.StatusBadRequest {
		t.Errorf("trailing garbage: status %d, want 400", code)
	}
	if got, code := decodeBody[PatchGSTRateRequest](t, `{"reason":"fine"}`); code != http.StatusOK || got.Reason != "fine" {
		t.Errorf("a valid body: %d %+v", code, got)
	}
}

func TestChanges_ARepeatedGroupOrSettingIsRefused(t *testing.T) {
	t.Parallel()
	if _, code := decodeBody[PatchSettingsRequest](t, `{"expected_version":1,"changes":{"expiry":{"stop_months_before":5},"expiry":{"stop_months_before":2}}}`); code != http.StatusBadRequest {
		t.Errorf("a duplicate group: status %d, want 400", code)
	}
	if _, code := decodeBody[PatchSettingsRequest](t, `{"expected_version":1,"changes":{"expiry":{"stop_months_before":5,"stop_months_before":2}}}`); code != http.StatusBadRequest {
		t.Errorf("a duplicate setting: status %d, want 400", code)
	}
	got, code := decodeBody[PatchSettingsRequest](t, `{"expected_version":1,"changes":{"expiry":{"stop_months_before":5},"credit":{"enabled":true}}}`)
	if code != http.StatusOK || len(got.Changes) != 2 || string(got.Changes["expiry"]["stop_months_before"]) != "5" {
		t.Errorf("a valid body: %d %+v", code, got)
	}
}
