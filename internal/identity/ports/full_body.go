package ports

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
)

// decodeFullBody decodes a FULL-REPLACE request body: every json-tagged field of
// T (and of nested struct fields) must be present as a key and not null.
// Two struct tags relax that per field: `optional:"true"` (the key may be
// absent or null) and `nullable:"true"` (the key must be present but may be
// null, e.g. "null clears the parent"). The
// handlers behind these bodies write exactly what they receive, so a field the
// client left out would silently clear the stored value; the spec marks every
// such field `required`, and this makes the handler agree. A missing field is a
// 400 `invalid_body` whose `errors` map names each missing field ("required");
// a body that is not a JSON object is the same 400 as before. Unknown keys are
// ignored, as they always were. Writes the response and returns false on error.
func decodeFullBody[T any](w http.ResponseWriter, r *http.Request, dst *T) bool {
	// The body is already size-bounded by httpmw.BodyLimit (MaxBytesReader) upstream;
	// like every other handler it is decoded with json.NewDecoder, here once into raw
	// bytes so the keys can be checked before the typed decode.
	var data json.RawMessage
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&data); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
		return false
	}
	// Exactly one JSON value: anything after it is not part of this request.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body must be a single JSON object")
		return false
	}
	// encoding/json matches keys case-insensitively and the last duplicate wins,
	// so {"granted":[...],"Granted":null} would pass the presence check on one
	// spelling and decode the other. A repeated key is ambiguous: refuse it.
	if dup := duplicateKey(data); dup != "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body repeats the key "+dup+" (keys are matched case-insensitively)")
		return false
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil || keys == nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
		return false
	}
	if missing := missingRequiredKeys(reflect.TypeFor[T](), keys, ""); len(missing) > 0 {
		fields := make(map[string][]string, len(missing))
		for _, m := range missing {
			fields[m] = []string{"required"}
		}
		writeJSON(w, http.StatusBadRequest, ErrorResponse{
			Type:    problemType(ErrCodeInvalidBody),
			Title:   http.StatusText(http.StatusBadRequest),
			Status:  http.StatusBadRequest,
			Detail:  "required fields are missing: " + strings.Join(missing, ", "),
			Error:   ErrCodeInvalidBody,
			Message: "required fields are missing: " + strings.Join(missing, ", "),
			Errors:  fields,
		})
		return false
	}
	if err := json.Unmarshal(data, dst); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			msg := "field " + typeErr.Field + " must be of type " + typeErr.Type.String()
			writeJSON(w, http.StatusBadRequest, ErrorResponse{
				Type: problemType(ErrCodeInvalidBody), Title: http.StatusText(http.StatusBadRequest), Status: http.StatusBadRequest,
				Detail: msg, Error: ErrCodeInvalidBody, Message: msg,
				Errors: map[string][]string{typeErr.Field: {"invalid_type"}},
			})
			return false
		}
		writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
		return false
	}
	return true
}

// duplicateKey returns the first object key that repeats, compared
// case-insensitively, at any depth of the JSON value, or "".
func duplicateKey(data []byte) string {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func() string
	walk = func() string {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		switch d, _ := tok.(json.Delim); d {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return ""
				}
				key, _ := kt.(string)
				folded := strings.ToLower(key)
				if seen[folded] {
					return key
				}
				seen[folded] = true
				if dup := walk(); dup != "" {
					return dup
				}
			}
			_, _ = dec.Token() // the closing brace
		case '[':
			for dec.More() {
				if dup := walk(); dup != "" {
					return dup
				}
			}
			_, _ = dec.Token()
		}
		return ""
	}
	return walk()
}

// missingRequiredKeys lists, sorted, the dotted JSON paths of every json-tagged
// field of struct type t that is absent or null in keys, descending into nested
// struct fields.
func missingRequiredKeys(t reflect.Type, keys map[string]json.RawMessage, prefix string) []string {
	var missing []string
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" || !f.IsExported() {
			continue
		}
		if f.Tag.Get("optional") == "true" {
			continue // may be absent or null (create-time fields with a default)
		}
		raw, present := keys[name]
		isNull := bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
		if !present || (isNull && f.Tag.Get("nullable") != "true") {
			missing = append(missing, prefix+name)
			continue
		}
		if f.Type.Kind() == reflect.Struct {
			if isNull {
				continue // already reported above for a non-nullable field
			}
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(raw, &nested); err != nil || nested == nil {
				continue // not an object: the typed decode reports it as a type error
			}
			missing = append(missing, missingRequiredKeys(f.Type, nested, prefix+name+".")...)
		}
	}
	slices.Sort(missing)
	return missing
}
