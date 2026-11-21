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
		{"ZWNJ U+200C", "a\u200cb", false},
		{"ZWJ U+200D", "a\u200db", false},
		{"ZWJ emoji sequence refused", "\U0001F468\u200d\U0001F469\u200d\U0001F467", false},
		{"word joiner U+2060", "a\u2060b", false},
		{"BOM U+FEFF", "\ufeffAcme", false},
		{"neighbours just outside the ranges", "\u200a\u2010\u2029\u202f\u205f\u2061\u206a", true},
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
