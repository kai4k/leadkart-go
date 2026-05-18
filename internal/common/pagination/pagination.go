// Package pagination provides cursor (keyset) pagination primitives
// shared across modules per ADR 0038.
//
// Wire shape (Stripe / Google AIP-158 convergent):
//
//	GET /v1/users?cursor=<opaque>&page_size=50
//
//	200 OK
//	{
//	  "items":       [ ... ],
//	  "has_more":    true,
//	  "next_cursor": "eyJzIjoi..."
//	}
//
// The cursor is base64(JSON) of [Cursor] — opaque to clients,
// debuggable for us. Per-endpoint cursor shape may carry additional
// fields by embedding [Cursor] or by writing a peer encoder; clients
// must treat it as a blob and never introspect.
//
// Sort tuple is always (sort_value, id) for tiebreak stability — two
// rows sharing a sort_value would cause page-boundary skips or
// duplicates without the secondary id ordering (Markus Winand
// "Use the Index, Luke" canon).
//
// This package has zero project-internal imports so it can be used
// from any module without coupling.
package pagination

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Page is the canonical wire shape for a paginated list response.
//
// has_more / next_cursor are the only two pieces of metadata exposed.
// Total counts are intentionally NOT included per ADR 0038 (computing
// total under RLS is an O(n) scan that doesn't pay rent).
type Page[T any] struct {
	Items      []T    `json:"items"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// Cursor is the typed payload encoded into the opaque base64 token.
//
// SortValue is the primary sort column's value at the cursor row.
// ID is the tiebreaker (always UUIDv7 string in LeadKart).
//
// Per ADR 0038 the wire token is opaque — adding fields (direction,
// filter_hash, format_version) doesn't break clients because they
// treat the cursor as a blob.
type Cursor struct {
	SortValue time.Time `json:"s"`
	ID        string    `json:"i"`
}

// ErrInvalidCursor surfaces when a client supplies a cursor that
// can't be base64- or JSON-decoded. HTTP handlers should map to 400.
var ErrInvalidCursor = errors.New("pagination: invalid cursor")

// Encode serialises a Cursor as a URL-safe base64 string.
//
// Empty / zero cursor returns "" — used to signal "first page" or
// "this is the last page" depending on direction.
func Encode(c Cursor) string {
	if c.ID == "" && c.SortValue.IsZero() {
		return ""
	}
	raw, err := json.Marshal(c)
	if err != nil {
		// Defensive: a marshal error here would mean the struct
		// definition itself is invalid, not user input. Crash-fast
		// over silent corruption.
		panic(fmt.Sprintf("pagination: encode cursor: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// Decode parses a URL-safe base64 cursor back to its typed shape.
//
// Empty string returns the zero Cursor — useful for "no cursor
// supplied = caller wants the first page".
//
// Any decode error returns ErrInvalidCursor (wrapped) — HTTP handlers
// branch via errors.Is to return 400.
func Decode(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: base64: %w", ErrInvalidCursor, err)
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return Cursor{}, fmt.Errorf("%w: json: %w", ErrInvalidCursor, err)
	}
	return c, nil
}

// Page-size policy per ADR 0038. Constants exported so per-endpoint
// handlers don't redefine them inconsistently.
const (
	DefaultPageSize = 50
	MinPageSize     = 1
	MaxPageSize     = 200
)

// ClampPageSize coerces a caller-supplied page_size into the valid
// range. Zero / negative inputs return DefaultPageSize. Values above
// MaxPageSize are capped (no error — silently clamp matches Stripe /
// GitHub UX — clients can't get a 400 from "page size too large").
func ClampPageSize(supplied int) int {
	if supplied <= 0 {
		return DefaultPageSize
	}
	if supplied > MaxPageSize {
		return MaxPageSize
	}
	if supplied < MinPageSize {
		return MinPageSize
	}
	return supplied
}

// BuildPage assembles the wire response from a result slice that was
// fetched with `LIMIT page_size + 1` (the canonical "peek one extra"
// pattern from ADR 0038). The helper:
//
//   - drops the peek row if present
//   - emits has_more=true + next_cursor when the peek row existed
//   - emits has_more=false + empty next_cursor on the last page
//
// cursorFn extracts (sort_value, id) from a single item — endpoint-
// specific; usually `func(u UserDto) Cursor { return Cursor{u.JoinedAt, u.ID} }`.
//
// The slice argument may be mutated (peek row dropped); callers
// should treat it as consumed.
func BuildPage[T any](items []T, pageSize int, cursorFn func(T) Cursor) Page[T] {
	if len(items) > pageSize {
		last := items[pageSize-1]
		return Page[T]{
			Items:      items[:pageSize],
			HasMore:    true,
			NextCursor: Encode(cursorFn(last)),
		}
	}
	return Page[T]{
		Items:      items,
		HasMore:    false,
		NextCursor: "",
	}
}
