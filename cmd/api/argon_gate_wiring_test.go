package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Every command that hashes or verifies a password must be built under the one
// process-wide Argon2 gate (ADR 0068 D12): a handler wired without it would be an
// unbounded memory sink on an anonymous or low-privilege route. The gate is a
// builder option, so the compiler cannot force it; this test reads the
// composition root and does.
func TestWiring_EveryPasswordHandlerRunsUnderTheArgonGate(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	passwordHandlers := map[string]bool{
		"NewLoginHandler":                false,
		"NewChangePasswordHandler":       false,
		"NewConfirmPasswordResetHandler": false,
		"NewRegisterTenantHandler":       false,
		"NewCreateUserHandler":           false,
	}
	built := map[string]int{} // constructor calls seen
	gated := map[string]int{} // constructor calls whose result is passed through WithArgonGate(argonGate)

	isCommandCtor := func(e ast.Expr) (string, bool) {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return "", false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return "", false
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "command" {
			return sel.Sel.Name, true
		}
		return "", false
	}

	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if name, ok := isCommandCtor(call); ok {
			if _, tracked := passwordHandlers[name]; tracked {
				built[name]++
			}
		}
		// A chain of builders ends at X.WithArgonGate(...) with X a (possibly chained) ctor call.
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WithArgonGate" {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		if id, ok := call.Args[0].(*ast.Ident); !ok || id.Name != "argonGate" {
			return true
		}
		// Walk down the receiver chain to the constructor.
		recv := sel.X
		for {
			if name, ok := isCommandCtor(recv); ok {
				if _, tracked := passwordHandlers[name]; tracked {
					gated[name]++
				}
				break
			}
			inner, ok := recv.(*ast.CallExpr)
			if !ok {
				break
			}
			isel, ok := inner.Fun.(*ast.SelectorExpr)
			if !ok {
				break
			}
			recv = isel.X
		}
		return true
	})

	for name := range passwordHandlers {
		if built[name] == 0 {
			t.Errorf("%s is no longer built in main.go: update this test if the handler moved", name)
			continue
		}
		if gated[name] != built[name] {
			t.Errorf("%s is built %d time(s) but %d with .WithArgonGate(argonGate)", name, built[name], gated[name])
		}
	}
}
