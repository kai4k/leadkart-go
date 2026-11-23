// Fitness function for identity fix round 2, I2: the person change guard never
// waits for a second pooled connection, and the one read that makes that
// possible stays the guard's alone.
//
// The guard (global suspension) holds a tenant authority lock and
// the person row while it re-reads the person's memberships across tenants. That
// read runs on the guard's own transaction: membership.Repository's
// ListActiveRefsForPersonInTx opens a savepoint, turns the platform scope on,
// reads, and rolls the savepoint back, which puts the scope back. A transaction
// that can be made platform-wide from the inside is a sharp tool, so it has ONE
// caller, and nothing else switches the scope of a live transaction.
//
// What the scan pins, on production sources:
//
//  1. ListActiveRefsForPersonInTx is selected (called, or taken as a method
//     value) in exactly one place: PersonChangeGuard.lockVerifyAndChange. It
//     must be selected there: a guard that went back to a read which opens its
//     own connection fails this test as well as the pool tests.
//  2. Inside the guard, ListAllForPerson (the read that opens its own
//     connection) is selected only in activeMemberships, and activeMemberships
//     is called only from runOnce and never inside a function literal: the
//     guard's transactions are function literals, so that read happens before
//     any lock is taken.
//  3. The savepoint mechanism readAsPlatformOnTx is used only by the adapter's
//     ListActiveRefsForPersonInTx.
//  4. pg.SetPlatformOnTx (the statement that turns the platform scope on) is
//     used only where a transaction is opened (pg's bindScope) and by that
//     savepoint mechanism.
//
// What it cannot see: reflection, and a platform scope set by a hand-written
// set_config statement (TestArch_GUCNamesSingleSourced covers the GUC name).
package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	onTxReadMethod    = "ListActiveRefsForPersonInTx"
	ownConnReadMethod = "ListAllForPerson"
	guardFilePath     = "internal/identity/app/command/person_change_guard.go"
	membershipPGPath  = "internal/identity/adapters/membership_repository_pg.go"
	transactorPath    = "internal/common/pg/transactor.go"
	guardOnTxReadHome = guardFilePath + ":lockVerifyAndChange"
)

// identUse is one use of a name: where it is, and whether it sits inside a
// function literal of its enclosing declaration.
type identUse struct {
	file, fn string
	inLit    bool
}

func (u identUse) at() string { return u.file + ":" + u.fn }

// usesOf lists every use of name in src (a selector x.name or a bare identifier),
// except the declaration of a function or method of that name.
func usesOf(t *testing.T, file string, src []byte, name string) []identUse {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	var out []identUse
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		var walk func(n ast.Node, inLit bool)
		walk = func(n ast.Node, inLit bool) {
			ast.Inspect(n, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.FuncLit:
					walk(x.Body, true)
					return false
				case *ast.SelectorExpr:
					if x.Sel.Name == name {
						out = append(out, identUse{file: file, fn: fd.Name.Name, inLit: inLit})
					}
					walk(x.X, inLit)
					return false
				case *ast.Ident:
					if x.Name == name {
						out = append(out, identUse{file: file, fn: fd.Name.Name, inLit: inLit})
					}
				}
				return true
			})
		}
		walk(fd.Body, false)
	}
	return out
}

// onTxPlatformReadOffences applies the four rules to production sources (path,
// slash-separated and relative to the repo root, to content).
func onTxPlatformReadOffences(t *testing.T, sources map[string][]byte) []string {
	t.Helper()
	var offences []string
	guardReads := 0
	for _, file := range slices.Sorted(maps.Keys(sources)) {
		src := sources[file]
		for _, u := range usesOf(t, file, src, onTxReadMethod) {
			if u.at() != guardOnTxReadHome {
				offences = append(offences, u.at()+" uses "+onTxReadMethod+": only "+guardOnTxReadHome+" may (it turns the platform scope on inside a live transaction)")
				continue
			}
			guardReads++
		}
		for _, u := range usesOf(t, file, src, "readAsPlatformOnTx") {
			if u.at() != membershipPGPath+":"+onTxReadMethod {
				offences = append(offences, u.at()+" uses readAsPlatformOnTx: only the adapter's "+onTxReadMethod+" may")
			}
		}
		for _, u := range usesOf(t, file, src, "SetPlatformOnTx") {
			if u.at() != transactorPath+":bindScope" && u.at() != membershipPGPath+":readAsPlatformOnTx" {
				offences = append(offences, u.at()+" uses SetPlatformOnTx: the platform scope is set where a transaction is opened (pg.bindScope), or by readAsPlatformOnTx")
			}
		}
		if file != guardFilePath {
			continue
		}
		for _, u := range usesOf(t, file, src, ownConnReadMethod) {
			if u.fn != "activeMemberships" {
				offences = append(offences, u.at()+" uses "+ownConnReadMethod+": inside the guard only activeMemberships may (it opens a connection of its own)")
			}
		}
		for _, u := range usesOf(t, file, src, "activeMemberships") {
			if u.fn != "runOnce" || u.inLit {
				offences = append(offences, u.at()+" calls activeMemberships under the guard's transaction or outside runOnce: that read opens a connection of its own and must run before any lock is taken")
			}
		}
	}
	if _, scanned := sources[guardFilePath]; scanned && guardReads == 0 {
		offences = append(offences, guardOnTxReadHome+" does not use "+onTxReadMethod+": the guard's re-read under its locks must run on its own transaction")
	}
	return offences
}

// TestArch_PersonGuard_ReadsMembershipsOnItsOwnTransaction pins the four rules
// in this file's header.
//
// Scope: production — non-test files under internal/ and cmd/ (tests may call
// the read to exercise it).
//
// arch-test:no-negative-fixture (RED proof: TestOnTxPlatformReadScanner_FlagsEveryBypass feeds the scanner one synthetic offender per rule)
func TestArch_PersonGuard_ReadsMembershipsOnItsOwnTransaction(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	sources := map[string][]byte{}
	for _, dir := range []string{"internal", "cmd"} {
		walkGoFiles(t, filepath.Join(root, dir), false, func(path string, src []byte) {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				t.Fatalf("rel %s: %v", path, err)
			}
			sources[filepath.ToSlash(rel)] = src
		})
	}
	for _, must := range []string{guardFilePath, membershipPGPath, transactorPath} {
		if _, ok := sources[must]; !ok {
			t.Fatalf("%s was not scanned: the pin would pass on nothing", must)
		}
	}
	if offences := onTxPlatformReadOffences(t, sources); len(offences) > 0 {
		t.Errorf("%d offence(s) against the person guard's one-connection rule:\n  %s", len(offences), strings.Join(offences, "\n  "))
	}
}

// TestOnTxPlatformReadScanner_FlagsEveryBypass is the RED proof of the scan: a
// clean pair of sources passes, and each way around the rules is reported.
func TestOnTxPlatformReadScanner_FlagsEveryBypass(t *testing.T) {
	t.Parallel()
	const cleanGuard = `package command
func (g PersonChangeGuard) activeMemberships() { g.w.facts.memberships.ListAllForPerson(ctx, id) }
func (g PersonChangeGuard) runOnce() { g.activeMemberships(); g.w.locked(func() { g.lockVerifyAndChange() }) }
func (g PersonChangeGuard) lockVerifyAndChange() { g.w.facts.memberships.ListActiveRefsForPersonInTx(ctx, id) }
`
	const cleanAdapter = `package adapters
func (r *MembershipRepository) ListActiveRefsForPersonInTx() { readAsPlatformOnTx(ctx, tx, nil) }
func readAsPlatformOnTx() { pg.SetPlatformOnTx(ctx, sp) }
`
	const cleanPG = `package pg
func bindScope() { SetPlatformOnTx(ctx, tx) }
func SetPlatformOnTx() {}
`
	clean := func() map[string][]byte {
		return map[string][]byte{guardFilePath: []byte(cleanGuard), membershipPGPath: []byte(cleanAdapter), transactorPath: []byte(cleanPG)}
	}
	if got := onTxPlatformReadOffences(t, clean()); len(got) != 0 {
		t.Fatalf("the clean sources are reported: %v", got)
	}
	for name, tc := range map[string]struct {
		file, src string
		want      string
	}{
		"another command calls the on-transaction read": {"internal/identity/app/command/other.go",
			"package command\nfunc (h H) Handle() { h.memberships.ListActiveRefsForPersonInTx(ctx, id) }\n", "other.go:Handle uses " + onTxReadMethod},
		"a query takes it as a method value": {"internal/identity/app/query/q.go",
			"package query\nfunc f() { read := repo.ListActiveRefsForPersonInTx; _ = read }\n", "q.go:f uses " + onTxReadMethod},
		"the guard uses it outside lockVerifyAndChange": {guardFilePath,
			cleanGuard + "func (g PersonChangeGuard) extra() { g.w.facts.memberships.ListActiveRefsForPersonInTx(ctx, id) }\n", ":extra uses " + onTxReadMethod},
		"the guard goes back to the read that opens its own connection": {guardFilePath,
			strings.Replace(cleanGuard, "lockVerifyAndChange() { g.w.facts.memberships.ListActiveRefsForPersonInTx(ctx, id) }", "lockVerifyAndChange() { g.activeMemberships() }", 1),
			"lockVerifyAndChange calls activeMemberships"},
		"the guard drops the on-transaction read": {guardFilePath,
			strings.Replace(cleanGuard, "g.w.facts.memberships.ListActiveRefsForPersonInTx(ctx, id)", "", 1), "does not use " + onTxReadMethod},
		"the guard lists memberships inside its transaction": {guardFilePath,
			strings.Replace(cleanGuard, "g.w.locked(func() { g.lockVerifyAndChange() })", "g.w.locked(func() { g.activeMemberships() })", 1) + "", "runOnce calls activeMemberships"},
		"the guard calls ListAllForPerson directly under its locks": {guardFilePath,
			strings.Replace(cleanGuard, "lockVerifyAndChange() {", "lockVerifyAndChange() { g.w.facts.memberships.ListAllForPerson(ctx, id);", 1), "lockVerifyAndChange uses " + ownConnReadMethod},
		"another adapter read borrows the savepoint mechanism": {membershipPGPath,
			cleanAdapter + "func (r *MembershipRepository) ListForTenant() { readAsPlatformOnTx(ctx, tx, nil) }\n", ":ListForTenant uses readAsPlatformOnTx"},
		"a repository switches a live transaction to the platform scope": {"internal/orders/adapters/order_repository_pg.go",
			"package adapters\nfunc (r *R) Get() { pg.SetPlatformOnTx(ctx, tx) }\n", "order_repository_pg.go:Get uses SetPlatformOnTx"},
	} {
		sources := clean()
		sources[tc.file] = []byte(tc.src)
		got := onTxPlatformReadOffences(t, sources)
		if !slices.ContainsFunc(got, func(o string) bool { return strings.Contains(o, tc.want) }) {
			t.Errorf("%s: offences = %v, want one containing %q", name, got, tc.want)
		}
	}
}
