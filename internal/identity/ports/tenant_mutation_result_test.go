package ports

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
)

func TestWriteTenantMutationResult_MapsOutcomes(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name     string
		err      error
		status   int
		wantCode string
	}{
		{"success", nil, http.StatusNoContent, ""},
		{"not found", command.ErrTenantNotFound, http.StatusNotFound, ErrCodeTenantNotFound},
		{"platform tenant", command.ErrPlatformTenantUndeletable, http.StatusUnprocessableEntity, ErrCodePlatformTenantUndeletable},
		{"unexpected", errors.New("boom"), http.StatusInternalServerError, ErrCodeInternalError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			writeTenantMutationResult(rec, log, httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/x", nil), tc.err)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if tc.wantCode == "" {
				return
			}
			var body ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Error != tc.wantCode {
				t.Errorf("code = %q, want %q", body.Error, tc.wantCode)
			}
		})
	}
}
