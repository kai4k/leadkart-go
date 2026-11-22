package main

import (
	"regexp"
	"strings"
	"testing"
)

const listOutput = `TestZeta
TestAlpha
TestE2E_Login_Works
ExampleThing
FuzzParse
TestAlpha
ok  	github.com/leadkart/leadkart-go/cmd/api	0.512s
TestE2E_Login
BenchmarkNotATest
--- some build noise
`

func TestParseNames_SortedUniqueAndOnlyTests(t *testing.T) {
	t.Parallel()
	got := parseNames(listOutput)
	want := []string{"ExampleThing", "FuzzParse", "TestAlpha", "TestE2E_Login", "TestE2E_Login_Works", "TestZeta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("parseNames = %v, want %v", got, want)
	}
}

// Every test must run on exactly one runner, whatever the slice count.
func TestDeal_EveryNameInExactlyOneSlice(t *testing.T) {
	t.Parallel()
	names := parseNames(listOutput)
	for of := 1; of <= len(names); of++ {
		seen := map[string]int{}
		for slice := 1; slice <= of; slice++ {
			part, err := deal(names, slice, of)
			if err != nil {
				t.Fatalf("deal(%d of %d): %v", slice, of, err)
			}
			for _, n := range part {
				seen[n]++
			}
		}
		for _, n := range names {
			if seen[n] != 1 {
				t.Errorf("of=%d: %s is in %d slices, want exactly 1", of, n, seen[n])
			}
		}
	}
}

func TestDeal_SlicesDifferInSizeByAtMostOne(t *testing.T) {
	t.Parallel()
	names := make([]string, 0, 218)
	for i := range 218 {
		names = append(names, "Test"+strings.Repeat("x", i%7)+string(rune('A'+i%26))+strings.Repeat("y", i/26))
	}
	const of = 5
	minLen, maxLen := len(names), 0
	for slice := 1; slice <= of; slice++ {
		part, err := deal(names, slice, of)
		if err != nil {
			t.Fatal(err)
		}
		minLen, maxLen = min(minLen, len(part)), max(maxLen, len(part))
	}
	if maxLen-minLen > 1 {
		t.Fatalf("slice sizes range %d..%d, want a difference of at most 1", minLen, maxLen)
	}
}

// A runner that would run nothing must fail, never report green.
func TestDeal_RefusesAnEmptySliceAndBadArguments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		names     []string
		slice, of int
	}{
		{"more slices than tests", []string{"TestA"}, 2, 2},
		{"no tests at all", nil, 1, 1},
		{"slice zero", []string{"TestA"}, 0, 2},
		{"slice past the end", []string{"TestA"}, 3, 2},
		{"zero slices", []string{"TestA"}, 1, 0},
	}
	for _, tc := range cases {
		if _, err := deal(tc.names, tc.slice, tc.of); err == nil {
			t.Errorf("%s: deal returned no error", tc.name)
		}
	}
}

// The pattern must select exactly the named tests: a name that is a prefix
// of another (TestE2E_Login / TestE2E_Login_Works) must not pull it in.
func TestPattern_MatchesExactlyTheNamedTests(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(pattern([]string{"TestE2E_Login", "TestAlpha"}))
	for name, want := range map[string]bool{
		"TestE2E_Login":       true,
		"TestAlpha":           true,
		"TestE2E_Login_Works": false,
		"TestAlphabet":        false,
		"XTestAlpha":          false,
	} {
		if got := re.MatchString(name); got != want {
			t.Errorf("pattern matches %q = %v, want %v", name, got, want)
		}
	}
}
