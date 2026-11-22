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
package textlimit

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// ErrTooLong is the sentinel every [*Error] matches with errors.Is.
var ErrTooLong = errors.New("textlimit: too long")

// Unit is what a limit counts.
type Unit string

// The two units.
const (
	UnitBytes Unit = "bytes"
	UnitChars Unit = "characters"
)

// Error is a value over its limit.
type Error struct {
	Field string
	Max   int
	Unit  Unit
}

// Error describes the refusal; it never carries the value.
func (e *Error) Error() string {
	return fmt.Sprintf("%s too long (max %d %s)", e.Field, e.Max, e.Unit)
}

// Is makes errors.Is(err, ErrTooLong) true.
func (e *Error) Is(target error) bool { return target == ErrTooLong }

// Code is the stable wire code: "<field>_too_long".
func (e *Error) Code() string { return e.Field + "_too_long" }

// Bytes returns an [*Error] when s is longer than max UTF-8 bytes, else nil.
func Bytes(field, s string, max int) error {
	if len(s) > max {
		return &Error{Field: field, Max: max, Unit: UnitBytes}
	}
	return nil
}

// Chars returns an [*Error] when s is longer than max characters (runes, the
// unit of PostgreSQL's length()), else nil.
func Chars(field, s string, max int) error {
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
