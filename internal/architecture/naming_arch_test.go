// naming_arch_test.go — Principle V: Naming conventions.
//
// Effective Go + Go community canon: package names lowercase
// single-word, interface names ending in -er for single-method
// interfaces, constructors named NewX returning *X, short
// (1-3 char) receiver names consistent across all methods.
//
// These conventions are stylistic but load-bearing — drift makes
// the codebase feel "not Go" to newcomers + breaks tooling
// heuristics (gopls auto-import, etc.).
//
// Cited canon:
//   - Effective Go — package naming
//   - Andrew Gerrand — "Idiomatic naming conventions" (2014)
//   - Cheney — "Practical Go" §2 (receivers + interface naming)
//   - Russ Cox — "The Zen of Go" (2020) talk

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// V1: TestArch_PackageNamesLowercaseSingleWord
// ----------------------------------------------------------------------------
//
// Package names: lowercase, no underscores, no camelCase. Effective
// Go canon. Drift signal: when devs from camelCase languages start
// contributing, packages slowly accrete `userManagement` etc.
func TestArch_PackageNamesLowercaseSingleWord(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	pkgRE := regexp.MustCompile(`^package\s+(\w+)\s*$`)
	seen := map[string]struct{}{}
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		for _, ln := range strings.Split(string(src), "\n") {
			m := pkgRE.FindStringSubmatch(strings.TrimSpace(ln))
			if m == nil {
				continue
			}
			name := m[1]
			if _, ok := seen[name]; ok {
				return
			}
			seen[name] = struct{}{}
			if strings.Contains(name, "_") {
				bad = append(bad, pathToSlash(path)+": "+name+" (underscore)")
			}
			for _, r := range name {
				if r >= 'A' && r <= 'Z' {
					bad = append(bad, pathToSlash(path)+": "+name+" (uppercase)")
					break
				}
			}
			return
		}
	})

	if len(bad) > 0 {
		t.Fatalf("non-canonical package name (Effective Go: lowercase no underscore):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// V2: TestArch_SingleMethodInterfacesUseErSuffix
// ----------------------------------------------------------------------------
//
// Single-method interfaces SHOULD end in `-er`. (Reader, Writer,
// Closer, Stringer.) Codified in Effective Go. Multi-method
// interfaces get descriptive names (Repository, Gateway). The
// distinction is the most reliable single signal for "is this a
// behaviour or a thing".
//
// Allow-list: interfaces whose only method is `IsX() bool` (predicate
// shape, not action), or whose name explicitly ends in a noun-form
// (`Repository`, `Gateway`, `Store`, `Service`, `Reader`, `Writer`)
// — those are acceptable conventions even with one method.
func TestArch_SingleMethodInterfacesUseErSuffix(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	allowedSuffixes := []string{
		"er", // Reader, Writer, Closer, Verifier, etc.
		"or", // Validator (idiomatically -or in Go)
	}
	allowedNouns := map[string]bool{
		"Repository": true, "Gateway": true, "Store": true,
		"Service": true, "Container": true, "Reader": true,
		"Writer": true, "Bus": true, "Checker": true,
		// DDD canon (Vernon IDDD ch. 8: aggregate events as a base
		// interface named `Event`).
		"Event":       true,
		"Aggregate":   true,
		"ValueObject": true,
		// Hexagonal substrate (ADR 0047) — interfaces with one method
		// describing a process-control role.
		"UnitOfWork":   true,
		"Transactor":   true,
		"Transactional": true,
		"TenantScoped":  true,
		"Platform":      true, // platform-event marker interface
		"Fake":          true, // *Fake interfaces in moduletest/
	}

	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		slash := pathToSlash(path)
		if strings.Contains(slash, "/adapters/db/") {
			return
		}
		_, file := parseFile(t, path, src)
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok || it.Methods == nil {
				return true
			}
			// Count actual methods (skip embedded).
			methods := 0
			for _, f := range it.Methods.List {
				if _, isFn := f.Type.(*ast.FuncType); isFn {
					methods++
				}
			}
			if methods != 1 {
				return true
			}
			name := ts.Name.Name
			if !ts.Name.IsExported() {
				return true
			}
			if allowedNouns[name] {
				return true
			}
			for _, sfx := range allowedSuffixes {
				if strings.HasSuffix(name, sfx) {
					return true
				}
			}
			// Also accept names ending in a known suffix-noun.
			for noun := range allowedNouns {
				if strings.HasSuffix(name, noun) {
					return true
				}
			}
			bad = append(bad, slash+": "+name)
			return true
		})
	})

	if len(bad) > 0 {
		t.Fatalf("exported single-method interface doesn't end in -er/-or or known noun (Effective Go):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// V3: TestArch_ConstructorsNamedNewX
// ----------------------------------------------------------------------------
//
// Constructors are `NewT` returning `*T` (or `T`). Free-form factory
// names (`BuildT`, `MakeT`, `Create*Result`) are not constructors and
// are usually a hint that the function is doing too much.
//
// Heuristic: function returning a value type whose name matches
// `func ... (...) (*T, error)` should be named `NewT` or `Must<T>`
// or `Open<T>` (canonical alternative for I/O ctors).
//
// Allow-list: Test* / Build* / arch-test:non-ctor markers.
func TestArch_ConstructorsNamedNewX(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	allowedPrefixes := []string{"New", "Must", "Open", "Init", "Make", "Build", "Get", "From", "Parse", "Create", "Decode", "Encode", "Load", "Provide", "Connect", "Try", "Wrap", "With", "Default", "Apply", "Setup", "Configure", "Register"}

	walkGoFiles(t, root, false, func(path string, src []byte) {
		slash := pathToSlash(path)
		if strings.Contains(slash, "/adapters/db/") {
			return
		}
		_, file := parseFile(t, path, src)
		ast.Inspect(file, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok || !fd.Name.IsExported() {
				return true
			}
			if fd.Recv != nil {
				return true // method, not free func
			}
			if !returnsPointerAndError(fd) {
				return true
			}
			name := fd.Name.Name
			for _, p := range allowedPrefixes {
				if strings.HasPrefix(name, p) {
					return true
				}
			}
			bad = append(bad, slash+": "+name)
			return true
		})
	})

	if len(bad) > 0 {
		t.Fatalf("exported (*T, error)-returning fn doesn't use New*/Must*/Open*/etc. prefix:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// V4: TestArch_ReceiverNamesShortAndConsistent
// ----------------------------------------------------------------------------
//
// Method receivers: 1-3 chars; consistent across all methods on the
// same type. `self` / `this` / `me` are Java/C# hangovers, banned.
//
// Per Effective Go: "The name of a method's receiver should be a
// reflection of its identity; often a one or two letter abbreviation
// of its type suffices (such as 'c' or 'cl' for 'Client')."
func TestArch_ReceiverNamesShortAndConsistent(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	banned := map[string]bool{"self": true, "this": true, "me": true, "obj": true}
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		slash := pathToSlash(path)
		if strings.Contains(slash, "/adapters/db/") {
			return
		}
		_, file := parseFile(t, path, src)
		// (typeName -> set of receiver names seen)
		recvByType := map[string]map[string]struct{}{}
		ast.Inspect(file, func(n ast.Node) bool {
			fd, ok := n.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
				return true
			}
			recvField := fd.Recv.List[0]
			if len(recvField.Names) == 0 {
				return true
			}
			rname := recvField.Names[0].Name
			rtype := exprString(recvField.Type)
			rtype = strings.TrimPrefix(rtype, "*")
			if banned[strings.ToLower(rname)] {
				bad = append(bad, slash+": "+fd.Name.Name+" receiver "+rname+" banned (self/this/me)")
			}
			if len(rname) > 3 && rname != "_" {
				bad = append(bad, slash+": "+fd.Name.Name+" receiver "+rname+" exceeds 3 chars")
			}
			if recvByType[rtype] == nil {
				recvByType[rtype] = map[string]struct{}{}
			}
			recvByType[rtype][rname] = struct{}{}
			return true
		})
		// Inconsistent receivers per type.
		for typ, names := range recvByType {
			if len(names) > 1 {
				// Build the set string for the error.
				var set []string
				for n := range names {
					set = append(set, n)
				}
				bad = append(bad, slash+": "+typ+" inconsistent receivers "+strings.Join(set, "/"))
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("receiver naming violates Effective Go (short + consistent):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// Ensure filepath is used (it's pulled in via helpers).
var _ = filepath.Join
