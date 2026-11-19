// route_scan_test.go — the ONE route scanner behind every arch gate that reads
// HTTP registrations (scope classification, authorisation, middleware presence,
// spec parity, explicit methods).
//
// It parses and TYPE-CHECKS with go/parser + go/types, never regex over source.
// go/types runs per package with a stub net/http (so *http.ServeMux, its Handle
// and HandleFunc methods and the package-level http.Handle resolve to real
// objects) and every other import faked by path (so the authn package is
// identified by its import path, and an identifier that merely is NAMED authn
// is not it). Unresolved expressions are tolerated: the scanner only needs
// objects, not full types. golang.org/x/tools is deliberately not used: this
// needs no export data and no `go list`, so it is fast and offline.
//
// Reading a route. A registration must have the plain form
//
//	mux.Handle("METHOD /path", gate(handler))
//
// so the scanner can read it. Anything else is an OFFENCE, not a style to be
// taught: method-less patterns, patterns held in variables or built in loops,
// registrations inside function literals (local helpers, package-level
// `var _ = func(){...}()`), the default mux, and any (*http.ServeMux).Handle /
// HandleFunc or http.Handle / http.HandleFunc that is bound without being called
// (method values and method expressions).
//
// Gates. A route's tags come ONLY from the callee chain and the LAST argument
// (the handler) of each call, so a gate passed as an ignored argument counts for
// nothing. A gate is an `<authn>.<Factory>` selector whose qualifier resolves to
// the authn import path, or a variable bound once to such an expression. A
// variable that is written anywhere else (assignment, :=, ++, &x) is dropped, and
// variables are keyed by their object, so block shadowing cannot launder a gate.
//
// Surface. Every non-test .go file of the repository is scanned, once. Files
// under internal/<module>/ports/ and cmd/api/ are the routes the gates judge. A
// registration anywhere else is a failure unless the file is in
// pinnedRegistrationFiles with its exact registrations and a reason. Files whose
// //go:build constraint can never hold in a production build (integration,
// ignore, any custom tag) are skipped; every other constraint, including
// `go1.1` or `linux`, is scanned.
package architecture_test

import (
	"bytes"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// authnImportPath identifies the authn package; a qualifier counts only when it
// resolves to this path.
const authnImportPath = "github.com/leadkart/leadkart-go/internal/identity/ports/authn"

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

// ----- go/types plumbing ------------------------------------------------------

// stubNetHTTP is the part of net/http the scanner needs to resolve ServeMux,
// its Handle/HandleFunc methods and the default-mux functions to real objects.
const stubNetHTTP = `package http
type ResponseWriter interface{}
type Request struct{}
type Handler interface{ ServeHTTP(ResponseWriter, *Request) }
type HandlerFunc func(ResponseWriter, *Request)
func (HandlerFunc) ServeHTTP(ResponseWriter, *Request) {}
type ServeMux struct{}
func NewServeMux() *ServeMux { return nil }
func (*ServeMux) Handle(pattern string, handler Handler) {}
func (*ServeMux) HandleFunc(pattern string, handler func(ResponseWriter, *Request)) {}
func Handle(pattern string, handler Handler) {}
func HandleFunc(pattern string, handler func(ResponseWriter, *Request)) {}
`

var (
	stubOnce sync.Once
	stubPkg  *types.Package
)

func netHTTPStub() *types.Package {
	stubOnce.Do(func() {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "stub_net_http.go", stubNetHTTP, 0)
		if err != nil {
			panic(err)
		}
		stubPkg, err = (&types.Config{}).Check("net/http", fset, []*ast.File{f}, nil)
		if err != nil {
			panic(err)
		}
	})
	return stubPkg
}

// scanImporter resolves net/http to the stub and fakes every other package by
// path, so a PkgName still carries the import path.
type scanImporter struct{}

func (scanImporter) Import(p string) (*types.Package, error) {
	if p == "net/http" {
		return netHTTPStub(), nil
	}
	pkg := types.NewPackage(p, path.Base(p))
	pkg.MarkComplete()
	return pkg, nil
}

// pkgScan is one type-checked package.
type pkgScan struct {
	fset    *token.FileSet
	info    *types.Info
	defs    map[types.Object]ast.Expr // variable -> its single defining expression
	written map[types.Object]bool     // variable written anywhere other than its definition
}

func newPkgScan(fset *token.FileSet, files []*ast.File) *pkgScan {
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Implicits:  map[ast.Node]types.Object{},
	}
	conf := types.Config{Importer: scanImporter{}, Error: func(error) {}, FakeImportC: true}
	_, _ = conf.Check("scanned", fset, files, info) // errors (unresolved imports) are expected and ignored
	s := &pkgScan{fset: fset, info: info, defs: map[types.Object]ast.Expr{}, written: map[types.Object]bool{}}
	for _, f := range files {
		s.collectWrites(f)
	}
	return s
}

func (s *pkgScan) objOf(e ast.Expr) types.Object {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return nil
	}
	if o := s.info.Uses[id]; o != nil {
		return o
	}
	return s.info.Defs[id]
}

// collectWrites records each variable's single definition and every other write
// to it (assignment, redeclaring :=, op-assign, ++/--, range with =, &x).
func (s *pkgScan) collectWrites(f *ast.File) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, l := range x.Lhs {
				id, ok := ast.Unparen(l).(*ast.Ident)
				if !ok {
					continue
				}
				if x.Tok == token.DEFINE {
					if o := s.info.Defs[id]; o != nil {
						if len(x.Lhs) == len(x.Rhs) {
							s.defs[o] = x.Rhs[i]
						}
						continue
					}
				}
				if o := s.info.Uses[id]; o != nil {
					s.written[o] = true
				}
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				if o := s.info.Defs[id]; o != nil && len(x.Names) == len(x.Values) {
					s.defs[o] = x.Values[i]
				}
			}
		case *ast.IncDecStmt:
			if o := s.objOf(x.X); o != nil {
				s.written[o] = true
			}
		case *ast.RangeStmt:
			if x.Tok == token.ASSIGN {
				for _, e := range []ast.Expr{x.Key, x.Value} {
					if e != nil {
						if o := s.objOf(e); o != nil {
							s.written[o] = true
						}
					}
				}
			}
		case *ast.UnaryExpr:
			if x.Op == token.AND {
				if o := s.objOf(x.X); o != nil {
					s.written[o] = true
				}
			}
		}
		return true
	})
}

// tags returns the wrapper tags of an expression, outermost first: those of the
// callee chain, then those of the LAST argument only (the handler). A variable
// contributes its tags only if it was bound exactly once and never written again.
func (s *pkgScan) tags(e ast.Expr, seen map[types.Object]bool) []string {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return s.tags(x.X, seen)
	case *ast.Ident:
		o := s.info.Uses[x]
		v, ok := o.(*types.Var)
		if !ok || s.written[o] || seen[o] {
			return nil
		}
		def, ok := s.defs[v]
		if !ok {
			return nil
		}
		seen[o] = true
		defer delete(seen, o)
		return s.tags(def, seen)
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok {
			return nil
		}
		if pn, ok := s.info.Uses[id].(*types.PkgName); ok && pn.Imported().Path() == authnImportPath {
			return authnFactoryTags[x.Sel.Name]
		}
		return nil
	case *ast.CallExpr:
		out := slices.Clone(s.tags(x.Fun, seen))
		if len(x.Args) > 0 {
			out = append(out, s.tags(x.Args[len(x.Args)-1], seen)...)
		}
		return out
	}
	return nil
}

// ----- recognising registrations ----------------------------------------------

// netHTTPHandleFunc classifies an object as http.Handle / http.HandleFunc
// (method=false) or (*http.ServeMux).Handle / HandleFunc (method=true).
func netHTTPHandleFunc(o types.Object) (fn *types.Func, method, ok bool) {
	fn, ok = o.(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "net/http" || (fn.Name() != "Handle" && fn.Name() != "HandleFunc") {
		return nil, false, false
	}
	sig, _ := fn.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil {
		return fn, false, true
	}
	rt := sig.Recv().Type()
	if p, isPtr := rt.(*types.Pointer); isPtr {
		rt = p.Elem()
	}
	if n, isNamed := rt.(*types.Named); isNamed && n.Obj().Name() == "ServeMux" {
		return fn, true, true
	}
	return nil, false, false
}

func isStringType(t types.Type) bool {
	if t == nil {
		return false
	}
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

// calleeObject returns the object a function expression names, if resolved.
func (s *pkgScan) calleeObject(fun ast.Expr) types.Object {
	switch f := ast.Unparen(fun).(type) {
	case *ast.SelectorExpr:
		if sel := s.info.Selections[f]; sel != nil {
			return sel.Obj()
		}
		return s.info.Uses[f.Sel]
	case *ast.Ident:
		return s.info.Uses[f]
	}
	return nil
}

func funName(fun ast.Expr) string {
	switch f := ast.Unparen(fun).(type) {
	case *ast.SelectorExpr:
		return f.Sel.Name
	case *ast.Ident:
		return f.Name
	}
	return ""
}

// parentOf returns the node enclosing the last one on the stack.
func parentOf(stack []ast.Node) ast.Node {
	if len(stack) < 2 {
		return nil
	}
	return stack[len(stack)-2]
}

// scanFile reads every registration of one parsed file.
func (s *pkgScan) scanFile(file string, f *ast.File) ([]routeReg, []routeOffence) {
	var routes []routeReg
	var offences []routeOffence

	// Expressions that are the function of a call (the rest are bound values).
	callFuns := map[ast.Node]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			callFuns[ast.Unparen(c.Fun)] = true
		}
		return true
	})

	var stack []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		line := s.fset.Position(n.Pos()).Line
		off := func(msg string) { offences = append(offences, routeOffence{file, line, msg}) }

		switch x := n.(type) {
		case *ast.SelectorExpr:
			if !callFuns[x] {
				if _, _, ok := netHTTPHandleFunc(s.calleeObject(x)); ok {
					off("(*http.ServeMux).Handle/HandleFunc or http.Handle/HandleFunc used as a value or method expression, not called: write mux.Handle(\"METHOD /path\", ...) directly")
				}
			}
		case *ast.Ident:
			if parent, ok := parentOf(stack).(*ast.SelectorExpr); ok && parent.Sel == x {
				return true // the selector itself is judged above
			}
			if !callFuns[x] {
				if _, _, ok := netHTTPHandleFunc(s.info.Uses[x]); ok {
					off("http.Handle/HandleFunc used as a value, not called")
				}
			}
		case *ast.CallExpr:
			name := funName(x.Fun)
			if name != "Handle" && name != "HandleFunc" {
				return true
			}
			fn, onMux, isHTTP := netHTTPHandleFunc(s.calleeObject(x.Fun))
			if isHTTP && !onMux {
				off("registers on the default ServeMux (http." + fn.Name() + "): register on the module's mux")
				return true
			}
			if !isHTTP && !s.registrationLike(x) {
				return true // a command/query handler's Handle(ctx, cmd), not a route
			}
			reg, msg := s.readRegistration(file, line, x, stack)
			if msg != "" {
				off(msg)
			}
			if reg != nil {
				routes = append(routes, *reg)
			}
		}
		return true
	})
	return routes, offences
}

// registrationLike decides whether a Handle/HandleFunc call that does not
// resolve to net/http is a route registration: its first parameter or argument
// is a string (an interface or wrapper type with Handle(pattern string, h)).
func (s *pkgScan) registrationLike(c *ast.CallExpr) bool {
	if len(c.Args) == 0 {
		return false
	}
	if fn, ok := s.calleeObject(c.Fun).(*types.Func); ok {
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Params().Len() > 0 && isStringType(sig.Params().At(0).Type()) {
			return true
		}
	}
	if bl, ok := c.Args[0].(*ast.BasicLit); ok && bl.Kind == token.STRING {
		return true
	}
	if tv, ok := s.info.Types[c.Args[0]]; ok && isStringType(tv.Type) {
		return true
	}
	return false
}

// readRegistration applies the plain-form rules to one registration call.
func (s *pkgScan) readRegistration(file string, line int, c *ast.CallExpr, stack []ast.Node) (*routeReg, string) {
	if len(c.Args) != 2 {
		return nil, funName(c.Fun) + " must take exactly a pattern and a handler"
	}
	bl, ok := c.Args[0].(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return nil, "pattern is not a string literal (variable, loop or helper registrations are not scanned): write mux.Handle(\"METHOD /path\", gate(handler))"
	}
	pattern, err := strconv.Unquote(bl.Value)
	if err != nil || !routePatternRE.MatchString(pattern) {
		return nil, "pattern " + bl.Value + " is not \"METHOD /path\" (method-less patterns match every method)"
	}
	for _, n := range stack {
		switch n.(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			return nil, "registered inside a loop: " + pattern
		case *ast.FuncLit:
			return nil, "registered inside a function literal (local helper): " + pattern
		}
	}
	method, p, _ := strings.Cut(pattern, " ")
	return &routeReg{File: file, Line: line, Pattern: pattern, Method: method, Path: p, Tags: s.tags(c.Args[1], map[types.Object]bool{})}, ""
}

// ----- scanning files, packages and the repository ----------------------------

// fileScan is what one file registers.
type fileScan struct {
	routes   []routeReg
	offences []routeOffence
}

// scanPackageFiles type-checks the files of ONE package (rel path -> source) and
// reads every registration per file.
func scanPackageFiles(files map[string][]byte) map[string]fileScan {
	out := map[string]fileScan{}
	fset := token.NewFileSet()
	var parsed []*ast.File
	names := map[*ast.File]string{}
	for _, rel := range sortedKeys(files) {
		f, err := parser.ParseFile(fset, rel, files[rel], parser.SkipObjectResolution)
		if err != nil {
			out[rel] = fileScan{offences: []routeOffence{{rel, 1, "does not parse: " + err.Error()}}}
			continue
		}
		parsed = append(parsed, f)
		names[f] = rel
	}
	if len(parsed) == 0 {
		return out
	}
	s := newPkgScan(fset, parsed)
	for _, f := range parsed {
		r, o := s.scanFile(names[f], f)
		out[names[f]] = fileScan{routes: r, offences: o}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// scanRouteSource scans one file on its own (self-tests). file is only used for
// messages.
func scanRouteSource(file string, src []byte) ([]routeReg, []routeOffence) {
	r := scanPackageFiles(map[string][]byte{file: src})[file]
	return r.routes, r.offences
}

// buildableInProduction reports whether a file's //go:build constraint can hold
// in a production build: some GOOS/GOARCH combination satisfies it with every
// custom tag (integration, ignore, e2e, ...) false. No constraint, or one that is
// satisfied by platform/version tags alone (go1.1, linux), is buildable.
func buildableInProduction(src []byte) bool {
	for _, line := range strings.Split(string(src[:min(len(src), 8192)]), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "package ") {
			break
		}
		if !constraint.IsGoBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			return true // unreadable: scan it rather than skip it
		}
		for _, goos := range []string{"linux", "darwin", "windows", "freebsd", "js", "wasip1"} {
			for _, goarch := range []string{"amd64", "arm64", "386", "wasm"} {
				unix := goos != "windows" && goos != "js" && goos != "wasip1"
				if expr.Eval(func(tag string) bool {
					switch tag {
					case goos, goarch, "gc", "cgo":
						return true
					case "unix":
						return unix
					}
					return strings.HasPrefix(tag, "go1.")
				}) {
					return true
				}
			}
		}
		return false
	}
	return true
}

// pinnedRegistration is a file outside the scanned surface that legitimately
// registers: its exact registrations are asserted (a new one, or a vanished one,
// fails), so a pin is a reviewed fact and never a blanket exemption.
type pinnedRegistration struct {
	Reason   string
	Patterns []string // the readable "METHOD /path" registrations, exactly
	Dynamic  int      // registrations the scanner cannot read (a helper, a variable), exactly
}

var pinnedRegistrationFiles = map[string]pinnedRegistration{
	"internal/common/obs/admin.go": {
		Reason: "the admin mux (pprof) served on its own loopback listener, never part of the public API or its spec",
		Patterns: []string{
			"GET /debug/pprof/", "GET /debug/pprof/cmdline", "GET /debug/pprof/profile",
			"GET /debug/pprof/symbol", "GET /debug/pprof/trace",
		},
	},
	"internal/common/obs/health.go": {
		Reason:   "liveness, readiness and health probes: unauthenticated by design, outside the versioned API and its spec",
		Patterns: []string{"GET /alive", "GET /health", "GET /ready"},
	},
	"internal/identity/ports/authn/scope_policy.go": {
		Reason:  "a lookup-only ServeMux used for pattern matching (Handler(r)), never served; it registers through one local helper",
		Dynamic: 1,
	},
}

// routeSourceFile reports whether a repo-relative, slash path is a file whose
// registrations the gates judge as API routes: every non-test .go file under
// internal/<module>/ports/ (event-handler registrations under subscribers/ are
// not routes) and cmd/api/*.go. The pinned files are judged separately.
func routeSourceFile(rel string) bool {
	if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
		return false
	}
	if _, pinned := pinnedRegistrationFiles[rel]; pinned {
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
	return !slices.Contains(parts[3:len(parts)-1], "subscribers")
}

// scanRepoFiles scans a whole repository given as rel path -> source: every
// non-test .go file once. It returns the routes of the scanned surface and every
// offence, including registrations outside the surface that are not pinned and
// pins that no longer match. realRepo makes missing pinned files an offence.
func scanRepoFiles(files map[string][]byte, realRepo bool) ([]routeReg, []routeOffence) {
	byDir := map[string]map[string][]byte{}
	for rel, src := range files {
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") || !buildableInProduction(src) {
			continue
		}
		dir := path.Dir(rel)
		if byDir[dir] == nil {
			byDir[dir] = map[string][]byte{}
		}
		byDir[dir][rel] = src
	}
	// every registration method is named Handle or HandleFunc: a package without
	// the word needs no type-check
	for dir, fs := range byDir {
		if !slices.ContainsFunc(sortedKeys(fs), func(rel string) bool { return bytes.Contains(fs[rel], []byte("Handle")) }) {
			delete(byDir, dir)
		}
	}
	var routes []routeReg
	var offences []routeOffence
	seenPinned := map[string]bool{}
	for _, dir := range sortedKeys(byDir) {
		scanned := scanPackageFiles(byDir[dir])
		for _, rel := range sortedKeys(scanned) {
			fs := scanned[rel]
			pin, pinned := pinnedRegistrationFiles[rel]
			switch {
			case routeSourceFile(rel):
				routes = append(routes, fs.routes...)
				offences = append(offences, fs.offences...)
			case pinned:
				seenPinned[rel] = true
				var got []string
				for _, r := range fs.routes {
					got = append(got, r.Pattern)
				}
				slices.Sort(got)
				want := slices.Clone(pin.Patterns)
				slices.Sort(want)
				if !slices.Equal(got, want) || len(fs.offences) != pin.Dynamic {
					offences = append(offences, routeOffence{rel, 1, "pinned registration file changed: it registers [" + strings.Join(got, ", ") + "] plus " + strconv.Itoa(len(fs.offences)) + " unreadable, the pin says [" + strings.Join(want, ", ") + "] plus " + strconv.Itoa(pin.Dynamic) + " (" + pin.Reason + "): update pinnedRegistrationFiles deliberately"})
				}
			default:
				for _, r := range fs.routes {
					offences = append(offences, routeOffence{rel, r.Line, "registers " + r.Pattern + " outside internal/<module>/ports and cmd/api: move it to the module's ports (or, for a reviewed non-API mux, pin the file in pinnedRegistrationFiles)"})
				}
				for _, o := range fs.offences {
					o.Msg = "registration outside internal/<module>/ports and cmd/api: " + o.Msg
					offences = append(offences, o)
				}
			}
		}
	}
	for _, rel := range sortedKeys(pinnedRegistrationFiles) {
		_, present := files[rel]
		switch {
		case present && !seenPinned[rel]:
			offences = append(offences, routeOffence{rel, 1, "pinned registration file no longer registers anything: remove its pin"})
		case !present && realRepo:
			offences = append(offences, routeOffence{rel, 1, "pinned registration file does not exist: remove its pin"})
		}
	}
	slices.SortFunc(routes, func(a, b routeReg) int { return strings.Compare(a.Pattern, b.Pattern) })
	return routes, offences
}

// scanAllRoutes scans the whole repository once per call.
func scanAllRoutes(t *testing.T) ([]routeReg, []routeOffence) {
	t.Helper()
	root := repoRoot(t)
	files := map[string][]byte{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "testdata" || name == "node_modules" || name == "vendor" || (strings.HasPrefix(name, ".") && p != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		src, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		files[filepath.ToSlash(rel)] = src
		return nil
	})
	if err != nil {
		t.Fatalf("scan routes: %v", err)
	}
	return scanRepoFiles(files, true)
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

const syntheticHead = "package ports\nimport (\"net/http\"; \"github.com/leadkart/leadkart-go/internal/identity/ports/authn\")\n"

func scanSynthetic(t *testing.T, file, src string) ([]routeReg, []routeOffence) {
	t.Helper()
	return scanRouteSource(file, []byte(src))
}

// rejected reports whether the scanner and the shared verdict together refuse a
// repository (rel path -> source): an unreadable registration, a registration
// outside the scanned surface, or a route with no authorising gate.
func rejected(files map[string]string) (bool, []string) {
	in := map[string][]byte{}
	for k, v := range files {
		in[k] = []byte(v)
	}
	routes, off := scanRepoFiles(in, false)
	var why []string
	for _, o := range off {
		why = append(why, o.String())
	}
	for _, r := range routes {
		for _, p := range routeVerdict(r, nil) {
			why = append(why, r.Pattern+": "+p)
		}
	}
	return len(why) > 0, why
}

func TestRouteScanner_RejectsEveryHiddenRegistrationStyle(t *testing.T) {
	t.Parallel()
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
		{"method value", `func AddRoutes(mux *http.ServeMux) { reg := mux.Handle; reg("GET /api/v1/x", h) }`},
		{"method value HandleFunc", `func AddRoutes(mux *http.ServeMux) { reg := mux.HandleFunc; reg("GET /api/v1/x", h) }`},
		{"method expression", `func AddRoutes(mux *http.ServeMux) { (*http.ServeMux).Handle(mux, "GET /api/v1/x", h) }`},
		{"method expression bound", `func AddRoutes(mux *http.ServeMux) { reg := (*http.ServeMux).Handle; reg(mux, "GET /api/v1/x", h) }`},
		{"default mux function value", `func AddRoutes() { reg := http.Handle; reg("GET /api/v1/x", h) }`},
		{"package-level func literal", `var _ = func() { http.NewServeMux().Handle("GET /api/v1/x", h) }()`},
		{"package-level func literal on a param-less mux", `var m = http.NewServeMux()
var _ = func() { m.Handle("GET /api/v1/x", h) }()`},
		{"interface receiver with a variable pattern", `type router interface{ Handle(pattern string, h http.Handler) }
func AddRoutes(r router, p string) { r.Handle(p, h) }`},
		{"wrapper struct field with a variable pattern", `type wrapper struct{ mux *http.ServeMux }
func (w wrapper) add(p string) { w.mux.Handle(p, h) }`},
		{"embedded ServeMux with a variable pattern", `type wrapper struct{ *http.ServeMux }
func (w wrapper) add(p string) { w.Handle(p, h) }`},
		{"unresolved receiver with a string variable", `func AddRoutes(r other.Router) { p := "GET /api/v1/x"; r.Handle(p, h) }`},
	}
	for _, tc := range offenders {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, off := scanSynthetic(t, "internal/x/ports/http.go", syntheticHead+tc.body); len(off) == 0 {
				t.Fatalf("scanner accepted: %s", tc.body)
			}
		})
	}
}

// The command/query handlers' Handle(ctx, cmd) are not registrations.
func TestRouteScanner_IgnoresCommandHandlers(t *testing.T) {
	t.Parallel()
	const body = `type cmdHandler interface{ Handle(ctx context.Context, c Cmd) error }
func use(h cmdHandler, ctx context.Context) { _ = h.Handle(ctx, Cmd{}) }`
	routes, off := scanSynthetic(t, "internal/x/ports/http.go", syntheticHead+body)
	if len(routes) != 0 || len(off) != 0 {
		t.Fatalf("a command handler call was read as a registration: %v %v", routes, off)
	}
}

// (d): a mux named anything is still scanned, and its routes are judged like any other.
func TestRouteScanner_SeesRegistrationsOnAnyReceiverName(t *testing.T) {
	t.Parallel()
	const body = `func AddRoutes(m *http.ServeMux, v authn.Verifier) {
	auth := authn.RequireAuth(v)
	m.Handle("GET /api/v1/x", auth(h))
}
func other() {
	router := http.NewServeMux()
	router.HandleFunc("GET /api/v1/y", h)
	alias := router
	alias.Handle("POST /api/v1/z", h)
}
type holder struct{ inner *http.ServeMux }
func (h holder) add() { h.inner.Handle("PUT /api/v1/w", nil) }`
	routes, off := scanSynthetic(t, "internal/x/ports/http.go", syntheticHead+body)
	if len(off) != 0 {
		t.Fatalf("unexpected offences: %v", off)
	}
	got := []string{}
	for _, r := range routes {
		got = append(got, r.Pattern)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"GET /api/v1/x", "GET /api/v1/y", "POST /api/v1/z", "PUT /api/v1/w"}) {
		t.Fatalf("scanned routes = %v", got)
	}
	for _, r := range routes {
		if r.Pattern == "GET /api/v1/x" && len(routeVerdict(r, nil)) == 0 {
			t.Fatal("authentication-only route on an arbitrarily named mux must be judged unauthorised")
		}
	}
}

// Every way of making a non-gate look like a gate (or hiding a gate's absence)
// must leave the route without its authorising tag.
func TestRouteScanner_GateEvasionsAreNotGates(t *testing.T) {
	t.Parallel()
	evasions := []struct{ name, body string }{
		{"alias reassignment", `func AddRoutes(mux *http.ServeMux, v authn.Verifier, s authn.StampValidator) {
	read := authn.RequirePermission(v, s, perm)
	read = noop
	mux.Handle("GET /api/v1/x", read(h))
}`},
		{"alias reassigned inside a closure", `func AddRoutes(mux *http.ServeMux, v authn.Verifier, s authn.StampValidator) {
	read := authn.RequirePermission(v, s, perm)
	func() { read = noop }()
	mux.Handle("GET /api/v1/x", read(h))
}`},
		{"alias address taken", `func AddRoutes(mux *http.ServeMux, v authn.Verifier, s authn.StampValidator) {
	read := authn.RequirePermission(v, s, perm)
	p := &read
	*p = noop
	mux.Handle("GET /api/v1/x", read(h))
}`},
		{"block shadowing", `func AddRoutes(mux *http.ServeMux, v authn.Verifier, s authn.StampValidator) {
	read := authn.RequirePermission(v, s, perm)
	_ = read
	{
		read := noop
		mux.Handle("GET /api/v1/x", read(h))
	}
}`},
		{"gate passed as an ignored argument", `func AddRoutes(mux *http.ServeMux, v authn.Verifier, s authn.StampValidator) {
	read := authn.RequirePermission(v, s, perm)
	mux.Handle("GET /api/v1/x", ignore(read, h))
}`},
		{"gate value reassigned after use", `func AddRoutes(mux *http.ServeMux, v authn.Verifier, s authn.StampValidator) {
	read := authn.RequirePermission(v, s, perm)
	x := read(h)
	x = h
	mux.Handle("GET /api/v1/x", x)
}`},
		{"a variable named authn", `func AddRoutes(mux *http.ServeMux, authn fake) {
	mux.Handle("GET /api/v1/x", authn.RequirePermission(1, 2, 3)(h))
}`},
		{"another package imported as authn", `import authn "example.com/not/the/real/authn"
func AddRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/x", authn.RequirePermission(1, 2, 3)(h))
}`},
		{"a struct field named authn", `type g struct{ authn fake }
func (x g) add(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/x", x.authn.RequirePermission(1, 2, 3)(h))
}`},
	}
	for _, tc := range evasions {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			head := syntheticHead
			if strings.Contains(tc.body, "import authn") {
				head = "package ports\nimport \"net/http\"\n"
			}
			routes, off := scanSynthetic(t, "internal/x/ports/http.go", head+tc.body)
			if len(off) != 0 {
				t.Fatalf("unexpected offences: %v", off)
			}
			if len(routes) != 1 {
				t.Fatalf("want 1 route, got %d", len(routes))
			}
			if routes[0].has(tagAuthz) {
				t.Fatalf("the evasion produced an authorising tag: %v", routes[0].Tags)
			}
			if len(routeVerdict(routes[0], nil)) == 0 {
				t.Fatal("verdict accepted the evasion")
			}
		})
	}
}

// The honest forms around the evasions still read as gates (the scanner is not
// merely rejecting everything).
func TestRouteScanner_HonestGateFormsAreRead(t *testing.T) {
	t.Parallel()
	const body = `func AddRoutes(mux *http.ServeMux, v authn.Verifier, s authn.StampValidator) {
	read := authn.RequirePermission(v, s, perm)
	mux.Handle("GET /api/v1/a", read(h))
	mux.Handle("GET /api/v1/b", authn.RequirePermission(v, s, perm)(h))
	mux.Handle("GET /api/v1/c", authn.RejectTenantScope(read(h)))
	{
		other := authn.RequirePermission(v, s, perm)
		mux.Handle("GET /api/v1/d", other(h))
	}
	var viaVar = authn.RequireAnyPermission(v, s, perm, perm)
	mux.Handle("GET /api/v1/e", viaVar(h))
}`
	routes, off := scanSynthetic(t, "internal/x/ports/http.go", syntheticHead+body)
	if len(off) != 0 || len(routes) != 5 {
		t.Fatalf("routes=%d offences=%v", len(routes), off)
	}
	for _, r := range routes {
		if v := routeVerdict(r, nil); len(v) != 0 {
			t.Errorf("%s must pass: %v", r.Pattern, v)
		}
	}
}

// (a): which files are the scanned API surface.
func TestRouteScanner_ScansEveryPortsFile(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{
		"internal/crm/ports/http.go", "internal/crm/ports/http_reminders.go", "internal/crm/ports/anything.go",
		"internal/identity/ports/tenant_http.go", "internal/identity/ports/sub/deep.go", "cmd/api/main.go",
		// the authn exclusion is pinned to ONE file: every other authn directory is scanned
		"internal/identity/ports/authn/authn.go", "internal/crm/ports/authn/x.go",
	} {
		if !routeSourceFile(rel) {
			t.Errorf("%s must be scanned", rel)
		}
	}
	for _, rel := range []string{
		"internal/crm/ports/http_test.go", "internal/crm/ports/subscribers/s.go",
		"internal/identity/ports/authn/scope_policy.go",
		"internal/crm/app/x.go", "internal/common/obs/health.go", "cmd/api/sub/x.go", "cmd/migrate/main.go",
	} {
		if routeSourceFile(rel) {
			t.Errorf("%s must not be judged as an API route file", rel)
		}
	}
}

// Build constraints: only files that cannot be in a production build are skipped.
func TestRouteScanner_BuildConstraintSelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"", true},
		{"//go:build go1.1", true},
		{"//go:build linux", true},
		{"//go:build !integration", true},
		{"//go:build unix", true},
		{"//go:build integration || go1.1", true},
		{"//go:build !windows", true},
		{"//go:build integration", false},
		{"//go:build ignore", false},
		{"//go:build e2e", false},
		{"//go:build integration && linux", false},
		{"//go:build go1.1 && integration", false},
		{"//go:build tools", false},
	} {
		src := tc.line + "\n\npackage ports\n"
		if got := buildableInProduction([]byte(src)); got != tc.want {
			t.Errorf("%q: buildable=%v, want %v", tc.line, got, tc.want)
		}
	}
}

const unauthRoute = `package ports
import "net/http"
func AddRoutes(mux *http.ServeMux) { mux.Handle("POST /api/v1/zz", h) }
`

// Every hole the review found must now be a failure, each proven with a
// synthetic repository.
func TestRouteScanner_RepositoryWidePass(t *testing.T) {
	t.Parallel()
	rejects := map[string]map[string]string{
		"build-tagged file compiled everywhere (go1.1)": {
			"internal/crm/ports/zz_tagged.go": "//go:build go1.1\n\n" + unauthRoute,
		},
		"build-tagged !integration": {
			"internal/crm/ports/zz_tagged.go": "//go:build !integration\n\n" + unauthRoute,
		},
		"registration in internal/common outside any ports dir": {
			"internal/common/zzhttp/r.go": strings.Replace(unauthRoute, "POST /api/v1/zz", "DELETE /api/v1/zz-outside", 1),
		},
		"registration in cmd/api subdirectory": {
			"cmd/api/sub/r.go": unauthRoute,
		},
		"registration in a module adapters/http directory": {
			"internal/crm/adapters/http/r.go": unauthRoute,
		},
		"registration in a module app directory": {
			"internal/crm/app/r.go": unauthRoute,
		},
		"a second pinned-looking file in obs": {
			"internal/common/obs/zz.go": strings.Replace(unauthRoute, "POST /api/v1/zz", "GET /debug/zz", 1),
		},
		"a pinned file registering one more route": {
			"internal/common/obs/health.go": `package obs
import "net/http"
func Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /alive", nil)
	mux.HandleFunc("GET /ready", nil)
	mux.HandleFunc("GET /health", nil)
	mux.HandleFunc("GET /metrics", nil)
}`,
		},
		"a pinned file losing a route": {
			"internal/common/obs/health.go": `package obs
import "net/http"
func Mount(mux *http.ServeMux) { mux.HandleFunc("GET /alive", nil) }`,
		},
		"the authn exclusion does not extend to another module": {
			"internal/crm/ports/authn/zz.go": unauthRoute,
		},
		"the authn exclusion does not extend to another file of the identity authn package": {
			"internal/identity/ports/authn/zz.go": unauthRoute,
		},
		"an unreadable registration outside the surface": {
			"internal/common/zzhttp/r.go": "package zz\nimport \"net/http\"\nfunc A(mux *http.ServeMux, p string) { mux.Handle(p, nil) }\n",
		},
	}
	for name, files := range rejects {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if bad, _ := rejected(files); !bad {
				t.Fatalf("accepted: %v", files)
			}
		})
	}

	accepts := map[string]map[string]string{
		"integration-tagged file is not part of the production build": {
			"internal/crm/ports/zz_tagged.go": "//go:build integration\n\n" + unauthRoute,
		},
		"ignore-tagged file": {
			"internal/crm/ports/zz_tagged.go": "//go:build ignore\n\n" + unauthRoute,
		},
		"a test file": {
			"internal/crm/ports/zz_test.go": unauthRoute,
		},
		"pinned obs health file exactly as pinned": {
			"internal/common/obs/health.go": `package obs
import "net/http"
func Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /alive", nil)
	mux.HandleFunc("GET /ready", nil)
	mux.HandleFunc("GET /health", nil)
}`,
		},
		"a command handler outside the surface": {
			"internal/crm/app/c.go": "package app\ntype C interface{ Handle(ctx Ctx, x X) error }\nfunc f(c C) { _ = c.Handle(nil, nil) }\n",
		},
		"authorised route in a ports file": {
			"internal/crm/ports/ok.go": syntheticHead + `func AddRoutes(mux *http.ServeMux, v authn.Verifier, s authn.StampValidator) {
	mux.Handle("GET /api/v1/ok", authn.RequirePermission(v, s, perm)(h))
}`,
		},
	}
	for name, files := range accepts {
		t.Run("accepts: "+name, func(t *testing.T) {
			t.Parallel()
			if bad, why := rejected(files); bad {
				t.Fatalf("rejected: %v", why)
			}
		})
	}
}

// (g), (h) and the two latent cases: the shared verdict must reject each.
func TestRouteScanner_VerdictRejectsUnauthorisedAndMisplacedGates(t *testing.T) {
	t.Parallel()
	const body = `func AddRoutes(mux *http.ServeMux, v authn.Verifier, s authn.StampValidator) {
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
	routes, off := scanSynthetic(t, "internal/x/ports/http.go", syntheticHead+body)
	if len(off) != 0 {
		t.Fatalf("unexpected offences: %v", off)
	}
	got := map[string][]string{}
	byPattern := map[string]routeReg{}
	for _, r := range routes {
		got[r.Pattern] = routeVerdict(r, nil)
		byPattern[r.Pattern] = r
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
	if v := routeVerdict(byPattern["GET /api/v1/platform/x"], selfService); len(v) != 0 {
		t.Errorf("allow-listed route: %v", v)
	}
}
