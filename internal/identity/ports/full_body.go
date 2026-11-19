package ports

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
)

// decodeFullBody decodes a FULL-REPLACE request body: every json-tagged field of
// T (and of nested struct fields) must be present as a key and not null. The
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
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
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
		writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
		return false
	}
	return true
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
		raw, present := keys[name]
		if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			missing = append(missing, prefix+name)
			continue
		}
		if f.Type.Kind() == reflect.Struct {
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(raw, &nested); err != nil || nested == nil {
				missing = append(missing, prefix+name) // not an object: unusable
				continue
			}
			missing = append(missing, missingRequiredKeys(f.Type, nested, prefix+name+".")...)
		}
	}
	slices.Sort(missing)
	return missing
}
