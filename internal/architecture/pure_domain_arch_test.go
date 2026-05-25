// pure_domain_arch_test.go — Principle 1: Pure Domain.
//
// The domain layer has no hidden inputs and no hidden outputs. Time,
// randomness, identifiers, environment, filesystem, concurrency, and
// loggers are all dependencies — they enter the domain via method
// parameters or constructor-injected closures, NEVER via package-level
// globals or substrate-package calls.
//
// Canon: Khorikov "Unit Testing Principles" §4 + §11 (side-effect-free
// domain, mocking time); Vernon IDDD ch. 4 (Aggregates); TDL Wild
// Workouts (the canonical Go DDD reference); Hennessy & Sicheri,
// "Designing Data-Intensive Applications" §2 (the "pure function"
// boundary).

package architecture_test

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Test 1: TestArch_NoClockPackageReference
// ----------------------------------------------------------------------------
//
// Per the May 2026 clock-injection refactor (commit a33e9a0): the
// previous `internal/common/clock` package is DELETED. Aggregates take
// `now time.Time` as an explicit parameter at the end of their method
// signatures. Handlers carry a `now func() time.Time` constructor
// dependency; composition root wires `time.Now`, tests inject a fixed
// closure. Any future re-introduction of a clock package or a
// `clock.Now()` call trips a PR-time failure.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoClockPackageReference(t *testing.T) {
	t.Parallel()

	tokens := []string{"clock.Now", "clock.Set", "clock.Reset", "freezeClock", "activeFreezes"}

	type violation struct {
		file  string
		token string
		line  int
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		stripped := stripGoComments(string(src))
		for _, tok := range tokens {
			idx := 0
			for {
				j := strings.Index(stripped[idx:], tok)
				if j < 0 {
					break
				}
				absolute := idx + j
				line := 1 + strings.Count(stripped[:absolute], "\n")
				violations = append(violations, violation{file: path, token: tok, line: line})
				idx = absolute + len(tok)
			}
		}
	})

	if len(violations) > 0 {
		t.Logf("CLOCK-PACKAGE REVIVAL VIOLATIONS — %d", len(violations))
		t.Logf("The internal/common/clock package was deleted in May 2026.")
		t.Logf("Use the injected `now func() time.Time` on the handler or")
		t.Logf("accept `now time.Time` as a parameter.")
		for _, v := range violations {
			t.Errorf("%s:%d — references %s", v.file, v.line, v.token)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 2: TestArch_NoTimeNowInDomain
// ----------------------------------------------------------------------------
//
// Domain layer is time-pure. Per Khorikov §11 + Wild Workouts + Vernon
// IDDD: time flows in via method parameters (`now time.Time` last arg).
// A direct `time.Now()` call in the domain makes the aggregate
// non-deterministic + un-testable without monkey-patching.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoTimeNowInDomain(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			fset, f := parseFile(t, path, src)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				pkg, name := callPkgAndName(call.Fun)
				if pkg == "time" && name == "Now" {
					violations = append(violations, violation{
						file: path,
						line: fset.Position(call.Pos()).Line,
					})
				}
				return true
			})
		})
	}

	if len(violations) > 0 {
		t.Logf("DOMAIN-LAYER time.Now() VIOLATIONS — %d", len(violations))
		t.Logf("Per Khorikov §11: domain is time-pure. Take `now time.Time`")
		t.Logf("as a method parameter and let handlers pass the wall-clock value.")
		for _, v := range violations {
			t.Errorf("%s:%d — time.Now() inside domain", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 3: TestArch_HandlersInjectNow
// ----------------------------------------------------------------------------
//
// Every *Handler method in app/command/ or app/query/ that needs
// wall-clock time MUST acquire it from an injected
// `now func() time.Time` field, NOT by calling `time.Now()` directly.
// The composition root wires `time.Now` into every handler ctor; tests
// inject a fixed-time closure for determinism.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_HandlersInjectNow(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		fn   string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		for _, sub := range []string{"command", "query"} {
			dir := filepath.Join(internalDir(t), mod, "app", sub)
			walkGoFiles(t, dir, false, func(path string, src []byte) {
				fset, f := parseFile(t, path, src)
				for _, decl := range f.Decls {
					fd, ok := decl.(*ast.FuncDecl)
					if !ok || fd.Recv == nil || fd.Body == nil {
						continue
					}
					if !isHandlerReceiver(fd.Recv) {
						continue
					}
					ast.Inspect(fd.Body, func(n ast.Node) bool {
						call, ok := n.(*ast.CallExpr)
						if !ok {
							return true
						}
						pkg, name := callPkgAndName(call.Fun)
						if pkg == "time" && name == "Now" {
							violations = append(violations, violation{
								file: path,
								line: fset.Position(call.Pos()).Line,
								fn:   fd.Name.Name,
							})
						}
						return true
					})
				}
			})
		}
	}

	if len(violations) > 0 {
		t.Logf("HANDLER time.Now() VIOLATIONS — %d", len(violations))
		t.Logf("Handlers acquire time via an injected `now func() time.Time`.")
		for _, v := range violations {
			t.Errorf("%s:%d — handler method %s calls time.Now() directly", v.file, v.line, v.fn)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 4: TestArch_NoRandInDomain
// ----------------------------------------------------------------------------
//
// Random is a hidden input. Domain methods that need entropy take an
// `io.Reader` parameter or accept caller-supplied values. Importing
// crypto/rand or math/rand directly in the domain breaks determinism
// and forces test code into monkey-patching gymnastics.
//
// Canon: Khorikov §11 (mocking time generalises to all environmental
// inputs); Wild Workouts repository pattern (callers own the random
// source).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoRandInDomain(t *testing.T) {
	t.Parallel()

	forbidden := map[string]bool{
		"crypto/rand":     true,
		"math/rand":       true,
		"math/rand/v2":    true,
	}

	type violation struct {
		file string
		imp  string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			for _, imp := range parseImports(t, path, src) {
				if forbidden[imp] {
					violations = append(violations, violation{file: path, imp: imp})
				}
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("DOMAIN rand IMPORT VIOLATIONS — %d", len(violations))
		t.Logf("Random is a hidden input. Pass an io.Reader or accept the")
		t.Logf("already-generated value as a method/ctor parameter.")
		for _, v := range violations {
			t.Errorf("%s — imports %s", v.file, v.imp)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 5: TestArch_NoUUIDGenerationInDomain
// ----------------------------------------------------------------------------
//
// IDs are caller-supplied. Aggregates take `id <Type>ID` as a
// constructor parameter; the calling handler owns the factory
// (typically wired from `ids.NewV7` in the composition root). Calling
// `ids.NewV7`, `uuid.New*`, or `uuid.NewV7` inside domain code makes
// the aggregate non-deterministic across replays and prevents
// idempotency-keyed retries from reproducing the same row.
//
// Canon: Vernon IDDD ch. 5 (Entities — identity given at creation);
// Wild Workouts (every ctor takes `id X.ID`).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoUUIDGenerationInDomain(t *testing.T) {
	t.Parallel()

	// Allow-list: aggregates whose internal child-entity mints are
	// canonical patterns (per the original Wild Workouts refresh-token
	// design + Vernon IDDD ch. 5 sub-entity composition). Each entry
	// is cited in KNOWN_VIOLATIONS.md with the canon link.
	//
	// We MATCH on file path suffix (forward slashes) so the allow-list
	// stays stable across OS-specific path separators.
	allowList := []string{
		"internal/identity/domain/refreshtoken/family.go",      // Token children minted inside the Family aggregate (sub-entity composition)
		"internal/identity/domain/person/credential.go",        // Reset-token + email-change-token (sub-VO mint inside credential rotation)
		"internal/identity/domain/impersonation/session.go",    // Session ID minted via factory inside the aggregate (in-memory store; ADR 0051)
	}

	type violation struct {
		file string
		line int
		expr string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			slashPath := pathToSlash(path)
			for _, allowed := range allowList {
				if strings.HasSuffix(slashPath, allowed) {
					return
				}
			}
			fset, f := parseFile(t, path, src)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				pkg, name := callPkgAndName(call.Fun)
				if (pkg == "ids" || pkg == "uuid") && strings.HasPrefix(name, "New") {
					violations = append(violations, violation{
						file: path,
						line: fset.Position(call.Pos()).Line,
						expr: pkg + "." + name,
					})
				}
				return true
			})
		})
	}

	if len(violations) > 0 {
		t.Logf("DOMAIN UUID-GENERATION VIOLATIONS — %d", len(violations))
		t.Logf("Per Vernon IDDD: identity is given at creation, never minted")
		t.Logf("inside the domain. Take `id <T>.ID` as a ctor parameter and")
		t.Logf("let the handler mint via the injected id-factory.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s() called from domain", v.file, v.line, v.expr)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 6: TestArch_NoOsGetenvOutsideConfigAndMain
// ----------------------------------------------------------------------------
//
// `os.Getenv` / `os.LookupEnv` are allowed ONLY in:
//   - cmd/*/main.go (composition root reads env into AppConfig)
//   - internal/common/config/ (koanf-backed config loader)
//   - scripts/devenv/* (dev-environment helpers)
//   - test files
//
// Domain/app/ports/adapters never reach into env directly — config
// arrives through constructor parameters. Anything else is a hidden
// input that nullifies determinism + makes the affected code untestable
// without env-var fixtures.
//
// Canon: 12-factor app §III (config in env, READ at startup); ADR 0017
// (koanf); Brandur "12-factor without the cult" (config is data, not
// behaviour).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoOsGetenvOutsideConfigAndMain(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		call string
	}
	var violations []violation

	allowedPathSubstrings := []string{
		"/internal/common/config/",
		"/cmd/",                 // catches cmd/api/main.go + cmd/worker/main.go etc.
		"/scripts/devenv/",
	}
	isAllowed := func(slashPath string) bool {
		for _, sub := range allowedPathSubstrings {
			if strings.Contains(slashPath, sub) {
				return true
			}
		}
		return false
	}

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		slashPath := pathToSlash(path)
		if isAllowed(slashPath) {
			return
		}
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pkg, name := callPkgAndName(call.Fun)
			if pkg == "os" && (name == "Getenv" || name == "LookupEnv") {
				violations = append(violations, violation{
					file: path,
					line: fset.Position(call.Pos()).Line,
					call: pkg + "." + name,
				})
			}
			return true
		})
	})

	if len(violations) > 0 {
		t.Logf("env-READ-OUTSIDE-CONFIG VIOLATIONS — %d", len(violations))
		t.Logf("Per 12-factor §III + ADR 0017: env is read once, in the")
		t.Logf("composition root (cmd/*/main.go) or the koanf loader")
		t.Logf("(internal/common/config). Everywhere else takes config as a")
		t.Logf("constructor parameter.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s outside cmd/ or common/config/", v.file, v.line, v.call)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 7: TestArch_NoFilesystemInDomain
// ----------------------------------------------------------------------------
//
// The domain cannot touch the filesystem. Filesystem I/O is a substrate
// concern — it belongs in adapters. Importing `os`, `io/ioutil`, or
// `path/filepath` for I/O inside the domain makes the aggregate's
// behaviour depend on disk state that no test fixture can isolate.
//
// EXCEPTION: `path` (pure-string operations on slash-separated paths)
// is permitted — it does no I/O.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoFilesystemInDomain(t *testing.T) {
	t.Parallel()

	forbidden := map[string]bool{
		"os":            true,
		"io/ioutil":     true,
		"path/filepath": true,
	}

	type violation struct {
		file string
		imp  string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			for _, imp := range parseImports(t, path, src) {
				if forbidden[imp] {
					violations = append(violations, violation{file: path, imp: imp})
				}
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("DOMAIN FILESYSTEM IMPORT VIOLATIONS — %d", len(violations))
		t.Logf("Filesystem I/O belongs in adapters/. The domain operates on")
		t.Logf("values; let the adapter load + persist bytes.")
		for _, v := range violations {
			t.Errorf("%s — imports %s", v.file, v.imp)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 8: TestArch_NoGoroutinesInDomain
// ----------------------------------------------------------------------------
//
// The domain cannot spawn goroutines or coordinate via channels /
// mutexes. Concurrency is a runtime concern — the domain models the
// business in single-threaded values; cross-aggregate coordination
// happens via integration events, never via in-process goroutines.
//
// Detected: `go <expr>` statements (token.GO); channel type literals
// (`chan T`); imports of sync's mutex/waitgroup primitives.
//
// Canon: Bryan Mills "Rethinking Concurrency Patterns" (GopherCon 2018
// — keep concurrency at the edges); Vernon IDDD ch. 10 (aggregates
// own a single consistency boundary, single-threaded).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoGoroutinesInDomain(t *testing.T) {
	t.Parallel()

	// Allow-list: the permission package is a Flyweight intern + closed-
	// set catalog (Vernon IDDD ch. 6 + Gang-of-Four Flyweight). It uses
	// sync.Once for one-shot intern-pool initialisation. No goroutines
	// are spawned; no channels are used; the lock guards a startup-time
	// memoisation. Documented in KNOWN_VIOLATIONS.md.
	syncImportAllowList := []string{
		"internal/identity/domain/permission/permission.go",
	}

	type violation struct {
		file string
		line int
		kind string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		domainDir := filepath.Join(internalDir(t), mod, "domain")
		walkGoFiles(t, domainDir, false, func(path string, src []byte) {
			slashPath := pathToSlash(path)
			imports := parseImports(t, path, src)
			allowsSync := false
			for _, allowed := range syncImportAllowList {
				if strings.HasSuffix(slashPath, allowed) {
					allowsSync = true
					break
				}
			}
			for _, imp := range imports {
				if imp == "sync" && !allowsSync {
					violations = append(violations, violation{file: path, kind: "imports sync"})
				}
			}
			fset, f := parseFile(t, path, src)
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.GoStmt:
					violations = append(violations, violation{
						file: path,
						line: fset.Position(x.Pos()).Line,
						kind: "go statement",
					})
				case *ast.ChanType:
					violations = append(violations, violation{
						file: path,
						line: fset.Position(x.Pos()).Line,
						kind: "chan type",
					})
				}
				return true
			})
		})
	}

	if len(violations) > 0 {
		t.Logf("DOMAIN CONCURRENCY-PRIMITIVE VIOLATIONS — %d", len(violations))
		t.Logf("Per Bryan Mills + Vernon IDDD ch. 10: keep concurrency at the")
		t.Logf("edges. The aggregate is a single consistency boundary; cross-")
		t.Logf("aggregate effects happen via integration events.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s (forbidden in domain)", v.file, v.line, v.kind)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 9: TestArch_NoSlogDefault
// ----------------------------------------------------------------------------
//
// `slog.Default()` is a package-level global. Production code MUST
// inject a `*slog.Logger` parameter — substituting loggers per-test
// for capture + per-request for correlation-ID enrichment requires
// the logger to be a dependency, not a global.
//
// Canon: Cheney "accept interfaces, return structs"; Mat Ryer
// "How I write HTTP services" (NewServer takes the logger explicitly);
// Khorikov §6 (mocking out cross-cutting concerns).
//
// EXCEPTION: cmd/*/main.go MAY call slog.Default() exactly once for
// bootstrap-time logging before the configured logger is constructed.
// Detection ignores cmd/ entirely.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoSlogDefault(t *testing.T) {
	t.Parallel()

	// Files allow-listed for the GUARDED-NIL-FALLBACK pattern:
	//
	//   if log == nil { log = slog.Default() }
	//
	// All entries here MUST have a `*slog.Logger` constructor parameter
	// (or a Config-struct field) that the caller is meant to supply;
	// the fallback only fires when the caller forgets. This is the
	// common Go library idiom (cf. http.Handler nil-mux, sql.DB default
	// driver). Each entry below was audited by hand for the guarded
	// shape — drift here means a legitimate violation slipped in under
	// an existing whitelist entry. Re-audit when modifying any of these
	// files.
	allowList := []string{
		"internal/common/audit/purge.go",
		"internal/common/audit/writer.go",
		"internal/common/cache/hybrid.go",
		"internal/common/httpmw/recover.go",
		"internal/common/httpmw/requestlog.go",
		"internal/common/messaging/middleware.go",
		"internal/common/messaging/router.go",
		"internal/identity/ports/subscribers/email_sender.go",
		"internal/identity/ports/subscribers/invalidate_cache.go",
		"internal/identity/ports/subscribers/reuse_detected_siem.go",
		"internal/identity/ports/subscribers/revoke_families.go",
		"internal/crm/ports/subscribers/lead_purchased.go",
	}

	type violation struct {
		file string
		line int
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		slashPath := pathToSlash(path)
		for _, allowed := range allowList {
			if strings.HasSuffix(slashPath, allowed) {
				return
			}
		}
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pkg, name := callPkgAndName(call.Fun)
			if pkg == "slog" && name == "Default" {
				violations = append(violations, violation{
					file: path,
					line: fset.Position(call.Pos()).Line,
				})
			}
			return true
		})
	})

	if len(violations) > 0 {
		t.Logf("slog.Default() VIOLATIONS — %d", len(violations))
		t.Logf("Loggers are dependencies. Inject `*slog.Logger` as a ctor")
		t.Logf("parameter (Mat Ryer 2024). cmd/*/main.go is the sole")
		t.Logf("legitimate caller; everywhere else takes log as a param.")
		for _, v := range violations {
			t.Errorf("%s:%d — slog.Default() (inject *slog.Logger instead)", v.file, v.line)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 10: TestArch_HandlersInjectIDFactory
// ----------------------------------------------------------------------------
//
// Handlers that mint new aggregates do so via an injected
// `idFactory func() <Agg>.ID` field OR by accepting the ID from the
// command/request payload (idempotency-key flow). A direct
// `ids.NewV7()` / `uuid.New*()` call inside a handler method bakes the
// random source into the handler — breaking determinism + preventing
// replay-based testing.
//
// Detection: walk every method on a *Handler receiver in app/command/;
// flag CallExprs to `ids.NewV*`, `uuid.New*`.
//
// This is the handler counterpart of P1 #5 (no UUID generation in
// domain): the domain takes IDs as parameters, the handler is where
// minting is allowed — but ONLY via an injected factory so tests
// pin the value.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_HandlersInjectIDFactory(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		fn   string
		expr string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		dir := filepath.Join(internalDir(t), mod, "app", "command")
		walkGoFiles(t, dir, false, func(path string, src []byte) {
			fset, f := parseFile(t, path, src)
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv == nil || fd.Body == nil {
					continue
				}
				if !isHandlerReceiver(fd.Recv) {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					pkg, name := callPkgAndName(call.Fun)
					if (pkg == "ids" || pkg == "uuid") && strings.HasPrefix(name, "New") {
						violations = append(violations, violation{
							file: path,
							line: fset.Position(call.Pos()).Line,
							fn:   fd.Name.Name,
							expr: pkg + "." + name,
						})
					}
					return true
				})
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("HANDLER ID-MINT VIOLATIONS — %d", len(violations))
		t.Logf("Handlers mint via an injected `idFactory func() <T>.ID` field")
		t.Logf("(or accept ID from the command payload for idempotency replays).")
		t.Logf("Direct ids.NewV7() / uuid.New() bakes randomness into the handler.")
		for _, v := range violations {
			t.Errorf("%s:%d — handler %s calls %s() directly", v.file, v.line, v.fn, v.expr)
		}
	}
}

// Sentinel so future maintainers see the canonical token constant
// without having to grep the token package.
var _ = token.GO
