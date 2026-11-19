// route_scan_test.go — the ONE route scanner behind every arch gate that reads
// HTTP registrations (scope classification, authorisation, middleware presence,
// spec parity, explicit methods).
//
// It parses with go/ast, never regex over source, and sees every non-test .go
// file under each module's ports/ (except subscribers/ and authn/, see
// routeSourceFile) plus cmd/api. A registration must have the plain form
//
//	mux.Handle("METHOD /path", gate(handler))
//
// so the scanner can read it. Anything else is an OFFENCE, not a style to be
// taught: method-less patterns, patterns held in variables or built in loops,
// registrations inside function literals (local helpers), and the default mux.
//
// Gates are recognised as identifiers in call position: an `authn.<Factory>`
// selector, or a local alias bound to an expression that contains one
// (`gate := authn.RequirePlatformGate(v, s)`, `x := chain(a, b)`). Strings,
// comments and argument names never count. The result per route is the ordered
// list of tags from the outermost wrapper to the innermost, which is what lets
// the gates check that a scope marker runs BEFORE the first auth gate.
package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Tags a wrapper contributes to a route, outermost first.
const (
	tagAuth     = "auth"     // authentication only (RequireAuth, RequireFreshStamp)
	tagAuthz    = "authz"    // a permission decision
	tagAnchor   = "anchor"   // platform-tenant membership (not authorisation)
	tagInner    = "inner"    // a guard that is neither (DenySelfTarget)
	tagReject   = "reject"   // scope marker: authn.RejectTenantScope
	tagPath     = "path"     // scope marker: path-tenant routes
	tagWritable = "writable" // scope marker: authn.TenantScopeWritable
	tagHonour   = "honour"   // scope marker: authn.HonourTenantScope
)

// authnFactoryTags maps every authn wrapper a route can use to its tags.
// RequireTenantPermission applies the path marker itself, outside its auth gate.
var authnFactoryTags = map[string][]string{
	"RequireAuth":             {tagAuth},
	"RequireFreshStamp":       {tagAuth},
	"RequirePermission":       {tagAuthz},
	"RequireAnyPermission":    {tagAuthz},
	"RequirePermissionOrSelf": {tagAuthz},
	"RequireTenantPermission": {tagPath, tagAuthz},
	"RequirePlatform":         {tagAnchor},
	"RequirePlatformGate":     {tagAnchor, tagAuthz},
	"RejectTenantScope":       {tagReject},
	"TenantScopeMatchesPath":  {tagPath},
	"TenantScopeLifecycle":    {tagPath},
	"TenantScopePathOnly":     {tagPath},
	"TenantScopeWritable":     {tagWritable},
	"HonourTenantScope":       {tagHonour},
	"DenySelfTarget":          {tagInner},
}

var routePatternRE = regexp.MustCompile(`^(GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS) /\S*$`)

// routeReg is one registration the scanner understood.
type routeReg struct {
	File    string
	Line    int
	Pattern string // "METHOD /path"
	Method  string
	Path    string
	Tags    []string // outermost wrapper first
}

// routeOffence is a registration the scanner refuses to read.
type routeOffence struct {
	File string
	Line int
	Msg  string
}

func (o routeOffence) String() string { return o.File + ":" + strconv.Itoa(o.Line) + " " + o.Msg }

func (r routeReg) has(tag string) bool { return slices.Contains(r.Tags, tag) }

// firstGate is the index of the first authentication / authorisation / anchor
// tag, or -1.
func (r routeReg) firstGate() int {
	for i, tg := range r.Tags {
		if tg == tagAuth || tg == tagAuthz || tg == tagAnchor {
			return i
		}
	}
	return -1
}

// scanRouteSource scans one Go source file. file is only used for messages.
func scanRouteSource(file string, src []byte) ([]routeReg, []routeOffence) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, []routeOffence{{file, 1, "does not parse: " + err.Error()}}
	}
	muxNames := trackedMuxNames(f)
	var routes []routeReg
	var offences []routeOffence
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		env := map[string][]string{}
		var stack []ast.Node
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			switch x := n.(type) {
			case *ast.AssignStmt:
				if len(x.Lhs) == len(x.Rhs) {
					for i, l := range x.Lhs {
						if id, ok := l.(*ast.Ident); ok {
							if tg := exprTags(x.Rhs[i], env); len(tg) > 0 {
								env[id.Name] = tg
							}
						}
					}
				}
			case *ast.ValueSpec:
				for i, id := range x.Names {
					if i < len(x.Values) {
						if tg := exprTags(x.Values[i], env); len(tg) > 0 {
							env[id.Name] = tg
						}
					}
				}
			case *ast.CallExpr:
				reg, off := registration(fset, file, x, muxNames, env, stack)
				if off != nil {
					offences = append(offences, *off)
				}
				if reg != nil {
					routes = append(routes, *reg)
				}
			}
			return true
		})
	}
	return routes, offences
}

// registration reads a Handle/HandleFunc call. Both results nil means "not a
// route registration".
func registration(fset *token.FileSet, file string, c *ast.CallExpr, muxNames map[string]bool, env map[string][]string, stack []ast.Node) (*routeReg, *routeOffence) {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "Handle" && sel.Sel.Name != "HandleFunc") {
		return nil, nil
	}
	line := fset.Position(c.Pos()).Line
	off := func(msg string) *routeOffence { return &routeOffence{file, line, msg} }

	if id, ok := sel.X.(*ast.Ident); ok && id.Name == "http" {
		return nil, off("registers on the default ServeMux (http." + sel.Sel.Name + "): register on the module's mux")
	}
	firstIsLiteral := false
	if len(c.Args) > 0 {
		if bl, ok := c.Args[0].(*ast.BasicLit); ok && bl.Kind == token.STRING {
			firstIsLiteral = true
		}
	}
	if !isTrackedMux(sel.X, muxNames) && (!firstIsLiteral || len(c.Args) != 2) {
		return nil, nil // a command/query handler's Handle(ctx, cmd), not a route
	}
	if len(c.Args) != 2 {
		return nil, off(sel.Sel.Name + " must take exactly a pattern and a handler")
	}
	if !firstIsLiteral {
		return nil, off("pattern is not a string literal (variable, loop or helper registrations are not scanned): write mux.Handle(\"METHOD /path\", gate(handler))")
	}
	pattern, err := strconv.Unquote(c.Args[0].(*ast.BasicLit).Value)
	if err != nil || !routePatternRE.MatchString(pattern) {
		return nil, off("pattern " + c.Args[0].(*ast.BasicLit).Value + " is not \"METHOD /path\" (method-less patterns match every method)")
	}
	for _, n := range stack {
		switch n.(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			return nil, off("registered inside a loop: " + pattern)
		case *ast.FuncLit:
			return nil, off("registered inside a function literal (local helper): " + pattern)
		}
	}
	method, path, _ := strings.Cut(pattern, " ")
	return &routeReg{File: file, Line: line, Pattern: pattern, Method: method, Path: path, Tags: exprTags(c.Args[1], env)}, nil
}

// exprTags returns the wrapper tags of an expression, outermost first: the
// tags of the callee, then those of every argument (a handler argument carries
// the inner wrappers; an alias argument, as in chain(a, b), carries a's and b's).
func exprTags(e ast.Expr, env map[string][]string) []string {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return exprTags(x.X, env)
	case *ast.Ident:
		return env[x.Name]
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && id.Name == "authn" {
			return authnFactoryTags[x.Sel.Name]
		}
		return nil
	case *ast.CallExpr:
		out := slices.Clone(exprTags(x.Fun, env))
		for _, a := range x.Args {
			out = append(out, exprTags(a, env)...)
		}
		return out
	}
	return nil
}

func isServeMuxType(e ast.Expr) bool {
	if st, ok := e.(*ast.StarExpr); ok {
		e = st.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "http" && sel.Sel.Name == "ServeMux"
}

func isNewServeMux(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
			id, ok := sel.X.(*ast.Ident)
			return ok && id.Name == "http" && sel.Sel.Name == "NewServeMux"
		}
	case *ast.UnaryExpr:
		if cl, ok := x.X.(*ast.CompositeLit); ok {
			return isServeMuxType(cl.Type)
		}
	}
	return false
}

// trackedMuxNames returns every identifier the file declares as a ServeMux
// (parameter, field, var, http.NewServeMux() result, or an alias of one), so a
// mux named anything is scanned, not just `mux`.
func trackedMuxNames(f *ast.File) map[string]bool {
	names := map[string]bool{"mux": true}
	for range 3 {
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Field:
				if isServeMuxType(x.Type) {
					for _, id := range x.Names {
						names[id.Name] = true
					}
				}
			case *ast.ValueSpec:
				if x.Type != nil && isServeMuxType(x.Type) {
					for _, id := range x.Names {
						names[id.Name] = true
					}
				}
				for i, v := range x.Values {
					if i < len(x.Names) && (isNewServeMux(v) || isTrackedMux(v, names)) {
						names[x.Names[i].Name] = true
					}
				}
			case *ast.AssignStmt:
				if len(x.Lhs) == len(x.Rhs) {
					for i, l := range x.Lhs {
						if id, ok := l.(*ast.Ident); ok && (isNewServeMux(x.Rhs[i]) || isTrackedMux(x.Rhs[i], names)) {
							names[id.Name] = true
						}
					}
				}
			}
			return true
		})
	}
	return names
}

func isTrackedMux(e ast.Expr, names map[string]bool) bool {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return isTrackedMux(x.X, names)
	case *ast.Ident:
		return names[x.Name]
	case *ast.SelectorExpr:
		return names[x.Sel.Name]
	}
	return false
}

// routeSourceFile reports whether a repo-relative, slash path is a file whose
// registrations the gates scan: every non-test .go file under
// internal/<module>/ports/ and cmd/api/. subscribers/ registers event handlers,
// not routes, and ports/authn/ is the middleware library whose private matching
// table (scope_policy.go) is a lookup structure, never a served mux.
func routeSourceFile(rel string) bool {
	if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
		return false
	}
	if strings.HasPrefix(rel, "cmd/api/") && strings.Count(rel, "/") == 2 {
		return true
	}
	parts := strings.Split(rel, "/")
	if len(parts) < 4 || parts[0] != "internal" || parts[2] != "ports" {
		return false
	}
	if parts[1] == "common" || parts[1] == "architecture" {
		return false
	}
	return !slices.Contains(parts[3:len(parts)-1], "subscribers") && !slices.Contains(parts[3:len(parts)-1], "authn")
}

// scanAllRoutes scans the whole repository once per call.
func scanAllRoutes(t *testing.T) ([]routeReg, []routeOffence) {
	t.Helper()
	root := repoRoot(t)
	var routes []routeReg
	var offences []routeOffence
	for _, base := range []string{"internal", "cmd/api"} {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(base)), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return rerr
			}
			rel = filepath.ToSlash(rel)
			if !routeSourceFile(rel) {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			if strings.Contains(string(src[:min(len(src), 4096)]), "//go:build") {
				return nil
			}
			r, o := scanRouteSource(rel, src)
			routes = append(routes, r...)
			offences = append(offences, o...)
			return nil
		})
		if err != nil {
			t.Fatalf("scan routes: %v", err)
		}
	}
	slices.SortFunc(routes, func(a, b routeReg) int { return strings.Compare(a.Pattern, b.Pattern) })
	return routes, offences
}

// failOnOffences reports every unreadable registration; the gates call it so
// no gate silently skips a route it cannot see.
func failOnOffences(t *testing.T, offences []routeOffence) {
	t.Helper()
	for _, o := range offences {
		t.Errorf("%s", o)
	}
}

// markersBeforeGate returns a problem for every scope marker that sits inside
// (runs after) the first auth gate: the auth gate applies the scope, so a
// marker wrapped inside it is silently ignored at runtime.
func markersBeforeGate(r routeReg) []string {
	gate := r.firstGate()
	if gate < 0 {
		return nil
	}
	var out []string
	for i, tg := range r.Tags {
		if i > gate && (tg == tagReject || tg == tagPath || tg == tagWritable || tg == tagHonour) {
			out = append(out, "scope marker ("+tg+") is wrapped INSIDE the auth gate, so it runs after the scope is applied and has no effect: wrap it outside the gate")
		}
	}
	return out
}

// routeVerdict is the combined authorisation verdict every gate shares:
// a scope marker must run before the first auth gate, and a route outside
// selfService must carry a permission decision (GETs and the IGNORE group
// included). selfService is the pinned list of routes that legitimately do not.
func routeVerdict(r routeReg, selfService map[string]string) []string {
	out := markersBeforeGate(r)
	if _, ok := selfService[r.Pattern]; !ok && !r.has(tagAuthz) {
		out = append(out, "no authorising gate (authn.RequirePermission / RequireAnyPermission / RequireTenantPermission / RequirePermissionOrSelf / RequirePlatformGate): authentication or platform membership alone is not authorisation; if the route acts only on the caller's own identity add it to selfServiceRoutes with a reason")
	}
	return out
}

// ----- Self-test: synthetic offenders the scanner must reject -----------------

func scanSynthetic(t *testing.T, file, src string) ([]routeReg, []routeOffence) {
	t.Helper()
	return scanRouteSource(file, []byte(src))
}

func TestRouteScanner_RejectsEveryHiddenRegistrationStyle(t *testing.T) {
	t.Parallel()
	const head = "package ports\nimport (\"net/http\"; \"github.com/leadkart/leadkart-go/internal/identity/ports/authn\")\n"
	offenders := []struct{ name, body string }{
		{"(b) HandleFunc with a method-less pattern", `func AddRoutes(mux *http.ServeMux) { mux.HandleFunc("/api/v1/x", h) }`},
		{"(c) method-less Handle pattern", `func AddRoutes(mux *http.ServeMux) { mux.Handle("/api/v1/x", h) }`},
		{"(e) pattern in a variable", `func AddRoutes(mux *http.ServeMux) { p := "GET /api/v1/x"; mux.Handle(p, h) }`},
		{"(e) registered in a loop", `func AddRoutes(mux *http.ServeMux) { for _, p := range ps { mux.Handle(p, h) } }`},
		{"(e) literal pattern registered in a loop", `func AddRoutes(mux *http.ServeMux) { for range 2 { mux.Handle("GET /api/v1/x", h) } }`},
		{"(f) local helper", `func AddRoutes(mux *http.ServeMux) { handle := func(p string, h http.Handler) { mux.Handle(p, h) }; handle("GET /api/v1/x", h) }`},
		{"(f) local helper with a literal inside", `func AddRoutes(mux *http.ServeMux) { reg := func() { mux.Handle("GET /api/v1/x", h) }; reg() }`},
		{"default mux", `func AddRoutes() { http.Handle("GET /api/v1/x", h) }`},
		{"default mux HandleFunc", `func AddRoutes() { http.HandleFunc("GET /api/v1/x", h) }`},
		{"not a pattern", `func AddRoutes(mux *http.ServeMux) { mux.Handle("GET api/v1/x", h) }`},
	}
	for _, tc := range offenders {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, off := scanSynthetic(t, "internal/x/ports/http.go", head+tc.body); len(off) == 0 {
				t.Fatalf("scanner accepted: %s", tc.body)
			}
		})
	}
}

// (d): a mux named anything is still scanned, and its routes are judged like any other.
func TestRouteScanner_SeesRegistrationsOnAnyReceiverName(t *testing.T) {
	t.Parallel()
	const src = `package ports
import ("net/http"; "github.com/leadkart/leadkart-go/internal/identity/ports/authn")
func AddRoutes(m *http.ServeMux, v authn.Verifier) {
	auth := authn.RequireAuth(v)
	m.Handle("GET /api/v1/x", auth(h))
}
func other() {
	router := http.NewServeMux()
	router.HandleFunc("GET /api/v1/y", h)
	alias := router
	alias.Handle("POST /api/v1/z", h)
}`
	routes, off := scanSynthetic(t, "internal/x/ports/http.go", src)
	if len(off) != 0 {
		t.Fatalf("unexpected offences: %v", off)
	}
	got := []string{}
	for _, r := range routes {
		got = append(got, r.Pattern)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"GET /api/v1/x", "GET /api/v1/y", "POST /api/v1/z"}) {
		t.Fatalf("scanned routes = %v", got)
	}
	if v := routeVerdict(routes[0], nil); len(v) == 0 {
		t.Fatal("authentication-only route on an arbitrarily named mux must be judged unauthorised")
	}
}

// (a): files named ports/http_*.go and any other ports file are scanned.
func TestRouteScanner_ScansEveryPortsFile(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{
		"internal/crm/ports/http.go", "internal/crm/ports/http_reminders.go", "internal/crm/ports/anything.go",
		"internal/identity/ports/tenant_http.go", "internal/identity/ports/sub/deep.go", "cmd/api/main.go",
	} {
		if !routeSourceFile(rel) {
			t.Errorf("%s must be scanned", rel)
		}
	}
	for _, rel := range []string{
		"internal/crm/ports/http_test.go", "internal/crm/ports/subscribers/s.go", "internal/identity/ports/authn/x.go",
		"internal/crm/app/x.go", "internal/common/obs/health.go", "cmd/api/sub/x.go", "cmd/migrate/main.go",
	} {
		if routeSourceFile(rel) {
			t.Errorf("%s must not be scanned", rel)
		}
	}
}

// (g), (h) and the two latent cases: the shared verdict must reject each.
func TestRouteScanner_VerdictRejectsUnauthorisedAndMisplacedGates(t *testing.T) {
	t.Parallel()
	const src = `package ports
import ("net/http"; "github.com/leadkart/leadkart-go/internal/identity/ports/authn")
func AddRoutes(mux *http.ServeMux, v authn.Verifier, s authn.StampValidator) {
	auth := authn.RequireFreshStamp(v, s)
	read := authn.RequirePermission(v, s, p.Orders.Read)
	pathAuth := authn.TenantScopeMatchesPath("tenantId")
	mux.Handle("GET /api/v1/tenants/{tenantId}/x", pathAuth(auth(h)))
	mux.Handle("GET /api/v1/platform/x", auth(h))
	mux.Handle("GET /api/v1/orders/marker-inside", read(authn.RejectTenantScope(h)))
	mux.Handle("GET /api/v1/orders/string-literal", auth(handle("read")))
	mux.Handle("GET /api/v1/orders/ok", authn.RejectTenantScope(read(h)))
	mux.Handle("GET /api/v1/orders/composed-ok", authn.TenantScopeWritable(read(authn.DenySelfTarget("userId")(h))))
}`
	routes, off := scanSynthetic(t, "internal/x/ports/http.go", src)
	if len(off) != 0 {
		t.Fatalf("unexpected offences: %v", off)
	}
	got := map[string][]string{}
	for _, r := range routes {
		got[r.Pattern] = routeVerdict(r, nil)
	}
	for _, p := range []string{
		"GET /api/v1/tenants/{tenantId}/x",  // (g) path-tenant GET, authentication only
		"GET /api/v1/platform/x",            // (h) IGNORE-group GET, authentication only
		"GET /api/v1/orders/marker-inside",  // latent: REJECT marker runs after the gate
		"GET /api/v1/orders/string-literal", // latent: the string "read" is not a gate
	} {
		if len(got[p]) == 0 {
			t.Errorf("%s must be rejected", p)
		}
	}
	for _, p := range []string{"GET /api/v1/orders/ok", "GET /api/v1/orders/composed-ok"} {
		if v, ok := got[p]; !ok || len(v) != 0 {
			t.Errorf("%s must pass, got %v (present=%v)", p, v, ok)
		}
	}
	// An allow-listed route needs no authorising gate, but its markers are still placed.
	selfService := map[string]string{"GET /api/v1/platform/x": "test"}
	if v := routeVerdict(routes[1], selfService); len(v) != 0 {
		t.Errorf("allow-listed route: %v", v)
	}
}
