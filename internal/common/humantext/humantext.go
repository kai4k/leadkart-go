// Package humantext is the ONE rule for text a person types that other people
// read as a name or a title: a company's display name, a person's name, a role
// name, a designation, a free-text reason that lands in an audit line.
//
// Such text is printed inside shared UI chrome (the operator's tenant-context
// banner, lists, audit lines), so it must not be able to steer the text around
// it or hide in it. A value is refused when it holds any of
//
//   - C0 or C1 control characters: U+0000-U+001F and U+007F-U+009F (this
//     includes CR, LF and TAB: no field is multi-line);
//   - Unicode bidirectional controls: U+061C, U+200E, U+200F, U+202A-U+202E,
//     U+2066-U+2069 (a U+202E right-to-left override reorders the text after it);
//   - line and paragraph separators: U+2028, U+2029 (they break a one-line name);
//   - invisible or zero-width characters: U+00AD (soft hyphen), U+034F (combining
//     grapheme joiner), U+115F, U+1160, U+3164, U+FFA0 (Hangul fillers), U+180E
//     (Mongolian vowel separator), U+200B (zero-width space), U+2060-U+2064 (word
//     joiner, function application, invisible times, separator, plus), U+FEFF
//     (BOM / zero-width no-break space);
//   - U+FFF9-U+FFFC (interlinear annotation anchor, separator, terminator, object
//     replacement) and the TAG block U+E0000-U+E007F;
//   - U+200C (zero-width non-joiner) and U+200D (zero-width joiner) EXCEPT as
//     below.
//
// The joiners are legitimate INSIDE a word of an Indic script (Devanagari,
// Bengali, Gujarati, Gurmukhi, Kannada, Malayalam, Tamil, Telugu, Sinhala, ...) or
// an Arabic-script language (Persian, Urdu): they choose a conjunct form or break
// a ligature. U+200C and U+200D are therefore accepted when the rune immediately
// before AND the rune immediately after are each a letter or a combining mark
// (Unicode general category L* or M*), that is strictly inside a word. The one
// exception is the legacy Malayalam chillu, typed consonant + virama (U+0D4D) +
// ZWJ: its joiner ENDS the word, so U+200D is also accepted when the rune before
// it is exactly U+0D4D and the next rune is whitespace or the end of the value.
// U+200C never has this exception, and a joiner after any other virama (Devanagari,
// Tamil, ...) at a word end stays refused. Apart from that exception they are
// refused at the start or the end of the value, next to whitespace, a digit,
// punctuation, a symbol, another joiner or any other refused character. Emoji
// sequences glued with a joiner (a family emoji) stay refused: emoji are symbols,
// not letters.
//
// Everything else is accepted: letters of every script, combining marks, emoji,
// U+00A0 (no-break space) and U+3000 (ideographic space).
//
// The rule is for WRITES. A value already stored is never re-checked on a read,
// and a full-replace write that re-sends the stored value unchanged is accepted
// (see [CheckChanged]). Emails, slugs, codes and passwords have their own rules
// and do not use this package.
//
// The check iterates the runes of a value the caller has already bounded (every
// caller checks its length first) once; it uses no regular expression.
package humantext

import (
	"errors"
	"unicode"
	"unicode/utf8"
)

// Wire codes of a field error, stable. [Code] marks a refused character; a name
// that is blank or too long is not a "character" problem but belongs to the same
// field rule, so the aggregates report it through the same type and the HTTP layer
// writes one shape: `errors{<field>: [<code>]}`.
const (
	// Code is the validation code of a value holding a refused character.
	Code = "invalid_characters"
	// CodeRequired is the code of a blank value.
	CodeRequired = "required"
	// CodeTooLong is the code of a value over its length limit.
	CodeTooLong = "too_long"
)

// ErrInvalidCharacters is the sentinel an [*Error] with code [Code] matches with
// [errors.Is].
var ErrInvalidCharacters = errors.New("invalid characters")

// Error names the wire field whose value was refused and why. It never carries the
// value (it may hold spoofing characters that must not reach a log or a message).
type Error struct {
	// Field is the request field name as the client sends it (snake_case).
	Field string
	// Reason is the wire code: [Code] (also the meaning of ""), [CodeRequired] or
	// [CodeTooLong].
	Reason string
}

// Required is the error for a blank value of field.
func Required(field string) *Error { return &Error{Field: field, Reason: CodeRequired} }

// TooLong is the error for a value of field over its length limit.
func TooLong(field string) *Error { return &Error{Field: field, Reason: CodeTooLong} }

// WireCode is the code to put under errors.<Field>.
func (e *Error) WireCode() string {
	if e.Reason == "" {
		return Code
	}
	return e.Reason
}

func (e *Error) Error() string {
	switch e.WireCode() {
	case CodeRequired:
		return e.Field + " is required"
	case CodeTooLong:
		return e.Field + " is too long"
	}
	return e.Field + " must not contain control, direction-override or invisible formatting characters"
}

// Is makes errors.Is(err, ErrInvalidCharacters) match only a refused-character error.
func (e *Error) Is(target error) bool {
	return target == ErrInvalidCharacters && e.WireCode() == Code
}

// Valid reports whether s holds none of the refused characters.
func Valid(s string) bool {
	var prev rune // the rune before the current one; 0 at the start
	for i, r := range s {
		switch {
		case r == zwnj || r == zwj:
			next, size := utf8.DecodeRuneInString(s[i+utf8.RuneLen(r):])
			if !insideWord(prev, next) && !(r == zwj && malayalamChillu(prev, next, size)) {
				return false
			}
		case refused(r):
			return false
		}
		prev = r
	}
	return true
}

const (
	zwnj = '\u200c' // ZERO WIDTH NON-JOINER
	zwj  = '\u200d' // ZERO WIDTH JOINER

	malayalamVirama = '\u0d4d' // MALAYALAM SIGN VIRAMA
)

// insideWord reports whether a joiner between before and after sits strictly
// inside a word: both neighbours exist (the zero rune means "no neighbour";
// utf8.RuneError means the end of the string or a bad byte) and each is a letter
// or a combining mark.
func insideWord(before, after rune) bool {
	return wordRune(before) && wordRune(after)
}

// malayalamChillu reports a legacy chillu spelling: consonant + virama (U+0D4D) +
// ZWJ, where the joiner ends the word, so it has no letter after it. before is the
// rune before the ZWJ; after and size are the rune after it and its width (size 0
// means the end of the string).
func malayalamChillu(before, after rune, size int) bool {
	return before == malayalamVirama && (size == 0 || unicode.IsSpace(after))
}

func wordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsMark(r)
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

// refused reports a character refused wherever it stands. The joiners U+200C and
// U+200D are decided by [insideWord] and are not listed here.
func refused(r rune) bool {
	switch {
	case r <= 0x1F, r >= 0x7F && r <= 0x9F: // C0, DEL and C1 controls
		return true
	case r == 0x00AD, r == 0x034F: // soft hyphen, combining grapheme joiner
		return true
	case r == 0x061C, r == 0x200E, r == 0x200F: // ALM, LRM, RLM
		return true
	case r == 0x115F, r == 0x1160, r == 0x3164, r == 0xFFA0: // Hangul fillers
		return true
	case r == 0x180E: // Mongolian vowel separator
		return true
	case r == 0x200B: // zero-width space
		return true
	case r == 0x2028, r == 0x2029: // line and paragraph separators
		return true
	case r >= 0x202A && r <= 0x202E: // LRE RLE PDF LRO RLO
		return true
	case r >= 0x2060 && r <= 0x2064: // word joiner, invisible operators
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI RLI FSI PDI
		return true
	case r == 0xFEFF: // BOM / zero-width no-break space
		return true
	case r >= 0xFFF9 && r <= 0xFFFC: // interlinear annotation, object replacement
		return true
	case r >= 0xE0000 && r <= 0xE007F: // TAG block
		return true
	}
	return false
}
