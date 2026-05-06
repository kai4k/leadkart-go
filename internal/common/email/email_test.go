package email_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/errs"
)

func TestNew_AcceptsValid(t *testing.T) {
	t.Parallel()
	cases := []string{
		"alice@example.com",
		"user.name+tag@example.co.in",
		"a@b.io",
		"firstname.lastname@subdomain.example.org",
	}
	for _, raw := range cases {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			e, err := email.New(raw)
			if err != nil {
				t.Fatalf("New(%q): unexpected error %v", raw, err)
			}
			if e.IsZero() {
				t.Fatalf("New(%q): zero value returned", raw)
			}
		})
	}
}

func TestNew_NormalisesToLowercase(t *testing.T) {
	t.Parallel()
	e, err := email.New("Alice@Example.COM")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.String() != "alice@example.com" {
		t.Fatalf("normalisation: got %q, want %q", e.String(), "alice@example.com")
	}
}

func TestNew_TrimsSurroundingWhitespace(t *testing.T) {
	t.Parallel()
	e, err := email.New("  alice@example.com\t\n")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if e.String() != "alice@example.com" {
		t.Fatalf("trim: got %q, want %q", e.String(), "alice@example.com")
	}
}

func TestNew_RejectsInvalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"no @", "alice.example.com"},
		{"no local part", "@example.com"},
		{"no domain", "alice@"},
		{"no tld", "alice@example"},
		{"spaces inside", "ali ce@example.com"},
		{"too long", string(make([]byte, 255)) + "@example.com"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := email.New(tc.raw)
			if err == nil {
				t.Fatalf("New(%q): expected error, got nil", tc.raw)
			}
			if got := errs.KindOf(err); got != errs.KindInvalidInput {
				t.Fatalf("New(%q): KindOf = %v, want KindInvalidInput", tc.raw, got)
			}
		})
	}
}

func TestNew_RejectsTooLong(t *testing.T) {
	t.Parallel()
	// RFC 5321 caps at 254 chars; one over.
	overlong := strings.Repeat("a", 245) + "@example.com" // 245 + 12 = 257, over 254 limit
	_, err := email.New(overlong)
	if err == nil {
		t.Fatal("expected length-cap rejection, got nil")
	}
}

func TestZero_IsZero(t *testing.T) {
	t.Parallel()
	var zero email.Address
	if !zero.IsZero() {
		t.Fatal("zero value Address should report IsZero()")
	}
	if zero.String() != "" {
		t.Fatalf("zero.String() = %q, want empty", zero.String())
	}
}

func TestErr_IsClassified(t *testing.T) {
	t.Parallel()
	_, err := email.New("not-an-email")
	if !errs.Is(err, errs.KindInvalidInput) {
		t.Fatalf("expected KindInvalidInput, got %v", errs.KindOf(err))
	}
	if !errors.Is(err, email.ErrInvalid) {
		t.Fatalf("expected errors.Is(_, ErrInvalid) = true, got false")
	}
}
