package textlimit_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/textlimit"
)

func TestBytesAndChars(t *testing.T) {
	t.Parallel()
	multi := strings.Repeat("é", 5) // 5 characters, 10 bytes

	if err := textlimit.Bytes("notes", multi, 10); err != nil {
		t.Fatalf("10 bytes within 10: %v", err)
	}
	if err := textlimit.Bytes("notes", multi, 9); err == nil {
		t.Fatal("10 bytes over a 9-byte limit must be refused")
	}
	if err := textlimit.Chars("notes", multi, 5); err != nil {
		t.Fatalf("5 characters within 5: %v", err)
	}
	if err := textlimit.Chars("notes", multi, 4); err == nil {
		t.Fatal("5 characters over a 4-character limit must be refused")
	}
	if err := textlimit.Chars("notes", strings.Repeat("a", 101), 100); err == nil {
		t.Fatal("101 ASCII characters over 100 must be refused")
	}
}

func TestErrorCodeAndSentinel(t *testing.T) {
	t.Parallel()
	err := textlimit.Chars("external_reference", strings.Repeat("x", 101), 100)
	var te *textlimit.Error
	if !errors.As(err, &te) {
		t.Fatalf("want *textlimit.Error, got %v", err)
	}
	if te.Code() != "external_reference_too_long" {
		t.Fatalf("code = %q", te.Code())
	}
	if !errors.Is(err, textlimit.ErrTooLong) {
		t.Fatal("errors.Is(ErrTooLong) must hold")
	}
	if strings.Contains(err.Error(), "xxx") {
		t.Fatal("the error must not echo the value")
	}
}

// PostgreSQL cannot store U+0000 in a text or jsonb value (SQLSTATE 22021 /
// 22P05), so a value holding it is refused here, with the field's own code,
// before it reaches an insert and becomes a 500.
func TestNULIsRefusedWithTheFieldsCode(t *testing.T) {
	t.Parallel()
	for name, check := range map[string]func(string) error{
		"Bytes": func(s string) error { return textlimit.Bytes("notes", s, 2000) },
		"Chars": func(s string) error { return textlimit.Chars("notes", s, 2000) },
		"NoNUL": func(s string) error { return textlimit.NoNUL("notes", s) },
	} {
		err := check("paid\x00in cash")
		te, ok := textlimit.Of(err)
		if !ok || te.Code() != "notes_invalid_characters" {
			t.Fatalf("%s: want notes_invalid_characters, got %v", name, err)
		}
		if !errors.Is(err, textlimit.ErrInvalidCharacters) || !errors.Is(err, textlimit.ErrInvalid) {
			t.Fatalf("%s: a NUL refusal must match ErrInvalidCharacters and ErrInvalid", name)
		}
		if errors.Is(err, textlimit.ErrTooLong) {
			t.Fatalf("%s: a NUL refusal is not a length refusal", name)
		}
		if strings.Contains(err.Error(), "paid") {
			t.Fatalf("%s: the error must not echo the value", name)
		}
		// Multi-line notes stay valid: only U+0000 is refused.
		if err := check("line one\nline two\ttabbed, हिन्दी"); err != nil {
			t.Fatalf("%s: newline, tab and Devanagari must be accepted: %v", name, err)
		}
	}
	// A length refusal still matches the common sentinel, and only its own kind.
	long := textlimit.Chars("notes", strings.Repeat("a", 11), 10)
	if !errors.Is(long, textlimit.ErrInvalid) || !errors.Is(long, textlimit.ErrTooLong) || errors.Is(long, textlimit.ErrInvalidCharacters) {
		t.Fatalf("too long must match ErrInvalid and ErrTooLong only: %v", long)
	}
}
