// messaging_resilience_arch_test.go — locks the canonical Watermill
// resilience wiring (ADR 0067) into router.go so a future edit cannot
// silently reintroduce the panic-no-retry / infinite-retry P0s.
//
// The middleware ORDER is load-bearing and invisible to the type system,
// so it is exactly the kind of invariant a fitness function must hold.
//
// arch-test:no-synctest — purely-static AST analysis of one file.
package architecture_test

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// renderAddMiddlewareCalls parses router.go and returns, for every
// `.AddMiddleware(...)` call, the rendered text of each argument in order.
func renderAddMiddlewareCalls(t *testing.T) [][]string {
	t.Helper()
	path := filepath.Join(internalDir(t), "common", "messaging", "router.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse router.go: %v", err)
	}
	var calls [][]string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "AddMiddleware" {
			return true
		}
		args := make([]string, 0, len(call.Args))
		for _, a := range call.Args {
			var sb strings.Builder
			if perr := printer.Fprint(&sb, fset, a); perr != nil {
				t.Fatalf("render arg: %v", perr)
			}
			args = append(args, sb.String())
		}
		calls = append(calls, args)
		return true
	})
	if len(calls) == 0 {
		t.Fatal("no .AddMiddleware calls found in router.go")
	}
	return calls
}

func contains(ss []string, sub string) int {
	for i, s := range ss {
		if strings.Contains(s, sub) {
			return i
		}
	}
	return -1
}

// TestArch_MessagingMiddlewareOrderResilient asserts the per-handler stack
// is exactly PoisonQueue → Idempotency → Audit → Retry → Recoverer (so
// panics retry, the DLQ fires only after exhaustion, and a poisoned message
// is NOT marked processed — Idempotency sits INSIDE PoisonQueue so the dedup
// row is written only on genuine success), and that the global setup chain
// does NOT carry Recoverer (which would make it outermost and defeat
// panic-retry — the original P0).
//
// arch-test:no-negative-fixture — RED→GREEN proof is the mutation test
// (move Recoverer to the global chain, swap PoisonQueue/Idempotency, or drop
// r.poison → RED; revert → GREEN). A committed fixture would be a second
// router.go.
func TestArch_MessagingMiddlewareOrderResilient(t *testing.T) {
	t.Parallel()
	calls := renderAddMiddlewareCalls(t)

	// Every per-handler stack is an AddMiddleware call carrying r.poison + an
	// Idempotency variant. Two canonical shapes (ADR 0067 Phase-4):
	//   at-least-once : poison → Idempotency → Audit → retry → Recoverer
	//   transactional : poison → Audit → retry → TransactionalIdempotency → Recoverer
	// Transactional keeps the dedup+handler tx INSIDE Retry so each retry
	// attempt gets a FRESH tx (retrying inside an aborted pgx tx fails);
	// at-least-once keeps Idempotency outermost so a duplicate short-circuits
	// before Audit/Retry/DLQ. Both end in Recoverer (innermost) so panics
	// convert to errors the inner path handles.
	var stacks [][]string
	for _, c := range calls {
		if contains(c, "poison") >= 0 && contains(c, "Idempotency") >= 0 {
			stacks = append(stacks, c)
		}
	}
	if len(stacks) == 0 {
		t.Fatal("no per-handler AddMiddleware stack (Idempotency + poison) found")
	}
	for _, stack := range stacks {
		order := []string{"poison", "Idempotency", "Audit", "retry", "Recoverer"}
		if contains(stack, "TransactionalIdempotency") >= 0 {
			order = []string{"poison", "Audit", "retry", "TransactionalIdempotency", "Recoverer"}
		}
		prev := -1
		for _, want := range order {
			at := contains(stack, want)
			if at < 0 {
				t.Errorf("per-handler stack missing %q (got %v)", want, stack)
				continue
			}
			if at <= prev {
				t.Errorf("per-handler stack out of order: %q at %d, expected after %d (got %v)", want, at, prev, stack)
			}
			prev = at
		}
		if last := stack[len(stack)-1]; !strings.Contains(last, "Recoverer") {
			t.Errorf("Recoverer must be INNERMOST (last arg); got last=%q", last)
		}
	}

	// Global setup chain (Correlation/Trace/Tenant) must NOT include Recoverer.
	for _, c := range calls {
		if contains(c, "CorrelationID") >= 0 || contains(c, "TenantContext") >= 0 {
			if contains(c, "Recoverer") >= 0 {
				t.Errorf("global setup AddMiddleware must not contain Recoverer (would be outermost, panics skip Retry): %v", c)
			}
		}
	}
}

// TestArch_PoisonQueueWired asserts the DLQ machinery is present: router.go
// constructs a PoisonQueueWithFilter against DeadLetterTopic, and the
// NonRetryable sentinel is wired into the retry ShouldRetry decision.
//
// Scope: production — inspects router.go + errors.go (non-test substrate).
//
// arch-test:no-negative-fixture — RED→GREEN proof is the mutation test
// (delete the PoisonQueueWithFilter wiring or the NonRetryable sentinel →
// RED; revert → GREEN).
func TestArch_PoisonQueueWired(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(internalDir(t), "common", "messaging")
	var routerSrc, errorsSrc string
	walkGoFiles(t, dir, false, func(path string, src []byte) {
		switch filepath.Base(pathToSlash(path)) {
		case "router.go":
			routerSrc = string(src)
		case "errors.go":
			errorsSrc = string(src)
		}
	})
	if !strings.Contains(routerSrc, "PoisonQueueWithFilter") {
		t.Error("router.go does not construct a PoisonQueueWithFilter (no DLQ salvage)")
	}
	if !strings.Contains(routerSrc, "DeadLetterTopic") {
		t.Error("router.go does not reference DeadLetterTopic")
	}
	if !strings.Contains(routerSrc, "IsNonRetryable") {
		t.Error("router.go retry middleware does not consult IsNonRetryable (ShouldRetry)")
	}
	if !strings.Contains(errorsSrc, "func NonRetryable(") || !strings.Contains(errorsSrc, "func IsNonRetryable(") {
		t.Error("messaging/errors.go missing NonRetryable / IsNonRetryable sentinel")
	}
}

// TestArch_InboxIsTransactional asserts the transactional inbox (ADR 0067
// Phase-4) is wired: the receiver exposes WrapTx that opens a tx via
// Transactor.WithinTx and dedups with INSERT ... ON CONFLICT DO NOTHING, and
// Router.AddCqrsHandler routes DB handlers through
// TransactionalIdempotencyMiddleware. The middleware ORDER (txn idempotency
// INSIDE Retry, so each retry gets a fresh tx) is enforced by
// TestArch_MessagingMiddlewareOrderResilient.
//
// Scope: production — inspects messaging/inbox.go + router.go.
//
// arch-test:no-synctest — static text analysis.
//
// arch-test:no-negative-fixture — RED→GREEN proof is the mutation test
// (drop WrapTx's WithinTx / ON CONFLICT, or revert AddCqrsHandler to the
// non-transactional middleware → RED; revert → GREEN).
func TestArch_InboxIsTransactional(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(internalDir(t), "common", "messaging")
	var inboxSrc, routerSrc string
	walkGoFiles(t, dir, false, func(path string, src []byte) {
		switch filepath.Base(pathToSlash(path)) {
		case "inbox.go":
			inboxSrc = string(src)
		case "router.go":
			routerSrc = string(src)
		}
	})
	if !strings.Contains(inboxSrc, "func (r *IdempotentReceiver) WrapTx(") {
		t.Error("inbox.go missing WrapTx — the transactional inbox path")
	}
	if !strings.Contains(inboxSrc, "WithinTx") {
		t.Error("inbox.go WrapTx must open a tx via Transactor.WithinTx")
	}
	if !strings.Contains(inboxSrc, "ON CONFLICT DO NOTHING") {
		t.Error("inbox.go insertDedup must use INSERT ... ON CONFLICT DO NOTHING")
	}
	if !strings.Contains(routerSrc, "TransactionalIdempotencyMiddleware") {
		t.Error("router.go AddCqrsHandler must route DB handlers through TransactionalIdempotencyMiddleware")
	}
}
