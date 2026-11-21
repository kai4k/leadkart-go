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
// (Unicode general category L* or M*), that is strictly inside a word. They are
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
	var prev rune // the rune before the current one; 0 at the start
	for i, r := range s {
		switch {
		case r == zwnj || r == zwj:
			next, _ := utf8.DecodeRuneInString(s[i+utf8.RuneLen(r):])
			if !insideWord(prev, next) {
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
)

// insideWord reports whether a joiner between before and after sits strictly
// inside a word: both neighbours exist (the zero rune means "no neighbour";
// utf8.RuneError means the end of the string or a bad byte) and each is a letter
// or a combining mark.
func insideWord(before, after rune) bool {
	return wordRune(before) && wordRune(after)
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
