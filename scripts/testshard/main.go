// Package main prints the `go test -run` pattern for one slice of a
// package's tests, so a slow test package can run on several CI runners
// at once without anyone keeping a list of test names.
//
//	go run ./scripts/testshard -tags=integration -slice=1 -of=4 ./cmd/api/...
//	→ ^(TestA|TestE|TestI)$
//
// Why a generated pattern, not hand-written prefixes: a prefix list
// (`^TestE2E_[A-F]`) silently skips any test whose name fits no prefix.
// Here the names come from `go test -list` on every run, are sorted, and
// are dealt round-robin: every test is in exactly one slice, a new test
// needs no CI change, and the slices stay about the same size.
//
// Why a Go binary, not shell: Task's embedded shell has no awk / paste on
// Windows (same reason as scripts/devenv).
//
// Invoked by `task ci:test:int:shard` when SLICES is set. Output is the
// pattern on stdout; it exits non-zero when the slice would be empty, so a
// runner can never report green after running nothing.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// testName matches the top-level names `go test -list` prints; everything
// else it prints ("ok  pkg  0.1s", build output) is not a test name.
var testName = regexp.MustCompile(`^(Test|Example|Fuzz)[A-Za-z0-9_]*$`)

func main() {
	tags := flag.String("tags", "", "build tags passed to go test -list")
	slice := flag.Int("slice", 0, "which slice to print, 1-based")
	of := flag.Int("of", 0, "how many slices the tests are dealt into")
	flag.Parse()

	pattern, err := run(context.Background(), *tags, *slice, *of, flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "testshard:", err)
		os.Exit(1)
	}
	fmt.Println(pattern)
}

func run(ctx context.Context, tags string, slice, of int, pkgs []string) (string, error) {
	if len(pkgs) == 0 {
		return "", errors.New("no package given")
	}
	args := []string{"test", "-list", "^(Test|Example|Fuzz)"}
	if tags != "" {
		args = append(args, "-tags="+tags)
	}
	args = append(args, pkgs...)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "go", args...) //nolint:gosec // G204: fixed binary, arguments are flags and package paths from the Taskfile
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go test -list: %w\n%s", err, stderr.String())
	}

	names, err := deal(parseNames(stdout.String()), slice, of)
	if err != nil {
		return "", err
	}
	return pattern(names), nil
}

// parseNames returns the sorted, de-duplicated test names in `go test
// -list` output.
func parseNames(listOutput string) []string {
	seen := map[string]struct{}{}
	var names []string
	sc := bufio.NewScanner(strings.NewReader(listOutput))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !testName.MatchString(line) {
			continue
		}
		if _, dup := seen[line]; dup {
			continue
		}
		seen[line] = struct{}{}
		names = append(names, line)
	}
	sort.Strings(names)
	return names
}

// deal returns the names of slice `slice` (1-based) when the sorted names
// are dealt round-robin into `of` slices. An empty slice is an error.
func deal(names []string, slice, of int) ([]string, error) {
	if of < 1 {
		return nil, fmt.Errorf("-of must be at least 1, got %d", of)
	}
	if slice < 1 || slice > of {
		return nil, fmt.Errorf("-slice must be between 1 and %d, got %d", of, slice)
	}
	var out []string
	for i, name := range names {
		if i%of == slice-1 {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("slice %d of %d is empty (%d tests found)", slice, of, len(names))
	}
	return out, nil
}

// pattern anchors the names so `-run` matches exactly these top-level
// tests and nothing that merely starts with one of them.
func pattern(names []string) string {
	return "^(" + strings.Join(names, "|") + ")$"
}
