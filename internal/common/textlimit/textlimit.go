// Package textlimit is the one length check for free text a caller sends to a
// commerce write path (CRM, Orders, Inventory, Dispatch), and the one wire code
// it answers with.
//
// A text column with a database limit must be refused by the domain with a
// stable, field-named code BEFORE the insert; otherwise the CHECK violation
// surfaces as a 500. The unit follows what the limit measures: PostgreSQL's
// length() counts characters, so a limit that mirrors such a CHECK uses
// [Chars]; a limit the domain has always measured with len() stays in
// [Bytes] (a byte limit is the stricter of the two, so it never lets through
// what a character CHECK would refuse).
//
// The code is "<field>_too_long", for example "reason_too_long" or
// "external_reference_too_long". It is stable: the web switches on it.
//
// The same check refuses U+0000. PostgreSQL cannot store it in a text or jsonb
// value (SQLSTATE 22021 / 22P05), so a value holding it would also surface as a
// 500. Its code is "<field>_invalid_characters". Nothing else is refused here:
// notes and reasons are multi-line, so newline and tab are valid (a one-line
// NAME uses common/humantext instead).
package textlimit

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrInvalid is the sentinel EVERY [*Error] matches with errors.Is: a transport
// layer tests it once, then answers with [Error.Code].
var ErrInvalid = errors.New("textlimit: invalid")

// ErrTooLong is the sentinel a value over its limit matches with errors.Is.
var ErrTooLong = errors.New("textlimit: too long")

// ErrInvalidCharacters is the sentinel a value holding U+0000 matches with
// errors.Is.
var ErrInvalidCharacters = errors.New("textlimit: invalid characters")

// Unit is what a limit counts.
type Unit string

// The two units.
const (
	UnitBytes Unit = "bytes"
	UnitChars Unit = "characters"
)

// Error is a refused value: over its limit, or (NUL true) holding U+0000.
type Error struct {
	Field string
	Max   int
	Unit  Unit
	NUL   bool
}

// Error describes the refusal; it never carries the value.
func (e *Error) Error() string {
	if e.NUL {
		return e.Field + " must not contain the NUL character (U+0000)"
	}
	return fmt.Sprintf("%s too long (max %d %s)", e.Field, e.Max, e.Unit)
}

// Is makes errors.Is(err, ErrInvalid) true for every refusal, and
// errors.Is(err, ErrTooLong) / errors.Is(err, ErrInvalidCharacters) true for its
// own kind only.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrInvalid:
		return true
	case ErrTooLong:
		return !e.NUL
	case ErrInvalidCharacters:
		return e.NUL
	}
	return false
}

// Code is the stable wire code: "<field>_too_long" or
// "<field>_invalid_characters".
func (e *Error) Code() string {
	if e.NUL {
		return e.Field + "_invalid_characters"
	}
	return e.Field + "_too_long"
}

// NoNUL returns an [*Error] when s holds U+0000, else nil. It is the check for a
// text input that has no length limit; [Bytes] and [Chars] include it.
func NoNUL(field, s string) error {
	if strings.IndexByte(s, 0) >= 0 {
		return &Error{Field: field, NUL: true}
	}
	return nil
}

// Bytes returns an [*Error] when s holds U+0000 or is longer than max UTF-8
// bytes, else nil.
func Bytes(field, s string, max int) error {
	if err := NoNUL(field, s); err != nil {
		return err
	}
	if len(s) > max {
		return &Error{Field: field, Max: max, Unit: UnitBytes}
	}
	return nil
}

// Chars returns an [*Error] when s holds U+0000 or is longer than max characters
// (runes, the unit of PostgreSQL's length()), else nil.
func Chars(field, s string, max int) error {
	if err := NoNUL(field, s); err != nil {
		return err
	}
	if len(s) > max && utf8.RuneCountInString(s) > max { // len(s) >= runes: skip the count when it cannot exceed
		return &Error{Field: field, Max: max, Unit: UnitChars}
	}
	return nil
}

// Of returns the [*Error] inside err, if there is one, so a transport layer
// can answer 422 with its Code.
func Of(err error) (*Error, bool) {
	var te *Error
	if errors.As(err, &te) {
		return te, true
	}
	return nil, false
}
