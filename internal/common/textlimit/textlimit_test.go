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
