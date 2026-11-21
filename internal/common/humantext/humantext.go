// Package humantext is the ONE rule for text a person types that other people
// read as a name or a title: a company's display name, a person's name, a role
// name, a designation, a free-text reason that lands in an audit line.
//
// Such text is printed inside shared UI chrome (the operator's tenant-context
// banner, lists, audit lines), so it must not be able to steer the text around
// it. A value is refused when it holds any of
//
//   - C0 or C1 control characters: U+0000-U+001F and U+007F-U+009F (this
//     includes CR, LF and TAB: no field is multi-line);
//   - Unicode bidirectional controls: U+061C, U+200E, U+200F, U+202A-U+202E,
//     U+2066-U+2069 (a U+202E right-to-left override reorders the text after it);
//   - zero-width and invisible formatting characters used for spoofing:
//     U+200B-U+200D (zero-width space, non-joiner, JOINER), U+2060, U+FEFF.
//
// Everything else is accepted: letters of every script (Devanagari included),
// combining marks, emoji and U+00A0 (no-break space). The zero-width joiner is
// refused everywhere, so emoji sequences glued with it (a family emoji) are
// refused too; the simpler rule was chosen over "allowed between emoji only".
//
// The rule is for WRITES. A value already stored is never re-checked on a read,
// and a full-replace write that re-sends the stored value unchanged is accepted
// (see [CheckChanged]). Emails, slugs, codes and passwords have their own rules
// and do not use this package.
//
// The check iterates runes of a value the caller has already bounded (every
// caller checks its length first); it uses no regular expression.
package humantext

import (
	"errors"
	"strings"
)

// Code is the stable validation code a field answers with on the wire:
// `errors{<field>: ["invalid_characters"]}`.
const Code = "invalid_characters"

// ErrInvalidCharacters is the sentinel an [*Error] matches with [errors.Is].
var ErrInvalidCharacters = errors.New("invalid characters")

// Error names the wire field whose value was refused. It never carries the
// value (it may hold spoofing characters that must not reach a log or a message).
type Error struct {
	// Field is the request field name as the client sends it (snake_case).
	Field string
}

func (e *Error) Error() string {
	return e.Field + " must not contain control, direction-override or invisible formatting characters"
}

// Unwrap lets errors.Is(err, ErrInvalidCharacters) match.
func (e *Error) Unwrap() error { return ErrInvalidCharacters }

// Valid reports whether s holds none of the refused characters.
func Valid(s string) bool {
	return strings.IndexFunc(s, refused) < 0
}

// Check returns an [*Error] for field when s holds a refused character, else nil.
// Pass the TRIMMED, length-checked value.
func Check(field, s string) error {
	if Valid(s) {
		return nil
	}
	return &Error{Field: field}
}

// CheckChanged is [Check] for a full-replace write: a value equal to the one
// already stored is accepted even if that stored (legacy) value holds a refused
// character, so re-sending an untouched value never fails; only a changed value
// is checked.
func CheckChanged(field, stored, next string) error {
	if next == stored {
		return nil
	}
	return Check(field, next)
}

func refused(r rune) bool {
	switch {
	case r <= 0x1F, r >= 0x7F && r <= 0x9F: // C0, DEL and C1 controls
		return true
	case r == 0x061C, r == 0x200E, r == 0x200F: // ALM, LRM, RLM
		return true
	case r >= 0x202A && r <= 0x202E: // LRE RLE PDF LRO RLO
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI RLI FSI PDI
		return true
	case r >= 0x200B && r <= 0x200D: // ZWSP ZWNJ ZWJ
		return true
	case r == 0x2060, r == 0xFEFF: // word joiner, BOM / ZWNBSP
		return true
	}
	return false
}
