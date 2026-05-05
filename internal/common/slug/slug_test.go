package slug_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/common/slug"
)

func TestNew_AcceptsValid(t *testing.T) {
	t.Parallel()
	cases := []string{
		"abc",
		"acme-co",
		"acme-co-2",
		"a1b",
		"acme",
		"01-acme",
		"super-long-tenant-name-that-fits-within-the-63-char-rfc1035-cap",
	}
	for _, raw := range cases {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			s, err := slug.New(raw)
			if err != nil {
				t.Fatalf("New(%q): unexpected error %v", raw, err)
			}
			if s.String() != raw {
				t.Fatalf("New(%q).String() = %q", raw, s.String())
			}
		})
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
		{"too short (< 3)", "ab"},
		{"single char", "a"},
		{"too long (> 63)", "a" + string(make([]byte, 64))},
		{"uppercase", "Acme"},
		{"underscore", "acme_co"},
		{"space", "acme co"},
		{"leading hyphen", "-acme"},
		{"trailing hyphen", "acme-"},
		{"double hyphen ok per DNS but disallowed by us for clarity", "acme--co"},
		{"unicode", "açme"},
		{"dot", "acme.co"},
		{"slash", "acme/co"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := slug.New(tc.raw)
			if err == nil {
				t.Fatalf("New(%q): expected error, got nil", tc.raw)
			}
			if got := errs.KindOf(err); got != errs.KindInvalidInput {
				t.Fatalf("New(%q): KindOf = %v, want KindInvalidInput", tc.raw, got)
			}
			if !errors.Is(err, slug.ErrInvalid) {
				t.Fatalf("New(%q): expected errors.Is ErrInvalid, got %v", tc.raw, err)
			}
		})
	}
}

func TestZero_IsZero(t *testing.T) {
	t.Parallel()
	var zero slug.Slug
	if !zero.IsZero() {
		t.Fatal("zero value should report IsZero()")
	}
	if zero.String() != "" {
		t.Fatalf("zero.String() = %q, want empty", zero.String())
	}
}

func TestEqual(t *testing.T) {
	t.Parallel()
	a, _ := slug.New("acme")
	b, _ := slug.New("acme")
	c, _ := slug.New("acme-co")
	if !a.Equal(b) {
		t.Fatal("equal slugs should compare equal")
	}
	if a.Equal(c) {
		t.Fatal("different slugs should not compare equal")
	}
}
