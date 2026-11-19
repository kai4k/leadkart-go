package ids_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
)

func TestCanonical(t *testing.T) {
	t.Parallel()
	const want = "0190a000-0000-7000-8000-00000000000a"
	accept := map[string]string{
		"lowercase hyphenated": "0190a000-0000-7000-8000-00000000000a",
		"uppercase":            "0190A000-0000-7000-8000-00000000000A",
		"urn prefix":           "urn:uuid:0190a000-0000-7000-8000-00000000000a",
		"braces":               "{0190a000-0000-7000-8000-00000000000a}",
		"hyphenless":           "0190a000000070008000" + "00000000000a",
		"hyphenless uppercase": "0190A000000070008000" + "00000000000A",
	}
	for name, in := range accept {
		t.Run("accepts "+name, func(t *testing.T) {
			t.Parallel()
			got, err := ids.Canonical(in)
			if err != nil || got != want {
				t.Fatalf("Canonical(%q) = %q, %v; want %q", in, got, err, want)
			}
		})
	}
	reject := map[string]string{
		"empty":                    "",
		"only spaces":              "   ",
		"leading space":            " 0190a000-0000-7000-8000-00000000000a",
		"trailing space":           "0190a000-0000-7000-8000-00000000000a ",
		"trailing newline":         "0190a000-0000-7000-8000-00000000000a\n",
		"leading tab":              "\t0190a000-0000-7000-8000-00000000000a",
		"leading NBSP":             " 0190a000-0000-7000-8000-00000000000a",
		"internal space":           "0190a000-0000-7000 8000-00000000000a",
		"internal NBSP":            "0190a000-0000-7000 8000-00000000000a",
		"nil uuid":                 "00000000-0000-0000-0000-000000000000",
		"nil uuid hyphenless":      "00000000000000000000000000000000",
		"nil uuid urn":             "urn:uuid:00000000-0000-0000-0000-000000000000",
		"nil uuid braces":          "{00000000-0000-0000-0000-000000000000}",
		"not a uuid":               "not-a-uuid",
		"too short":                "0190a000-0000-7000-8000-00000000000",
		"non-hex":                  "0190a000-0000-7000-8000-00000000000g",
		"braced with whitespace":   "{0190a000-0000-7000-8000-00000000000a} ",
		"urn prefix with trailing": "urn:uuid:0190a000-0000-7000-8000-00000000000a\n",
	}
	for name, in := range reject {
		t.Run("rejects "+name, func(t *testing.T) {
			t.Parallel()
			got, err := ids.Canonical(in)
			if !errors.Is(err, ids.ErrInvalidID) || got != "" {
				t.Fatalf("Canonical(%q) = %q, %v; want \"\" and ErrInvalidID", in, got, err)
			}
		})
	}
}
