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
	if decodeStrict(rec, req, &dst) {
		return dst, http.StatusOK
	}
	return dst, rec.Code
}

// Review S4: the retired gst_rate_bps on a product body answered 204 and did nothing.
func TestDecodeStrict_RefusesRetiredAndUnknownFields(t *testing.T) {
	t.Parallel()
	if _, code := decodeBody[UpdateProductRequest](t, `{"gst_rate_bps":1800}`); code != http.StatusBadRequest {
		t.Errorf("retired gst_rate_bps on update: %d, want 400", code)
	}
	if _, code := decodeBody[CreateProductRequest](t, `{"sku":"A","gst_rate_bps":1800}`); code != http.StatusBadRequest {
		t.Errorf("retired gst_rate_bps on create: %d, want 400", code)
	}
	if _, code := decodeBody[AddBatchRequest](t, `{"expiry_date":"2027-03-31T00:00:00Z"}`); code != http.StatusBadRequest {
		t.Errorf("retired expiry_date on a batch: %d, want 400", code)
	}
	if _, code := decodeBody[LogMovementRequest](t, `{"type":"inbound","quantity":1}{"x":1}`); code != http.StatusBadRequest {
		t.Errorf("trailing value: %d, want 400", code)
	}
	if got, code := decodeBody[UpdateProductRequest](t, `{"name":"X"}`); code != http.StatusOK || got.Name == nil {
		t.Errorf("a valid body: %d", code)
	}
}
