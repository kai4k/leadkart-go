package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Review G2c N7: the dead-letter replay job is registered with the worker (a river
// worker for its kind) AND scheduled (a periodic job). Either one missing and every
// timed-out dead letter waits for a person. The composition root is read, as
// cmd/api/argon_gate_wiring_test.go reads its own.
func TestWiring_TheDeadLetterReplayJobIsRegisteredAndScheduled(t *testing.T) {
	t.Parallel()
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "jobs" {
				calls[sel.Sel.Name]++
			}
		}
		return true
	})
	for _, want := range []string{"NewDeadLetterReplayWorker", "DeadLetterReplayPeriodicJob"} {
		if calls[want] != 1 {
			t.Errorf("cmd/worker calls jobs.%s %d times, want once", want, calls[want])
		}
	}
}
