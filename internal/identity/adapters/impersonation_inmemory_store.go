// impersonation_inmemory_store.go — process-local in-memory adapter
// for [impersonation.Store].
//
// Per ADR 0051 (Wave 9.1b): MOVED from `internal/common/impersonation/`
// to here per TDL single-module rule. The Store interface + Session
// value type live in `internal/identity/domain/impersonation/`.
//
// Adequate for single-instance deployments + integration tests;
// production multi-replica needs a Redis-backed implementation
// behind the same [impersonation.Store] interface — wiring-time
// choice in the composition root.

package adapters

import (
	"context"
	"sync"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/impersonation"
)

// ImpersonationInMemoryStore is a process-local map-backed
// [impersonation.Store]. The store auto-evicts expired sessions
// inside Get / ListByOperator — no separate sweep goroutine needed.
type ImpersonationInMemoryStore struct {
	mu       sync.Mutex
	sessions map[string]impersonation.Session
	now      func() time.Time
}

// NewImpersonationInMemoryStore constructs an empty store. now is the
// clock source; pass time.Now in production, a fixed clock in tests.
func NewImpersonationInMemoryStore(now func() time.Time) *ImpersonationInMemoryStore {
	if now == nil {
		now = time.Now
	}
	return &ImpersonationInMemoryStore{
		sessions: make(map[string]impersonation.Session),
		now:      now,
	}
}

// Compile-time assertion: *ImpersonationInMemoryStore satisfies
// [impersonation.Store].
var _ impersonation.Store = (*ImpersonationInMemoryStore)(nil)

// Put satisfies [impersonation.Store].
func (s *ImpersonationInMemoryStore) Put(_ context.Context, sess impersonation.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID()] = sess
	return nil
}

// Get satisfies [impersonation.Store]. Expired sessions are
// auto-evicted + return [impersonation.ErrSessionNotFound].
func (s *ImpersonationInMemoryStore) Get(_ context.Context, id string) (impersonation.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return impersonation.Session{}, impersonation.ErrSessionNotFound
	}
	if sess.IsExpired(s.now()) {
		delete(s.sessions, id)
		return impersonation.Session{}, impersonation.ErrSessionNotFound
	}
	return sess, nil
}

// Delete satisfies [impersonation.Store]. Idempotent.
func (s *ImpersonationInMemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return nil
}

// ListByOperator satisfies [impersonation.Store]. Skips + evicts
// expired sessions it encounters.
func (s *ImpersonationInMemoryStore) ListByOperator(_ context.Context, operatorID string) ([]impersonation.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make([]impersonation.Session, 0, len(s.sessions))
	for id, sess := range s.sessions {
		if sess.IsExpired(now) {
			delete(s.sessions, id)
			continue
		}
		if sess.OperatorID() == operatorID {
			out = append(out, sess)
		}
	}
	return out, nil
}
