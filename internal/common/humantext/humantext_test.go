package humantext_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/humantext"
)

func TestValid_CharacterClasses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"ascii", "Acme Pharma Pvt. Ltd.", true},
		{"empty", "", true},
		{"devanagari", "अमित शर्मा", true},
		{"tamil and arabic", "தமிழ் عربى", true},
		{"combining marks", "José Zoë", true},
		{"emoji", "Acme 💊 Labs", true},
		{"no-break space allowed", "Acme\u00a0Labs", true},
		{"ideographic space allowed", "Acme\u3000Labs", true},
		{"apostrophe and hyphen", "D'Souza-Rao", true},
		{"NUL", "a\x00b", false},
		{"TAB", "a\tb", false},
		{"LF", "a\nb", false},
		{"CR", "a\rb", false},
		{"ESC", "a\x1bb", false},
		{"US U+001F", "a\x1fb", false},
		{"space U+0020 allowed", "a b", true},
		{"DEL U+007F", "a\x7fb", false},
		{"C1 U+0080", "a\u0080b", false},
		{"C1 NEL U+0085", "a\u0085b", false},
		{"C1 U+009F", "a\u009fb", false},
		{"U+00A0 is NOT C1", "a\u00a0b", true},
		{"ALM U+061C", "a\u061cb", false},
		{"LRM U+200E", "a\u200eb", false},
		{"RLM U+200F", "a\u200fb", false},
		{"LRE U+202A", "a\u202ab", false},
		{"RLE U+202B", "a\u202bb", false},
		{"PDF U+202C", "a\u202cb", false},
		{"LRO U+202D", "a\u202db", false},
		{"RLO U+202E", "Acme\u202eLtd", false},
		{"LRI U+2066", "a\u2066b", false},
		{"RLI U+2067", "a\u2067b", false},
		{"FSI U+2068", "a\u2068b", false},
		{"PDI U+2069", "a\u2069b", false},
		{"ZWSP U+200B", "a\u200bb", false},
		{"ZWNJ U+200C between letters is a joiner inside a word", "a\u200cb", true},
		{"ZWJ U+200D between letters is a joiner inside a word", "a\u200db", true},
		{"ZWJ emoji sequence refused", "\U0001F468\u200d\U0001F469\u200d\U0001F467", false},
		{"word joiner U+2060", "a\u2060b", false},
		{"BOM U+FEFF", "\ufeffAcme", false},
		{"neighbours just outside the ranges", "\u200a\u2010\u202f\u205f\u206a\uFFF8\uFFFD\u00AC\u00AE\u034E\u0350\u1161\u3163\u3165\uFF9F\uFFA1\U000E0080", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := humantext.Valid(c.in); got != c.want {
				t.Errorf("Valid(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestCheck_NamesTheFieldAndMatchesTheSentinel(t *testing.T) {
	t.Parallel()
	if err := humantext.Check("display_name", "Acme"); err != nil {
		t.Fatalf("Check(valid) = %v", err)
	}
	err := humantext.Check("display_name", "Acme\u202e")
	var he *humantext.Error
	if !errors.As(err, &he) || he.Field != "display_name" {
		t.Fatalf("Check = %v, want *Error for display_name", err)
	}
	if !errors.Is(err, humantext.ErrInvalidCharacters) {
		t.Errorf("error does not match ErrInvalidCharacters")
	}
}

func TestCheckChanged_AnUnchangedLegacyValueIsAccepted(t *testing.T) {
	t.Parallel()
	const legacy = "Old\u202eName"
	if err := humantext.CheckChanged("display_name", legacy, legacy); err != nil {
		t.Errorf("an unchanged stored value must be accepted: %v", err)
	}
	if err := humantext.CheckChanged("display_name", legacy, legacy+"x"); err == nil {
		t.Error("a changed value that still holds the character must be refused")
	}
	if err := humantext.CheckChanged("display_name", legacy, "Clean"); err != nil {
		t.Errorf("a changed clean value must be accepted: %v", err)
	}
}

// Real words with a joiner inside them. Built from explicit code points so this
// file carries no invisible character; the visible form is in each comment.
func TestValid_JoinersInsideARealWordAreAllowed(t *testing.T) {
	t.Parallel()
	const (
		zwj  = "\u200d"
		zwnj = "\u200c"
	)
	cases := []struct{ name, in string }{
		// क्ष with half-form ka: U+0915 U+094D ZWJ U+0937 (Hindi)
		{"Devanagari half-form ka + ZWJ", "क्" + zwj + "ष"},
		// क्ष with the conjunct broken: U+0915 U+094D ZWNJ U+0937 (Hindi)
		{"Devanagari ZWNJ breaks the conjunct", "क्" + zwnj + "ष"},
		// ऱ्या (eyelash ra, Marathi): U+0930 U+094D ZWJ U+092F U+093E
		{"Marathi eyelash ra + ZWJ", "र्" + zwj + "या"},
		// Marathi surname-like word with a ZWNJ between two syllables, in a full name
		{"Devanagari full name with a ZWNJ", "अमित क्" + zwnj + "षत्रिय"},
		// می\u200cخواهم (Persian, "I want"): U+0645 U+06CC ZWNJ U+062E U+0648 U+0627 U+0647 U+0645
		{"Persian ZWNJ", "می" + zwnj + "خواهم"},
		// کتاب\u200cها (Urdu/Persian, "books"): U+06A9 U+062A U+0627 U+0628 ZWNJ U+0647 U+0627
		{"Urdu ZWNJ before a plural suffix", "کتاب" + zwnj + "ها"},
		{"previously accepted scripts still pass", "अमित தமிழ் عربى José"},
		{"NBSP and ideographic space still pass", "Acme Labs　Pvt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if !humantext.Valid(c.in) {
				t.Errorf("Valid(%+q) = false, want true", c.in)
			}
		})
	}
}

func TestValid_JoinersOutsideAWordAreRefused(t *testing.T) {
	t.Parallel()
	const (
		zwj  = "\u200d"
		zwnj = "\u200c"
		ka   = "क"
		vir  = "्"
	)
	cases := []struct{ name, in string }{
		{"ZWJ at the start", zwj + ka},
		{"ZWNJ at the start", zwnj + ka},
		{"ZWJ at the end", ka + vir + zwj},
		{"ZWNJ at the end", ka + zwnj},
		{"only a ZWJ", zwj},
		{"ZWJ after a space", "a " + zwj + "b"},
		{"ZWJ before a space", "a" + zwj + " b"},
		{"ZWNJ next to a space", "a" + zwnj + " b"},
		{"ZWJ after a digit", "1" + zwj + "a"},
		{"ZWNJ before a digit", "a" + zwnj + "1"},
		{"ZWJ next to punctuation", "a" + zwj + "-b"},
		{"ZWJ after a symbol", "$" + zwj + "a"},
		{"two ZWJ in a row", ka + vir + zwj + zwj + ka},
		{"ZWJ then ZWNJ", ka + vir + zwj + zwnj + ka},
		{"ZWNJ then ZWSP", "a" + zwnj + "\u200b" + "b"},
		{"joiner next to a control character", "a" + zwj + "\x00" + "b"},
		{"joiner next to a bidi override", "a" + zwj + "\u202e" + "b"},
		{"ZWJ between two emoji", "👨" + zwj + "👩"},
		{"emoji family sequence", "👨" + zwj + "👩" + zwj + "👧"},
		{"ZWJ between an emoji and a letter", "👨" + zwj + "a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if humantext.Valid(c.in) {
				t.Errorf("Valid(%+q) = true, want false", c.in)
			}
		})
	}
}

func TestValid_InvisibleAndLineBreakingCharactersAreRefused(t *testing.T) {
	t.Parallel()
	refused := []struct {
		name string
		r    rune
	}{
		{"LINE SEPARATOR", 0x2028}, {"PARAGRAPH SEPARATOR", 0x2029},
		{"SOFT HYPHEN", 0x00AD}, {"COMBINING GRAPHEME JOINER", 0x034F},
		{"HANGUL CHOSEONG FILLER", 0x115F}, {"HANGUL JUNGSEONG FILLER", 0x1160},
		{"HANGUL FILLER", 0x3164}, {"HALFWIDTH HANGUL FILLER", 0xFFA0},
		{"MONGOLIAN VOWEL SEPARATOR", 0x180E},
		{"WORD JOINER", 0x2060}, {"FUNCTION APPLICATION", 0x2061}, {"INVISIBLE TIMES", 0x2062},
		{"INVISIBLE SEPARATOR", 0x2063}, {"INVISIBLE PLUS", 0x2064},
		{"INTERLINEAR ANNOTATION ANCHOR", 0xFFF9}, {"INTERLINEAR ANNOTATION SEPARATOR", 0xFFFA},
		{"INTERLINEAR ANNOTATION TERMINATOR", 0xFFFB}, {"OBJECT REPLACEMENT CHARACTER", 0xFFFC},
		{"TAG first", 0xE0000}, {"TAG LATIN SMALL A", 0xE0061}, {"TAG last (cancel tag)", 0xE007F},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for _, in := range []string{"a" + string(c.r) + "b", string(c.r) + "ab", "ab" + string(c.r)} {
				if humantext.Valid(in) {
					t.Errorf("Valid(%+q) = true, want false", in)
				}
			}
		})
	}
}
