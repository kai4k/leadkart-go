// end_impersonation_test.go — handler-unit tests for the
// EndImpersonationSessionHandler. Covers every branch of
// impersonation.go::EndImpersonationSessionHandler.Handle per ADR
// 0062 §6 (handler-unit MANY).
//
// Sibling to create_impersonation_session_test.go which covers the
// CreateImpersonationSessionHandler. Kept in a separate file so the
// two handlers' test surfaces stay independently navigable + the
// shared impSigningKey + newImpersonationIssuer helpers (defined in
// create_impersonation_session_test.go) can be reused without
// duplication.
//
// Wired against adapters.ImpersonationInMemoryStore (the canonical
// in-process impersonation.Store impl) + an inline error-injecting
// decorator for the lookup/delete infra-error branches.

package command_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/impersonation"
)

// impStoreWithErr wraps adapters.ImpersonationInMemoryStore with
// error-injection seams for the Get + Delete infra-error branches.
// Scoped to this file (per coordination warning — no shared
// fakes).
type impStoreWithErr struct {
	*adapters.ImpersonationInMemoryStore
	getErr    error
	deleteErr error
}

func (s *impStoreWithErr) Get(ctx context.Context, id string) (impersonation.Session, error) {
	if s.getErr != nil {
		return impersonation.Session{}, s.getErr
	}
	return s.ImpersonationInMemoryStore.Get(ctx, id)
}

func (s *impStoreWithErr) Delete(ctx context.Context, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.ImpersonationInMemoryStore.Delete(ctx, id)
}

// seedSession persists a fresh session for the supplied operator
// into the in-memory store + returns the session ID.
func seedSession(t *testing.T, store impersonation.Store, operatorID string) string {
	t.Helper()
	sess, err := impersonation.NewSession(
		operatorID,
		"22222222-2222-2222-2222-222222222222", // target tenant
		"investigate-customer-issue-ticket-1234",
		30*time.Minute,
		testNow,
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := store.Put(t.Context(), sess); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return sess.ID()
}

// TestEndImpersonationSession_HappyPath_DeletesSession covers the
// success branch: session exists + owned by caller → deleted from
// the store.
func TestEndImpersonationSession_HappyPath_DeletesSession(t *testing.T) {
	t.Parallel()
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow })
	const operatorID = "11111111-1111-1111-1111-111111111111"
	sessionID := seedSession(t, store, operatorID)
	h := command.NewEndImpersonationSessionHandler(store)

	if err := h.Handle(t.Context(), command.EndImpersonationSessionCommand{
		OperatorID: operatorID,
		SessionID:  sessionID,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// State assertion: session no longer in store.
	if _, err := store.Get(t.Context(), sessionID); !errors.Is(err, impersonation.ErrSessionNotFound) {
		t.Fatalf("Get post-delete: got %v, want ErrSessionNotFound", err)
	}
}

// TestEndImpersonationSession_RejectsMissingInputs is the input
// validation table for the two mandatory fields (OperatorID +
// SessionID). Each rejection fires BEFORE any store call.
func TestEndImpersonationSession_RejectsMissingInputs(t *testing.T) {
	t.Parallel()
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow })
	h := command.NewEndImpersonationSessionHandler(store)

	cases := []struct {
		name string
		cmd  command.EndImpersonationSessionCommand
	}{
		{
			name: "missing operator id",
			cmd: command.EndImpersonationSessionCommand{
				SessionID: "11111111-1111-1111-1111-111111111111",
			},
		},
		{
			name: "missing session id",
			cmd: command.EndImpersonationSessionCommand{
				OperatorID: "11111111-1111-1111-1111-111111111111",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := h.Handle(t.Context(), c.cmd)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// TestEndImpersonationSession_SessionNotFound_IdempotentNil covers
// the ErrSessionNotFound branch — handler is idempotent (already-
// deleted session returns nil).
func TestEndImpersonationSession_SessionNotFound_IdempotentNil(t *testing.T) {
	t.Parallel()
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow })
	h := command.NewEndImpersonationSessionHandler(store)

	err := h.Handle(t.Context(), command.EndImpersonationSessionCommand{
		OperatorID: "11111111-1111-1111-1111-111111111111",
		SessionID:  "deadbeef-dead-beef-dead-beefdeadbeef",
	})
	if err != nil {
		t.Fatalf("Handle: got %v, want nil (idempotent)", err)
	}
}

// TestEndImpersonationSession_LookupError_Wrapped covers the
// non-ErrSessionNotFound branch on store.Get — a real infra error
// surfaces as a wrapped "end_impersonation_session: lookup" error.
func TestEndImpersonationSession_LookupError_Wrapped(t *testing.T) {
	t.Parallel()
	store := &impStoreWithErr{
		ImpersonationInMemoryStore: adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow }),
		getErr:                     errors.New("redis: connection refused"),
	}
	h := command.NewEndImpersonationSessionHandler(store)

	err := h.Handle(t.Context(), command.EndImpersonationSessionCommand{
		OperatorID: "11111111-1111-1111-1111-111111111111",
		SessionID:  "any-session-id",
	})
	if err == nil {
		t.Fatal("expected wrapped lookup error, got nil")
	}
	if !errors.Is(err, store.getErr) {
		t.Errorf("got %v, want chain containing %v", err, store.getErr)
	}
	if !strings.Contains(err.Error(), "lookup") {
		t.Errorf("err msg %q missing 'lookup' prefix", err.Error())
	}
}

// TestEndImpersonationSession_DifferentOperator_IdempotentNil covers
// the cross-operator collapse branch: an operator attempting to
// delete a session that exists but belongs to a different operator
// gets nil (NOT 403/404 distinguishable). Enumeration-safety
// per security.md.
//
// State assertion: the session is STILL in the store after the
// rejected delete — proves the cross-operator path doesn't trip
// store.Delete.
func TestEndImpersonationSession_DifferentOperator_IdempotentNil(t *testing.T) {
	t.Parallel()
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow })
	const realOwner = "11111111-1111-1111-1111-111111111111"
	const otherOp = "22222222-2222-2222-2222-222222222222"
	sessionID := seedSession(t, store, realOwner)
	h := command.NewEndImpersonationSessionHandler(store)

	err := h.Handle(t.Context(), command.EndImpersonationSessionCommand{
		OperatorID: otherOp,
		SessionID:  sessionID,
	})
	if err != nil {
		t.Fatalf("Handle: got %v, want nil (cross-operator collapses to idempotent)", err)
	}
	// State assertion: session still present — the delete was NOT executed.
	if _, getErr := store.Get(t.Context(), sessionID); getErr != nil {
		t.Fatalf("Get post-attempted-cross-delete: got %v, want session still present", getErr)
	}
}

// TestEndImpersonationSession_DeleteError_Wrapped covers the
// store.Delete error branch — infra error surfaces as
// "end_impersonation_session: delete: <wrapped>".
func TestEndImpersonationSession_DeleteError_Wrapped(t *testing.T) {
	t.Parallel()
	store := &impStoreWithErr{
		ImpersonationInMemoryStore: adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow }),
	}
	const operatorID = "11111111-1111-1111-1111-111111111111"
	sessionID := seedSession(t, store.ImpersonationInMemoryStore, operatorID)
	store.deleteErr = errors.New("redis: TTL expired between get + delete")
	h := command.NewEndImpersonationSessionHandler(store)

	err := h.Handle(t.Context(), command.EndImpersonationSessionCommand{
		OperatorID: operatorID,
		SessionID:  sessionID,
	})
	if err == nil {
		t.Fatal("expected wrapped Delete error, got nil")
	}
	if !errors.Is(err, store.deleteErr) {
		t.Errorf("got %v, want chain containing %v", err, store.deleteErr)
	}
	if !strings.Contains(err.Error(), "delete") {
		t.Errorf("err msg %q missing 'delete' prefix", err.Error())
	}
}

// TestNewEndImpersonationSessionHandler_PanicsOnNilStore locks the
// wiring contract: store is mandatory at composition time.
func TestNewEndImpersonationSessionHandler_PanicsOnNilStore(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil store")
		}
	}()
	_ = command.NewEndImpersonationSessionHandler(nil) // arch-test:ignore-err - test fixture setup
}
