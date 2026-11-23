package ports

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// decodeStrict reads a request body that must be exactly one JSON object whose
// keys the request type knows: an unknown key (a misspelt or unsupported field),
// a second JSON value after the object, or invalid JSON is a 400 invalid_body and
// nothing is changed. A rate edit that names a field it cannot change must not
// answer 200 and do nothing (review M4). Writes the response and returns false on
// error.
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

// Changes is the group-then-setting body of a settings PATCH:
// {"expiry": {"stop_months_before": 2}}. A group or a setting named twice is
// refused: the standard decoder keeps the last one and silently drops the first
// (review M4).
type Changes map[string]map[string]json.RawMessage

// UnmarshalJSON decodes the two levels and refuses a repeated key at either.
func (c *Changes) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return errors.New("changes must be an object of groups")
	}
	out := Changes{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		group, _ := keyTok.(string)
		if _, dup := out[group]; dup {
			return fmt.Errorf("group %q is named twice", group)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		settings, err := decodeNoDuplicates(raw)
		if err != nil {
			return fmt.Errorf("group %q: %w", group, err)
		}
		out[group] = settings
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	*c = out
	return nil
}

// decodeNoDuplicates decodes one JSON object of raw values, refusing a repeated key.
func decodeNoDuplicates(raw json.RawMessage) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("a group must be an object of settings")
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, _ := keyTok.(string)
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("setting %q is named twice", name)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		out[name] = v
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}
