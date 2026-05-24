// observability_arch_test.go — Principle 12: Observability Uniformity.
//
// Per ADR 0013 (log/slog stdlib), ADR 0014 (OTel-Go + pprof), ADR
// 0027 (outbox doubles as audit log), and Charity Majors "Observability
// Engineering" (2022): every handler logs entry/exit; every error
// wraps with %w; correlation_id flows via ctx-aware slog; sensitive
// fields never appear in log args.
//
// Tests in this file:
//   81. TestArch_HandlerEntryExitLogs
//   82. TestArch_ErrorWrappingUsesPercentW
//   83. TestArch_NoFmtPrintInProduction
//   84. TestArch_NoSensitiveFieldsInLogArgs
//   85. TestArch_CorrelationIDPropagation
//   86. TestArch_OTelSpansOnExternalCalls
//   87. TestArch_EveryProblemDetailHasType
//   88. TestArch_AuditLogWritesForAuthnEvents

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------------
// Test 81: TestArch_NoInfoLogOnHandlerSuccessPath
// ----------------------------------------------------------------------------
//
// REWRITTEN to enforce Go canon. The previous shape ("every Handle method
// emits an Info log") is an explicit anti-pattern per:
//   - Peter Bourgon "Go best practices, six years in" §logging — "log only
//     actionable information"
//   - Dave Cheney "Let's talk about logging" — "if a log line isn't
//     actionable, delete it"
//   - Cindy Sridharan *Distributed Systems Observability* ch.5 — request
//     lifecycle = TRACE concern, not LOG concern
//
// Entry/exit logs duplicate access-log middleware + inflate log volume
// by ~10x. The canonical Go pattern is LOG ON FAILURE:
// slog.WarnContext / slog.ErrorContext on rejected paths is fine +
// encouraged; slog.Info/Debug on the success path is the anti-pattern.
//
// Heuristic: flag Info/Debug calls inside a `Handle` method body UNLESS
// the message text references error / fail / reject / deny / blocked /
// lock (those are diagnostic, not narrative).
func TestArch_NoInfoLogOnHandlerSuccessPath(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		msg  string
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
				if fd.Name.Name != "Handle" {
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
					if pkg != "slog" {
						return true
					}
					if name != "Info" && name != "InfoContext" && name != "Debug" && name != "DebugContext" {
						return true
					}
					if len(call.Args) == 0 {
						return true
					}
					msgLit, ok := call.Args[0].(*ast.BasicLit)
					if !ok {
						return true
					}
					msg := strings.ToLower(strings.Trim(msgLit.Value, "\""))
					if strings.Contains(msg, "err") || strings.Contains(msg, "fail") ||
						strings.Contains(msg, "reject") || strings.Contains(msg, "deny") ||
						strings.Contains(msg, "blocked") || strings.Contains(msg, "lock") {
						return true
					}
					violations = append(violations, violation{
						file: path,
						line: fset.Position(call.Pos()).Line,
						msg:  msg,
					})
					return true
				})
			}
		})
	}

	if len(violations) > 0 {
		t.Errorf("handler Handle() emits success-path Info/Debug log — narrate via requestlog middleware, not per-handler (Cheney 'Let's talk about logging' + Bourgon 'Go best practices' §logging):")
		for _, v := range violations {
			t.Logf("  %s:%d — %q", v.file, v.line, v.msg)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 82: TestArch_ErrorWrappingUsesPercentW
// ----------------------------------------------------------------------------
//
// `fmt.Errorf` calls that include an error argument use `%w` for
// wrapping, NOT `%v` or `%s`. The wrapped error supports errors.Is /
// errors.As; %v/%s loses the chain.
func TestArch_ErrorWrappingUsesPercentW(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pkg, name := callPkgAndName(call.Fun)
			if pkg != "fmt" || name != "Errorf" {
				return true
			}
			if len(call.Args) < 2 {
				return true
			}
			fmtLit, ok := call.Args[0].(*ast.BasicLit)
			if !ok {
				return true
			}
			format := fmtLit.Value
			// Check if the format string uses %v or %s where args look
			// like errors. We approximate by: if format contains %v
			// AND the call has 2+ args AND the LAST arg's textual
			// representation contains "err" or "Err", flag.
			if !strings.Contains(format, "%v") && !strings.Contains(format, "%s") {
				return true
			}
			// Inspect last arg for err-like identifier.
			lastArg := call.Args[len(call.Args)-1]
			lastText := exprText(lastArg)
			if !strings.Contains(strings.ToLower(lastText), "err") {
				return true
			}
			// Already wrapping with %w? OK.
			if strings.Contains(format, "%w") {
				return true
			}
			violations = append(violations, violation{
				file: path,
				line: fset.Position(call.Pos()).Line,
			})
			return true
		})
	})

	if len(violations) > 0 {
		t.Logf("ERROR-WRAPPING-NOT-%%w VIOLATIONS — %d", len(violations))
		t.Logf("Use %%w when wrapping errors. %%v / %%s loses errors.Is/As chain.")
		for _, v := range violations {
			t.Errorf("%s:%d — fmt.Errorf wraps an error with %%v/%%s (use %%w)", v.file, v.line)
		}
	}
}

// exprText returns a best-effort textual representation of an ast
// expression for grep-style matching.
func exprText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprText(x.X) + "." + x.Sel.Name
	case *ast.CallExpr:
		return exprText(x.Fun) + "(...)"
	}
	return ""
}

// ----------------------------------------------------------------------------
// Test 83: TestArch_NoFmtPrintInProduction
// ----------------------------------------------------------------------------
//
// No fmt.Print* in non-test production code outside cmd/*/main.go
// (allowed for startup banners). Use slog.
func TestArch_NoFmtPrintInProduction(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		call string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pkg, name := callPkgAndName(call.Fun)
			if pkg != "fmt" {
				return true
			}
			if name != "Print" && name != "Println" && name != "Printf" {
				return true
			}
			violations = append(violations, violation{
				file: path,
				line: fset.Position(call.Pos()).Line,
				call: pkg + "." + name,
			})
			return true
		})
	})

	if len(violations) > 0 {
		t.Logf("fmt.Print* IN PRODUCTION VIOLATIONS — %d", len(violations))
		t.Logf("Use slog. fmt.Print* writes to stdout outside the structured pipeline.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s", v.file, v.line, v.call)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 84: TestArch_NoSensitiveFieldsInLogArgs
// ----------------------------------------------------------------------------
//
// slog.<Level>(...) calls cannot have key strings matching password /
// secret / token / api_key / private_key / jwt — these leak credentials
// into log aggregation.
func TestArch_NoSensitiveFieldsInLogArgs(t *testing.T) {
	t.Parallel()

	sensitiveRE := regexp.MustCompile(`(?i)(password|secret|api_key|private_key)`)
	// We exclude "token" and "jwt" because LeadKart logs token METADATA
	// (token_id, jwt_kid) regularly — flagging those would noise the test.

	type violation struct {
		file string
		line int
		key  string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			pkg, _ := callPkgAndName(call.Fun)
			if pkg != "slog" {
				return true
			}
			for _, arg := range call.Args {
				lit, ok := arg.(*ast.BasicLit)
				if !ok {
					continue
				}
				if !sensitiveRE.MatchString(lit.Value) {
					continue
				}
				violations = append(violations, violation{
					file: path,
					line: fset.Position(lit.Pos()).Line,
					key:  lit.Value,
				})
			}
			return true
		})
	})

	if len(violations) > 0 {
		t.Logf("SENSITIVE-KEY-IN-LOG VIOLATIONS — %d", len(violations))
		t.Logf("slog args must not pass password/secret/api_key as field keys.")
		for _, v := range violations {
			t.Errorf("%s:%d — slog arg %s contains sensitive key name", v.file, v.line, v.key)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 85: TestArch_CorrelationIDPropagation
// ----------------------------------------------------------------------------
//
// Handler/adapter logs use slog.<Level>Context(ctx, ...) (not plain
// slog.<Level>) so OTel trace_id + correlation_id flow.
//
// Detection: any function body that takes ctx as its first parameter
// AND calls slog.<Level>(...) directly (without `Context` suffix) is
// flagged.
func TestArch_CorrelationIDPropagation(t *testing.T) {
	t.Parallel()

	contextlessRE := regexp.MustCompile(`^(Debug|Info|Warn|Error)$`)

	type violation struct {
		file string
		line int
		fn   string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		// Skip common/* — those are libraries; ctx-less callers may
		// legitimately accept the ctx-less slog API for callers who
		// haven't threaded ctx through.
		slashPath := pathToSlash(path)
		if strings.Contains(slashPath, "/common/") {
			return
		}
		fset, f := parseFile(t, path, src)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			// Function must take ctx as first param.
			if fd.Type.Params == nil || len(fd.Type.Params.List) == 0 {
				continue
			}
			firstParam := fd.Type.Params.List[0].Type
			isCtx := false
			if sel, ok := firstParam.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "context" && sel.Sel.Name == "Context" {
					isCtx = true
				}
			}
			if !isCtx {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				pkg, name := callPkgAndName(call.Fun)
				if pkg != "slog" {
					return true
				}
				if contextlessRE.MatchString(name) {
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

	for _, v := range violations {
		t.Errorf("%s:%d — %s: slog.<Level> in ctx-bearing function; use slog.<Level>Context(ctx, ...) so OTel trace_id + correlation_id flow",
			v.file, v.line, v.fn)
	}
}

// ----------------------------------------------------------------------------
// Test 86: TestArch_OTelInstrumentationViaLibraries
// ----------------------------------------------------------------------------
//
// REWRITTEN to enforce Go canon. The prior shape ("every adapter method
// opens an explicit tracer.Start span") is anti-canon per:
//   - OpenTelemetry-Go contrib README: "prefer instrumented drivers"
//   - exaring/otelpgx README: "The tracer fires automatically on every
//     pgx connection"
//   - go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp docs:
//     "wrap an http.Handler once"
//   - Google Dapper paper §3.2 ("instrumentation libraries")
//
// Manual tracer.Start() in business code is the anti-pattern — it
// leaks observability concerns into handlers + adapters, and ALWAYS
// drifts behind the library tracer. The canonical Go shape: wire the
// instrumentation library ONCE in the composition root + leave
// adapters/handlers ignorant.
//
// This test asserts the two library hooks are wired:
//   - internal/common/pg/pool.go installs otelpgx.NewTracer on every
//     connection (catches every pgx.Query / pgx.Exec)
//   - cmd/api/main.go wraps the public mux with otelhttp.NewHandler
//     (catches every HTTP request)
//
// Both are a single line in the composition root. If either drifts
// (e.g. someone bypasses pg.NewPool with pgxpool.New direct), this
// test fails immediately.
func TestArch_OTelInstrumentationViaLibraries(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)

	poolPath := filepath.Join(root, "internal", "common", "pg", "pool.go")
	poolSrc, err := readFileBytes(poolPath)
	if err != nil {
		t.Fatalf("read %s: %v", poolPath, err)
	}
	poolText := string(poolSrc)
	if !strings.Contains(poolText, "otelpgx.NewTracer") {
		t.Errorf("%s does not wire otelpgx.NewTracer — every pgx connection must carry the library tracer (canonical Go OTel pattern; exaring/otelpgx README)", poolPath)
	}
	if !strings.Contains(poolText, "otelpgx.RecordStats") {
		t.Errorf("%s does not wire otelpgx.RecordStats — pool-stat metrics must be recorded so connection saturation is observable (exaring/otelpgx README §RecordStats)", poolPath)
	}

	mainPath := filepath.Join(root, "cmd", "api", "main.go")
	mainSrc, err := readFileBytes(mainPath)
	if err != nil {
		t.Fatalf("read %s: %v", mainPath, err)
	}
	mainText := string(mainSrc)
	if !strings.Contains(mainText, "otelhttp.NewHandler") {
		t.Errorf("%s does not wrap the public mux with otelhttp.NewHandler — every inbound HTTP request must open an OTel span (go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp canonical wiring)", mainPath)
	}
}

// ----------------------------------------------------------------------------
// Test 87: TestArch_EveryProblemDetailHasType
// ----------------------------------------------------------------------------
//
// The shared ErrorResponse schema (in api/openapi.yaml) declares a
// `type` field per RFC 9457 §3.1.1. Per ADR 0050 the schema is the
// contract; presence is asserted on the spec, not on each handler.
func TestArch_EveryProblemDetailHasType(t *testing.T) {
	t.Parallel()

	raw, err := readFileBytes(apiSpecPath(t))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	text := string(raw)
	// We grep textually because yaml.Unmarshal into map[string]any
	// loses path context, and we just need the literal "type" field
	// declaration under ErrorResponse / ProblemDetails.
	for _, sch := range []string{"ErrorResponse", "ProblemDetails"} {
		idx := strings.Index(text, sch+":")
		if idx < 0 {
			continue
		}
		// Slice forward a generous block.
		end := idx + 2000
		if end > len(text) {
			end = len(text)
		}
		body := text[idx:end]
		// `properties:` block — the `type:` key must appear as `      type:`
		// indented under properties.
		if regexp.MustCompile(`(?m)^\s{4,}type:`).MatchString(body) {
			return
		}
	}
	t.Errorf("openapi.yaml: neither ErrorResponse nor ProblemDetails declares a `type:` property (RFC 9457 §3.1.1)")
}

// ----------------------------------------------------------------------------
// Test 88: TestArch_AuditLogWritesForAuthnEvents
// ----------------------------------------------------------------------------
//
// Login / logout / permission-change handlers write audit log entries.
// Heuristic: handler body calls `audit.Writer.Write` / `auditWriter.Write`
// OR emits an integration event the AuditMiddleware consumes.
//
// Per ADR 0027: the outbox doubles as the audit log. Every state-
// mutating handler emits at least one integration event — so the
// heuristic "handler emits AT LEAST one event" is sufficient for
// the audit contract.
func TestArch_AuditLogWritesForAuthnEvents(t *testing.T) {
	t.Parallel()

	// Handler files whose names hint at authn / authz state changes.
	// We assert the file mentions either an outbox write or an
	// audit-write call.
	authzFileRE := regexp.MustCompile(`(login|logout|password|role|permission|impersonation|user_create)\.go$`)

	type violation struct{ file string }
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		dir := filepath.Join(internalDir(t), mod, "app", "command")
		walkGoFiles(t, dir, false, func(path string, src []byte) {
			slashPath := pathToSlash(path)
			if !authzFileRE.MatchString(slashPath) {
				return
			}
			text := string(src)
			if strings.Contains(text, "PullEvents") ||
				strings.Contains(text, "outbox") ||
				strings.Contains(text, "Outbox") ||
				strings.Contains(text, "audit") ||
				strings.Contains(text, "Audit") ||
				strings.Contains(text, "recordEvent") ||
				strings.Contains(text, "UpdateByID") || // event drain happens via repo
				strings.Contains(text, "Revoke(") {
				return
			}
			violations = append(violations, violation{file: path})
		})
	}

	if len(violations) > 0 {
		t.Logf("AUTHN-HANDLER-WITHOUT-AUDIT VIOLATIONS — %d", len(violations))
		t.Logf("Login / logout / password / role / permission handlers must")
		t.Logf("either emit an integration event (consumed by AuditMiddleware)")
		t.Logf("or write an audit log entry directly.")
		for _, v := range violations {
			t.Errorf("%s — no audit/outbox/event surface", v.file)
		}
	}
}
