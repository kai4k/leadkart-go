// single_approach_arch_test.go — gates that enforce ONE canonical way of
// doing a thing, so a second approach can't drift in. These close the
// "two-sided invariant checked on one side" gaps that let real defects
// (the lead-purchase topic mismatch, the literal metadata-header drift)
// ship undetected.

package architecture_test

import (
	"go/ast"
	"go/token"
	"strings"
	"testing"
)

// TestArch_MessageMetadataUsesHeaderConstants bans string literals as the
// key of a msg.Metadata.Set / .Get call. Producers (forwarders) and
// consumers (middleware, subscribers) must use the messaging.Header*
// constants so a rename can't silently desync them — three of the four
// outbox forwarders previously hand-typed "event_type"/"tenant_id", one
// typo away from breaking routing for that module.
//
// Scope: production — metadata keys are set/read by production forwarders
// and middleware; test files construct envelopes freely.
//
// arch-test:no-synctest — purely-static analysis test.
//
// arch-test:no-negative-fixture — the three forwarders that used literal
// keys (pre-fix on this branch) are the recorded RED→GREEN proof; a
// fixture file would itself be the banned shape.
func TestArch_MessageMetadataUsesHeaderConstants(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
		key  string
	}
	var violations []violation

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		slash := pathToSlash(path)
		if strings.Contains(slash, "/adapters/db/") { // generated
			return
		}
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Set" && sel.Sel.Name != "Get") {
				return true
			}
			// Receiver must be `<x>.Metadata` so we only match
			// message.Metadata.Set/Get, not arbitrary Set/Get calls.
			recv, ok := sel.X.(*ast.SelectorExpr)
			if !ok || recv.Sel.Name != "Metadata" {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			violations = append(violations, violation{
				file: slash,
				line: fset.Position(lit.Pos()).Line,
				key:  lit.Value,
			})
			return true
		})
	})

	if len(violations) > 0 {
		t.Errorf("%d msg.Metadata.Set/Get call(s) using a string-literal key — use the messaging.Header* constants so producer and consumer share one source of truth:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d — %s", v.file, v.line, v.key)
		}
	}
}

// TestArch_EventTypeFilterUsesConstant bans comparing a
// msg.Metadata.Get(HeaderEventType) result against a string LITERAL. The
// subscriber must compare against an imported topic constant (the
// producer's exported Topic), otherwise the producer and consumer
// strings can diverge — exactly the underscore/hyphen mismatch that
// silently dropped the entire Platform→CRM lead-purchase flow.
//
// Scope: production — subscriber filters live in production ports code;
// test files may compare against literals freely.
//
// arch-test:no-synctest — purely-static analysis test.
//
// arch-test:no-negative-fixture — the CRM subscriber that compared
// against the literal "platform.lead-purchased.v1" (pre-fix on this
// branch) is the recorded RED→GREEN proof.
func TestArch_EventTypeFilterUsesConstant(t *testing.T) {
	t.Parallel()

	type violation struct {
		file string
		line int
	}
	var violations []violation

	// getsEventType reports whether expr is a call
	// `<x>.Get(<...>HeaderEventType)` — the event_type lookup.
	getsEventType := func(expr ast.Expr) bool {
		call, ok := expr.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Get" {
			return false
		}
		argSel, ok := call.Args[0].(*ast.SelectorExpr)
		if ok && argSel.Sel.Name == "HeaderEventType" {
			return true
		}
		if id, ok := call.Args[0].(*ast.Ident); ok && id.Name == "HeaderEventType" {
			return true
		}
		return false
	}

	walkGoFiles(t, internalDir(t), false, func(path string, src []byte) {
		fset, f := parseFile(t, path, src)
		ast.Inspect(f, func(n ast.Node) bool {
			bin, ok := n.(*ast.BinaryExpr)
			if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
				return true
			}
			// One side gets event_type, the other must NOT be a literal.
			eventSide := getsEventType(bin.X) || getsEventType(bin.Y)
			if !eventSide {
				return true
			}
			for _, side := range []ast.Expr{bin.X, bin.Y} {
				if lit, ok := side.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					violations = append(violations, violation{
						file: pathToSlash(path),
						line: fset.Position(lit.Pos()).Line,
					})
				}
			}
			return true
		})
	})

	if len(violations) > 0 {
		t.Errorf("%d event_type filter(s) compared against a string literal — import and compare the producer's exported Topic constant so producer/consumer cannot drift:", len(violations))
		for _, v := range violations {
			t.Errorf("  %s:%d", v.file, v.line)
		}
	}
}
