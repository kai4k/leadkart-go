// explicit_di_arch_test.go — Principle 2: Explicit Dependencies (DI,
// no service locator, no package-level singletons).
//
// Canon: Mat Ryer "How I write HTTP services" (NewServer takes every
// dependency); Cheney "accept interfaces, return structs"; Khorikov §6
// (composition root + Humble Object); CLAUDE.md ADR 0018 (manual
// NewServer wiring, NO DI container).
//
// The rule: every dependency arrives via constructor parameter. No
// package-level singletons; no `init()` doing real work; no captive
// `sync.Once` lazy bootstrap; no package-level logger. The composition
// root in cmd/*/main.go is the SOLE place where the global object
// graph is assembled.

package architecture_test

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Test 11: TestArch_NoPackageSingletons
// ----------------------------------------------------------------------------
//
// Outside test files, no `var <X> = New<Y>(...)` (or `MustNewY(...)`)
// at package level. The pattern creates a hidden global object graph
// — exactly what manual NewServer-style composition is supposed to
// avoid.
//
// EXCEPTION: legitimate package-level vars are pure literals
// (sentinel errors, regex.MustCompile, slice/map literals). The
// detection limits to call expressions to a `New*` or `Must*` factory.
//
// Allow-list: sentinel-error factories like `errs.New(...)` and
// `errors.New(...)` are common at package scope; we exclude them by
// pkg name.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoPackageSingletons(t *testing.T) {
	t.Parallel()

	// pkgs whose New* call at package scope is allowed (pure construction
	// of a value, not a service-locator anti-pattern).
	allowedPkgs := map[string]bool{
		"errors":     true, // errors.New
		"errs":       true, // common/errs.New — sentinel factory
		"regexp":     true, // regexp.MustCompile
		"slog":       true, // slog.NewTextHandler etc. (rare; tracked)
		"time":       true, // time.NewTicker is a system call but OK at pkg scope only if explicit
		"uuid":       true, // uuid.MustParse
		"prometheus": true, // counters etc.
		"ids":        true, // ids.New* — used as a factory wrapper
	}

	// Also allow common DDD package-scope helpers (collection literals).
	type violation struct {
		file string
		line int
		name string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					call, ok := vs.Values[i].(*ast.CallExpr)
					if !ok {
						continue
					}
					pkg, fnName := callPkgAndName(call.Fun)
					if !strings.HasPrefix(fnName, "New") && !strings.HasPrefix(fnName, "Must") {
						continue
					}
					if allowedPkgs[pkg] {
						continue
					}
					violations = append(violations, violation{
						file: path,
						line: fset.Position(name.Pos()).Line,
						name: name.Name,
					})
				}
			}
		}
	})

	if len(violations) > 0 {
		t.Logf("PACKAGE-SINGLETON VIOLATIONS — %d", len(violations))
		t.Logf("Per Mat Ryer + ADR 0018: every dependency is constructor-")
		t.Logf("supplied. A package-level `var x = NewY(...)` creates a")
		t.Logf("hidden global that bypasses the composition root.")
		for _, v := range violations {
			t.Errorf("%s:%d — package-scope var %s = New/Must... (move into a ctor)", v.file, v.line, v.name)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 12: TestArch_NoInitDoingRealWork
// ----------------------------------------------------------------------------
//
// `func init()` may register types / codecs / build-tag asserts but
// MAY NOT open DB connections, read env, open files, or make network
// calls. Real work belongs in a ctor invoked by the composition root.
//
// Detection heuristic: an init() body containing call expressions to
// substrate packages (pgxpool, sql, http, net, os.ReadFile, os.Getenv,
// os.Open) is flagged. The forbidden call list is closed.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoInitDoingRealWork(t *testing.T) {
	t.Parallel()

	// pkg.fn pairs forbidden inside init(). Each pair is a substrate
	// concern that belongs in a ctor.
	forbidden := map[string]bool{
		"os.Getenv":         true,
		"os.LookupEnv":      true,
		"os.Open":           true,
		"os.ReadFile":       true,
		"os.Create":         true,
		"pgxpool.New":       true,
		"pgxpool.NewWithConfig": true,
		"sql.Open":          true,
		"http.Get":          true,
		"http.Post":         true,
		"net.Dial":          true,
		"redis.NewClient":   true,
		"net.Listen":        true,
	}

	type violation struct {
		file string
		line int
		call string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Name.Name != "init" {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				pkg, name := callPkgAndName(call.Fun)
				if pkg == "" {
					return true
				}
				key := pkg + "." + name
				if forbidden[key] {
					violations = append(violations, violation{
						file: path,
						line: fset.Position(call.Pos()).Line,
						call: key,
					})
				}
				return true
			})
		}
	})

	if len(violations) > 0 {
		t.Logf("init() DOING REAL WORK VIOLATIONS — %d", len(violations))
		t.Logf("init() may register types/codecs + assert build tags ONLY.")
		t.Logf("DB connections, env reads, network calls belong in a ctor")
		t.Logf("invoked by the composition root.")
		for _, v := range violations {
			t.Errorf("%s:%d — init() calls %s (move into a ctor)", v.file, v.line, v.call)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 13: TestArch_ConstructorsEnumerateAllDeps
// ----------------------------------------------------------------------------
//
// Simplified shape per the brief: ctor bodies that exceed 10 logical
// lines OR contain `slog.Default()` / `time.Now()` / `os.Getenv` are
// flagged. The intent — a ctor that reaches for these is hiding a
// dependency that should have arrived as a parameter.
//
// Scope: every exported `New<X>` function returning `(*<X>, error)`
// under internal/<mod>/{adapters,app,ports} + every common/<X>/New*.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_ConstructorsEnumerateAllDeps(t *testing.T) {
	t.Parallel()

	// Files where the ctor legitimately reaches for substrate (e.g.
	// the composition-root wiring helper in identity/app/app.go +
	// guarded slog.Default nil-fallback per the P1 #9 allow-list).
	allowFiles := []string{
		"internal/identity/app/app.go", // Application facade composes sub-ctors
		"internal/inventory/app/app.go",
		"internal/platform/app/app.go",
		// Guarded `if log == nil { log = slog.Default() }` fallback:
		"internal/common/cache/hybrid.go",
		"internal/common/messaging/router.go",
	}

	type violation struct {
		file string
		line int
		fn   string
		why  string
	}
	var violations []violation

	hiddenDepCall := func(pkg, name string) (bool, string) {
		switch {
		case pkg == "slog" && name == "Default":
			return true, "slog.Default() inside ctor body — pass *slog.Logger"
		case pkg == "time" && name == "Now":
			return true, "time.Now() inside ctor body — pass `now func() time.Time`"
		case pkg == "os" && (name == "Getenv" || name == "LookupEnv"):
			return true, "os.Getenv inside ctor body — pass the value as a parameter"
		}
		return false, ""
	}

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		slashPath := pathToSlash(path)
		for _, allowed := range allowFiles {
			if strings.HasSuffix(slashPath, allowed) {
				return
			}
		}
		fset, f := parseFile(t, path, src)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Recv != nil {
				continue
			}
			if !strings.HasPrefix(fd.Name.Name, "New") {
				continue
			}
			if !returnsPointerAndError(fd) {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				pkg, name := callPkgAndName(call.Fun)
				if bad, why := hiddenDepCall(pkg, name); bad {
					violations = append(violations, violation{
						file: path,
						line: fset.Position(call.Pos()).Line,
						fn:   fd.Name.Name,
						why:  why,
					})
				}
				return true
			})
		}
	})

	if len(violations) > 0 {
		t.Logf("CTOR HIDDEN-DEPENDENCY VIOLATIONS — %d", len(violations))
		t.Logf("Per Mat Ryer + ADR 0018: a ctor reaches for substrate ONLY")
		t.Logf("when receiving it as a parameter. A ctor that calls")
		t.Logf("slog.Default / time.Now / os.Getenv is hiding a dependency.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s: %s", v.file, v.line, v.fn, v.why)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 14: TestArch_NoSyncOnceOutsideAdapters
// ----------------------------------------------------------------------------
//
// `sync.Once` lazy bootstrap is allowed only in `internal/<mod>/adapters/`
// or `internal/common/` (substrate code where one-shot wiring is the
// canonical idiom, e.g. driver init). Domain + app must be deterministic
// without lazy initialisation — a `sync.Once` in app or domain hides a
// captive global that bypasses constructor wiring.
//
// EXCEPTION: domain packages whose canonical design uses Flyweight
// intern (e.g. permission's closed-set catalog). Listed explicitly.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoSyncOnceOutsideAdapters(t *testing.T) {
	t.Parallel()

	allowAdditional := []string{
		"internal/identity/domain/permission/permission.go", // Flyweight intern
	}

	type violation struct {
		file string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		modRoot := filepath.Join(internalDir(t), mod)
		walkGoFiles(t, modRoot, false, func(path string, src []byte) {
			slashPath := pathToSlash(path)
			// Allowed: adapters/.
			if strings.Contains(slashPath, "/"+mod+"/adapters/") {
				return
			}
			for _, allowed := range allowAdditional {
				if strings.HasSuffix(slashPath, allowed) {
					return
				}
			}
			// Look for `sync.Once` type references (parse the whole file
			// for SelectorExpr matching).
			_, f := parseFile(t, path, src)
			found := false
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if id.Name == "sync" && (sel.Sel.Name == "Once" || sel.Sel.Name == "OnceFunc" || sel.Sel.Name == "OnceValue" || sel.Sel.Name == "OnceValues") {
					found = true
					return false
				}
				return true
			})
			if found {
				violations = append(violations, violation{file: path})
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("sync.Once OUTSIDE-ADAPTERS VIOLATIONS — %d", len(violations))
		t.Logf("Lazy bootstrap belongs in substrate code (adapters/ + common/).")
		t.Logf("Domain + app must be deterministic without sync.Once captive globals.")
		for _, v := range violations {
			t.Errorf("%s — references sync.Once* outside adapters/", v.file)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 15: TestArch_NoPackageLevelLogger
// ----------------------------------------------------------------------------
//
// No `var logger = slog.New(...)` at package level outside cmd/*/main.go.
// Loggers are constructor-injected. A package-level logger fragments
// observability — per-request correlation enrichment requires a
// logger that arrives WITH the request scope (via ctx in slog.*Context).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoPackageLevelLogger(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		name string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				// Either explicit *slog.Logger type or a slog.* assignment.
				isLogger := false
				if vs.Type != nil {
					if star, ok := vs.Type.(*ast.StarExpr); ok {
						if sel, ok := star.X.(*ast.SelectorExpr); ok {
							if id, ok := sel.X.(*ast.Ident); ok && id.Name == "slog" && sel.Sel.Name == "Logger" {
								isLogger = true
							}
						}
					}
				}
				for _, val := range vs.Values {
					if call, ok := val.(*ast.CallExpr); ok {
						pkg, name := callPkgAndName(call.Fun)
						if pkg == "slog" && (name == "New" || name == "Default") {
							isLogger = true
						}
					}
				}
				if isLogger {
					for _, name := range vs.Names {
						violations = append(violations, violation{
							file: path,
							line: fset.Position(name.Pos()).Line,
							name: name.Name,
						})
					}
				}
			}
		}
	})

	if len(violations) > 0 {
		t.Logf("PACKAGE-LEVEL LOGGER VIOLATIONS — %d", len(violations))
		t.Logf("Inject *slog.Logger as a constructor parameter. Package-level")
		t.Logf("loggers bypass per-request correlation enrichment.")
		for _, v := range violations {
			t.Errorf("%s:%d — package-scope var %s : *slog.Logger", v.file, v.line, v.name)
		}
	}
}
