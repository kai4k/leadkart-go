package ports

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// decodeStrict reads a request body that must be exactly one JSON object whose keys
// the request type knows. An unknown key (a field this contract retired, such as
// gst_rate_bps, or a misspelt one) or data after the object is 400 invalid_body and
// nothing is changed: it must never answer success and do nothing (review S4). Writes
// the response and returns false on error.
func decodeStrict[T any](w http.ResponseWriter, r *http.Request, dst *T) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON, or names a field this request does not take")
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body must be a single JSON object")
		return false
	}
	return true
}
