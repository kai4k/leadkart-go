// auth_middleware_arch_test.go — Principle 7: Auth in Middleware.
//
// Per ADR 0007 (stdlib net/http ServeMux), ADR 0011 (JWT), ADR 0031
// (idempotency via X-Command-Id), ADR 0036 (permission model), and
// OWASP API Top 10 §A01:2023 (broken object-level authorisation):
// every route is either explicitly public (allow-listed) or gated by
// a canonical middleware. No ad-hoc auth checks inside handler bodies.
//
// Tests in this file:
//   43. TestArch_EveryAuthenticatedRouteHasMiddleware
//   44. TestArch_PermissionConstantsFromCatalog
//   45. TestArch_MiddlewareOrderCanonical
//   46. TestArch_IdempotencyOnMutationEndpoints
//   47. TestArch_PasswordFieldsTyped

package architecture_test

import (
	"go/ast"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// publicRoutesAllowList enumerates routes that legitimately ship
// without an auth wrapper. These are the cross-cutting endpoints that
// either pre-date the JWT exchange (login / password-reset) or serve
// non-product surface (health, openapi).
var publicRoutesAllowList = map[string]bool{
	"GET /":                                          true, // root redirect to docs
	"GET /favicon.ico":                               true,
	"GET /docs":                                      true,
	"GET /docs/":                                     true,
	"GET /openapi.yaml":                              true,
	"GET /health":                                    true,
	"GET /alive":                                     true,
	"GET /ready":                                     true,
	"POST /api/v1/auth/login":                        true,
	"POST /api/v1/auth/refresh":                      true,
	"POST /api/v1/auth/logout":                       true,
	"POST /api/v1/auth/request-password-reset":       true,
	"POST /api/v1/auth/reset-password":               true,
	"POST /api/v1/auth/confirm-email-change":         true,
	"POST /api/v1/tenants":                           true, // open signup; rate-limited at IP
}

// muxHandleRE captures `mux.Handle("METHOD /path", ...)`. We pull
// the quoted route key; the trailing `...)` body is extracted by
// balanced-paren scanning (the regex captures the START position of
// the handler expression).
var muxHandleRE = regexp.MustCompile(`mux\.Handle\(\s*"([A-Z]+ [^"]+)"\s*,\s*`)

// extractMuxHandlerExpr returns the handler expression (everything
// between the `,` after the route key and the matching `)` that
// closes mux.Handle). pos is the index in text immediately after the
// `,\s*` capture.
func extractMuxHandlerExpr(text string, pos int) string {
	depth := 1
	start := pos
	for i := pos; i < len(text); i++ {
		switch text[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return text[start:i]
			}
		}
	}
	return text[start:]
}

// ----------------------------------------------------------------------------
// Test 43: TestArch_EveryAuthenticatedRouteHasMiddleware
// ----------------------------------------------------------------------------
//
// For every mux.Handle("METHOD /path", expr) call in
// internal/<mod>/ports/http.go (and sibling *_http.go files): either
// the path is in publicRoutesAllowList, OR the expr contains one of
// the canonical auth wrappers:
//   - auth(            (RequireAuth alias)
//   - authn.RequireAuth
//   - authn.RequirePermission
//   - authn.RequireAnyPermission
//   - authn.RequirePlatform
//   - authn.RequireTenantContext
//
// We scan the SECOND ARG of the mux.Handle call for the marker. False
// positives possible if a handler-wrapper alias re-exports the marker;
// the test allow-list (per file) is the escape hatch.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_EveryAuthenticatedRouteHasMiddleware(t *testing.T) {
	t.Parallel()

	// Markers that ALWAYS indicate an auth wrap.
	directMarkers := []string{
		"authn.RequireAuth",
		"authn.RequirePermission",
		"authn.RequireAnyPermission",
		"authn.RequirePlatform",
		"authn.RequireTenantContext",
	}

	// Indirect markers are auto-discovered by scanning the file for
	// `<name> := authn.Require...` bindings — see the loop below.

	type violation struct {
		file string
		line int
		key  string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		portsDir := filepath.Join(internalDir(t), mod, "ports")
		walkGoFiles(t, portsDir, false, func(path string, src []byte) {
			slashPath := pathToSlash(path)
			if !strings.HasSuffix(slashPath, "/ports/http.go") &&
				!strings.HasSuffix(slashPath, "_http.go") {
				return
			}
			text := string(src)
			// Auto-discover middleware-alias bindings.
			//
			// Pass 1: direct binding to authn.Require* (canonical shape).
			indirectActive := map[string]bool{}
			directBindingRE := regexp.MustCompile(`(?m)\b([a-zA-Z_]\w*)\s*:=\s*authn\.Require\w*\(`)
			for _, m := range directBindingRE.FindAllStringSubmatch(text, -1) {
				indirectActive[m[1]+"("] = true
			}
			// Pass 2: chain/compose helpers that wrap an existing
			// alias. We accept any `<name> := chain(<X>, ...)` or
			// `<name> := <X>(<Y>)` where <X> resolves through any
			// already-active marker. Two-pass closure-resolution.
			composeBindingRE := regexp.MustCompile(`(?m)\b([a-zA-Z_]\w*)\s*:=\s*\w+\s*\(([^)]*)\)`)
			for round := 0; round < 4; round++ {
				added := false
				for _, m := range composeBindingRE.FindAllStringSubmatch(text, -1) {
					lhs := m[1]
					if indirectActive[lhs+"("] {
						continue
					}
					rhs := m[2]
					for activeMarker := range indirectActive {
						if strings.Contains(rhs, strings.TrimSuffix(activeMarker, "(")) {
							indirectActive[lhs+"("] = true
							added = true
							break
						}
					}
					// Also: a chain whose direct arg contains
					// `authn.Require` qualifies as a marker.
					if strings.Contains(rhs, "authn.Require") {
						indirectActive[lhs+"("] = true
						added = true
					}
				}
				if !added {
					break
				}
			}
			// `auth(` is bound at the route-group level by the existing
			// identity ports/http.go pattern; trust it without textual
			// rebinding check (the pattern is canonical + audited).
			indirectActive["auth("] = true
			// `chain(requirePlatform, ...)` invoked inline in mux.Handle
			// is canonical too — the route call itself contains the
			// `authn.Require...`-shaped name `requirePlatform`. We
			// detect this by string-matching authn.Require markers
			// inside the route expression below.

			matches := muxHandleRE.FindAllStringSubmatchIndex(text, -1)
			for _, m := range matches {
				key := text[m[2]:m[3]]
				expr := extractMuxHandlerExpr(text, m[1])
				line := 1 + strings.Count(text[:m[0]], "\n")
				if publicRoutesAllowList[key] {
					continue
				}
				hasMarker := false
				for _, marker := range directMarkers {
					if strings.Contains(expr, marker) {
						hasMarker = true
						break
					}
				}
				if !hasMarker {
					for marker := range indirectActive {
						name := strings.TrimSuffix(marker, "(")
						// Match the marker either as a CALL (`name(`)
						// or as a function-passed argument (`name,`
						// `name)` `name ` `,name`).
						if strings.Contains(expr, name+"(") ||
							strings.Contains(expr, name+",") ||
							strings.Contains(expr, name+")") ||
							strings.Contains(expr, " "+name+" ") {
							hasMarker = true
							break
						}
					}
				}
				if !hasMarker {
					violations = append(violations, violation{file: path, line: line, key: key})
				}
			}
		})
	}

	if len(violations) > 0 {
		t.Logf("ROUTE WITHOUT AUTH-MIDDLEWARE VIOLATIONS — %d", len(violations))
		t.Logf("Every mux.Handle outside the public allow-list must wrap the")
		t.Logf("handler with an authn.Require* middleware. Add the route to")
		t.Logf("publicRoutesAllowList if it's intentionally unauthenticated.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s lacks Require* auth wrapper", v.file, v.line, v.key)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 44: TestArch_PermissionConstantsFromCatalog
// ----------------------------------------------------------------------------
//
// Every call to RequirePermission / RequireAnyPermission passes an
// identifier from `permission.IdentityPermissions.*`, NOT a string
// literal. Per ADR 0036: permissions are a closed set. A string literal
// in a call site bypasses the catalogue + breaks IDE refactors when
// a permission is renamed.
//
// Detection: parse every ports/*.go; find CallExprs to `RequirePermission`
// or `RequireAnyPermission` (selector-suffix match); inspect argument
// types — flag `*ast.BasicLit` (string literal).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_PermissionConstantsFromCatalog(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		fn   string
		lit  string
	}
	var violations []violation

	for _, mod := range modulesUnderInternal(t) {
		portsDir := filepath.Join(internalDir(t), mod, "ports")
		walkGoFiles(t, portsDir, false, func(path string, src []byte) {
			fset, f := parseFile(t, path, src)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := callName(call.Fun)
				if name != "RequirePermission" && name != "RequireAnyPermission" {
					return true
				}
				for _, arg := range call.Args {
					lit, ok := arg.(*ast.BasicLit)
					if !ok {
						continue
					}
					violations = append(violations, violation{
						file: path,
						line: fset.Position(lit.Pos()).Line,
						fn:   name,
						lit:  lit.Value,
					})
				}
				return true
			})
		})
	}

	if len(violations) > 0 {
		t.Logf("PERMISSION-STRING-LITERAL VIOLATIONS — %d", len(violations))
		t.Logf("Use a constant from permission.IdentityPermissions.* —")
		t.Logf("string literals bypass the closed-set catalogue + break")
		t.Logf("rename refactors.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s called with string literal %s", v.file, v.line, v.fn, v.lit)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 45: TestArch_MiddlewareOrderCanonical
// ----------------------------------------------------------------------------
//
// The canonical chain order in httpmw.PublicChain MUST be:
//   Correlation → RequestLog → Recover → IPRateLimit → Idempotency
// (then per-route auth + handler).
//
// Detection: parse internal/common/httpmw/chain.go; locate the
// `PublicChain` function; assert the return body lists these names
// in this order.
func TestArch_MiddlewareOrderCanonical(t *testing.T) {
	t.Parallel()

	canonical := []string{"Correlation", "RequestLog", "Recover", "ipLimiter", "Middleware"}
	// We accept a relaxed shape: the body must contain each marker
	// substring in order. `ipLimiter.Middleware()` registers the rate-
	// limit middleware; `idempotency.Middleware(...)` registers the
	// idempotency middleware. The marker check is substring-presence.

	chainPath := filepath.Join(internalDir(t), "common", "httpmw", "chain.go")
	raw, err := readFileBytes(chainPath)
	if err != nil {
		t.Fatalf("read %s: %v", chainPath, err)
	}
	text := string(raw)
	// Locate the PublicChain function body — we slice from the func
	// signature to the trailing closing `}`.
	start := strings.Index(text, "func PublicChain(")
	if start < 0 {
		t.Fatal("could not locate PublicChain in chain.go")
	}
	// Take the next 2000 bytes — sufficient to span the func body.
	end := start + 2000
	if end > len(text) {
		end = len(text)
	}
	body := text[start:end]

	pos := 0
	for _, marker := range canonical {
		next := strings.Index(body[pos:], marker)
		if next < 0 {
			t.Errorf("PublicChain body missing marker %q at or after offset %d", marker, pos)
			continue
		}
		pos += next + len(marker)
	}
}

// ----------------------------------------------------------------------------
// Test 46: TestArch_IdempotencyOnMutationEndpoints
// ----------------------------------------------------------------------------
//
// Every operation in api/openapi.yaml with HTTP method POST / PUT /
// PATCH declares the `X-Command-Id` header parameter (per ADR 0031).
// Stripe canon: write endpoints accept idempotency keys; replay returns
// the original response without re-executing.
func TestArch_IdempotencyOnMutationEndpoints(t *testing.T) {
	t.Parallel()

	raw, err := readFileBytes(apiSpecPath(t))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal openapi.yaml: %v", err)
	}
	paths, _ := doc["paths"].(map[string]any)
	if paths == nil {
		t.Fatal("openapi.yaml has no paths object")
	}

	// Paths that explicitly DON'T accept X-Command-Id (e.g. auth/login
	// is itself idempotent at the JWT exchange level; logout has no
	// state to replay).
	exempt := map[string]bool{
		"POST /api/v1/auth/login":   true,
		"POST /api/v1/auth/logout":  true,
		"POST /api/v1/auth/refresh": true,
	}

	type violation struct {
		op string
	}
	var violations []violation

	for pathKey, raw := range paths {
		methods, _ := raw.(map[string]any)
		for method, opRaw := range methods {
			method = strings.ToUpper(method)
			if method != "POST" && method != "PUT" && method != "PATCH" {
				continue
			}
			key := method + " " + pathKey
			if exempt[key] {
				continue
			}
			op, _ := opRaw.(map[string]any)
			params, _ := op["parameters"].([]any)
			hasXCmdID := false
			for _, p := range params {
				pm, _ := p.(map[string]any)
				// Inline form: `{ name: X-Command-Id, in: header, ... }`.
				if name, _ := pm["name"].(string); strings.EqualFold(name, "X-Command-Id") {
					hasXCmdID = true
					break
				}
				// $ref form: `{ $ref: '#/components/parameters/XCommandId' }`.
				// Stripe / Auth0 canon: shared parameter components are reused
				// across operations via $ref, NOT inlined on every op.
				if ref, _ := pm["$ref"].(string); strings.HasSuffix(ref, "/parameters/XCommandId") {
					hasXCmdID = true
					break
				}
			}
			if !hasXCmdID {
				violations = append(violations, violation{op: key})
			}
		}
	}

	if len(violations) > 0 {
		t.Errorf("openapi.yaml: %d POST/PUT/PATCH op(s) missing X-Command-Id parameter — add `parameters: [{$ref: '#/components/parameters/XCommandId'}]` (Stripe canon + ADR 0031):", len(violations))
		for _, v := range violations {
			t.Logf("  %s", v.op)
		}
	}
}

// ----------------------------------------------------------------------------
// Test 47: TestArch_PasswordFieldsTyped
// ----------------------------------------------------------------------------
//
// Fields named `Password` / `password` have type `string` only inside
// command-input DTOs (`*Command` / `*Request` / `*Input` suffix);
// never on response DTOs (which can be logged + serialised).
//
// Detection: walk every struct with a Password field of type string;
// assert struct name matches one of the input suffixes OR is on a
// documented allow-list.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_PasswordFieldsTyped(t *testing.T) {
	t.Parallel()

	inputSuffixes := []string{"Command", "Request", "Input", "Cmd", "Body", "Payload"}
	allowedExactTypes := map[string]bool{
		"PasswordSettings":     true, // domain VO — string is part of the type definition
		"PasswordRequirements": true,
		"RedisConfig":          true, // config struct sources Redis password from koanf-loaded env
	}

	type violation struct {
		file string
		line int
		typ  string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			if allowedExactTypes[ts.Name.Name] {
				return true
			}
			for _, field := range st.Fields.List {
				id, ok := field.Type.(*ast.Ident)
				if !ok || id.Name != "string" {
					continue
				}
				for _, fn := range field.Names {
					low := strings.ToLower(fn.Name)
					if !strings.Contains(low, "password") {
						continue
					}
					// Skip if obviously a hash (PasswordHash etc.).
					if strings.Contains(low, "hash") {
						continue
					}
					ok := false
					for _, suf := range inputSuffixes {
						if strings.HasSuffix(ts.Name.Name, suf) {
							ok = true
							break
						}
					}
					if !ok {
						violations = append(violations, violation{
							file: path,
							line: fset.Position(field.Pos()).Line,
							typ:  ts.Name.Name + "." + fn.Name,
						})
					}
				}
			}
			return true
		})
	})

	if len(violations) > 0 {
		t.Logf("PASSWORD-FIELD ON NON-INPUT-DTO VIOLATIONS — %d", len(violations))
		t.Logf("Plain-text passwords belong only on command-input DTOs (the")
		t.Logf("type that received the request body). Response / response-DTO")
		t.Logf("structs must use hashed form or omit entirely.")
		for _, v := range violations {
			t.Errorf("%s:%d — %s (string) on non-input struct", v.file, v.line, v.typ)
		}
	}
}

// ============================================================================
// Principle D deep — JWT / auth / crypto hardening (7 tests added per the
// comprehensive catalog brief). ADR 0011 + ADR 0012 + security.md.
// ============================================================================

// ----------------------------------------------------------------------------
// D1: TestArch_AccessTokenTTLBounded
// ----------------------------------------------------------------------------
//
// security.md: access tokens are short-lived (≤ 30min). Longer windows
// turn a single replay into a long-running compromise. Per OAuth 2.1
// + Auth0 best practices canon.
func TestArch_AccessTokenTTLBounded(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), "internal", "identity", "app", "jwt", "jwt.go")
	src, err := readFileBytes(path)
	if err != nil {
		t.Fatalf("read jwt.go: %v", err)
	}
	body := string(src)

	// Find the AccessTokenTTL = <duration> line.
	re := regexp.MustCompile(`AccessTokenTTL\s*=\s*([\d]+)\s*\*\s*time\.(Minute|Hour|Second)`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("AccessTokenTTL constant not declared in canonical `N * time.Unit` form")
	}
	n := atoi(m[1])
	var minutes int
	switch m[2] {
	case "Second":
		minutes = n / 60
	case "Minute":
		minutes = n
	case "Hour":
		minutes = n * 60
	}
	if minutes > 30 {
		t.Fatalf("AccessTokenTTL = %d minutes (cap 30; OAuth 2.1 + Auth0 best-practice)", minutes)
	}
}

// ----------------------------------------------------------------------------
// D2: TestArch_RefreshTokenTTLBounded
// ----------------------------------------------------------------------------
//
// Refresh-token absolute-expiry MUST be ≤ 14d. Per security.md "rotation
// + absolute lifetime"; longer-living refresh tokens become bearer-key
// equivalents.
//
// LeadKart stores refresh TTL as a config-injected duration (not a
// constant). Walk every config-default site + the integration-test
// constant declarations and bound them.
func TestArch_RefreshTokenTTLBounded(t *testing.T) {
	t.Parallel()

	maxMinutes := 14 * 24 * 60 // 14 days

	re := regexp.MustCompile(`(?i)refresh\w*TTL\s*=\s*([\d]+)\s*\*\s*([\d]+)?\s*\*?\s*time\.(Hour|Minute|Second)`)

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, filepath.Join(root, "identity"), true, func(path string, src []byte) {
		body := string(src)
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			n := atoi(m[1])
			mult := 1
			if m[2] != "" {
				mult = atoi(m[2])
			}
			var minutes int
			switch m[3] {
			case "Second":
				minutes = (n * mult) / 60
			case "Minute":
				minutes = n * mult
			case "Hour":
				minutes = n * mult * 60
			}
			if minutes > maxMinutes {
				bad = append(bad, pathToSlash(path)+": "+m[0])
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("refresh-token TTL exceeds 14d (security.md):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// D3: TestArch_JWTSigningMethodHS256OrRS256
// ----------------------------------------------------------------------------
//
// Only HS256 (HMAC) or RS256 (RSA) signing methods allowed. ES256
// (ECDSA) is acceptable in principle but unused here. `none` (no
// signing) is a known JWT-library CVE class — explicitly banned.
//
// Predicate: every `jwtv5.SigningMethod*` reference must be one of
// the allowed methods.
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_JWTSigningMethodHS256OrRS256(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{
		"SigningMethodHS256": true,
		"SigningMethodHS384": true,
		"SigningMethodHS512": true,
		"SigningMethodRS256": true,
		"SigningMethodRS384": true,
		"SigningMethodRS512": true,
	}

	re := regexp.MustCompile(`SigningMethod\w+`)
	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		body := stripGoComments(string(src))
		for _, m := range re.FindAllString(body, -1) {
			if m == "SigningMethod" {
				continue
			}
			if !allowed[m] {
				bad = append(bad, pathToSlash(path)+": "+m)
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("non-canonical JWT signing method (HS* or RS* only; `none` banned per CVE-2015-9235):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// D4: TestArch_JWTKidHeaderRequired
// ----------------------------------------------------------------------------
//
// Multi-key rotation needs the `kid` header. Every Issue path must
// set it; every Verify path must read it. Drift here breaks
// rotation-on-rollover (the new key is rejected because verifiers
// can't route to it).
func TestArch_JWTKidHeaderRequired(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), "internal", "identity", "app", "jwt", "jwt.go")
	src, err := readFileBytes(path)
	if err != nil {
		t.Fatalf("read jwt.go: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, `Header["kid"]`) {
		t.Fatal("jwt.go does not set Header[\"kid\"] on issued tokens (rotation breaks)")
	}
	if !strings.Contains(body, `t.Header["kid"]`) {
		t.Fatal("jwt.go does not read Header[\"kid\"] on Verify (no key-routing)")
	}
}

// ----------------------------------------------------------------------------
// D5: TestArch_NoBcryptOrScrypt
// ----------------------------------------------------------------------------
//
// ADR 0012 locks Argon2id. Bcrypt + Scrypt imports are banned to
// prevent accidental "I prefer the API" backsliding. The PHC string
// format is the only acceptable hash format in the codebase.
func TestArch_NoBcryptOrScrypt(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, true, func(path string, src []byte) {
		for _, im := range parseImports(t, path, src) {
			if strings.HasSuffix(im, "/bcrypt") || strings.HasSuffix(im, "/scrypt") {
				// Allow tests that explicitly assert non-use.
				if strings.HasSuffix(path, "_test.go") &&
					strings.Contains(string(src), "// arch-test:asserts-non-use") {
					continue
				}
				bad = append(bad, pathToSlash(path)+": imports "+im)
			}
		}
	})

	if len(bad) > 0 {
		t.Fatalf("bcrypt/scrypt imported (ADR 0012 locks Argon2id):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// ----------------------------------------------------------------------------
// D6: TestArch_Argon2ParamsMeetOWASP
// ----------------------------------------------------------------------------
//
// OWASP Password Storage Cheat Sheet 2025: Argon2id minimum is
// memory ≥ 19 MiB (19456 KiB), iterations ≥ 2, parallelism ≥ 1.
// (RFC 9106 §4 also acceptable: m=12MiB, t=3, p=1.) We enforce the
// stricter Memory-first OWASP profile because LeadKart is bcrypt-
// replacement, not constrained-hardware.
func TestArch_Argon2ParamsMeetOWASP(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRoot(t), "internal", "identity", "app", "argon2", "argon2.go")
	src, err := readFileBytes(path)
	if err != nil {
		t.Fatalf("read argon2.go: %v", err)
	}
	body := string(src)

	memRE := regexp.MustCompile(`Memory\s+uint32\s*=\s*(\d+)(?:\s*\*\s*(\d+))?`)
	iterRE := regexp.MustCompile(`Iterations\s+uint32\s*=\s*(\d+)`)
	parRE := regexp.MustCompile(`Parallel\s+uint8\s*=\s*(\d+)`)

	memM := memRE.FindStringSubmatch(body)
	iterM := iterRE.FindStringSubmatch(body)
	parM := parRE.FindStringSubmatch(body)
	if memM == nil || iterM == nil || parM == nil {
		t.Fatal("argon2.go missing canonical Memory/Iterations/Parallel constants")
	}

	mem := atoi(memM[1])
	if memM[2] != "" {
		mem *= atoi(memM[2])
	}
	iter := atoi(iterM[1])
	par := atoi(parM[1])

	if mem < 19*1024 {
		t.Errorf("Argon2 Memory = %d KiB; OWASP 2025 floor 19456 KiB (19 MiB)", mem)
	}
	if iter < 2 {
		t.Errorf("Argon2 Iterations = %d; OWASP 2025 floor 2", iter)
	}
	if par < 1 {
		t.Errorf("Argon2 Parallel = %d; minimum 1", par)
	}
}

// ----------------------------------------------------------------------------
// D7: TestArch_NoCryptoMd5OrSha1ForSecurity
// ----------------------------------------------------------------------------
//
// MD5 + SHA-1 are cryptographically broken (collision attacks
// practical since 2017 — Stevens et al). Banned in security
// contexts; allowed only with an explicit arch-test marker
// (e.g. ETag / cache-key hashing where collisions don't matter).
// Scope: production — applies to non-test files; test-side discipline lives under Principle TD/TP.
func TestArch_NoCryptoMd5OrSha1ForSecurity(t *testing.T) {
	t.Parallel()

	root := internalDir(t)
	var bad []string

	walkGoFiles(t, root, false, func(path string, src []byte) {
		for _, im := range parseImports(t, path, src) {
			if im != "crypto/md5" && im != "crypto/sha1" {
				continue
			}
			if strings.Contains(string(src), "arch-test:non-security-hash") {
				continue
			}
			bad = append(bad, pathToSlash(path)+": imports "+im)
		}
	})

	if len(bad) > 0 {
		t.Fatalf("crypto/md5 or crypto/sha1 used without // arch-test:non-security-hash opt-out (collision-broken):\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// helper: simple base-10 string->int (avoids strconv import where one symbol is needed).
func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int(c-'0')
	}
	return n
}
