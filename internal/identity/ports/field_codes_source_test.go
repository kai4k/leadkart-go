package ports

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fieldErrorLiteral finds a string-literal value written straight into an errors
// map: `map[string][]string{"field": {"value", ...}}`.
var fieldErrorLiteral = regexp.MustCompile(`map\[string\]\[\]string\{\s*"[^"]+":\s*\{([^}]*)\}`)

// A field error is a stable code, never a sentence: a client cannot translate or
// match English. No literal value written into an errors map by identity's HTTP
// layer may hold a space (values built at run time are covered by the per-route
// tests, which assert the same on the wire).
func TestIdentityPorts_ErrorsMapLiteralsAreCodes(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range fieldErrorLiteral.FindAllStringSubmatch(string(src), -1) {
			for _, v := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(m[1], -1) {
				if strings.ContainsAny(v[1], " \t") {
					t.Errorf("%s: errors-map value %q is a sentence, use a stable code", f, v[1])
				}
			}
		}
	}
}
