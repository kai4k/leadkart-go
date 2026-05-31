// impersonation_inmemory_store.go — process-local [impersonation.Store]
// (ADR 0051, Wave 9.1b). Adequate for single-instance and tests; swap
// to a Redis-backed implementation for multi-replica at composition time.

package adapters

import (
	"context"
	"sync"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/impersonation"
)

// ImpersonationInMemoryStore is a map-backed [impersonation.Store].
// Expired sessions are lazily evicted in Get and ListByOperator.
type ImpersonationInMemoryStore struct {
	mu       sync.Mutex
	sessions map[string]impersonation.Session
	now      func() time.Time
}

// NewImpersonationInMemoryStore constructs an empty store. Pass time.Now
// in production or a fixed clock in tests; nil defaults to time.Now.
func NewImpersonationInMemoryStore(now func() time.Time) *ImpersonationInMemoryStore {
	if now == nil {
		now = time.Now
	}
	return &ImpersonationInMemoryStore{
		sessions: make(map[string]impersonation.Session),
		now:      now,
	}
}

var _ impersonation.Store = (*ImpersonationInMemoryStore)(nil)

// Put satisfies [impersonation.Store].
func (s *ImpersonationInMemoryStore) Put(_ context.Context, sess impersonation.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID()] = sess
	return nil
}

// Get satisfies [impersonation.Store]. Evicts and returns
// [impersonation.ErrSessionNotFound] for expired sessions.
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

// ListByOperator satisfies [impersonation.Store]. Skips and evicts
// expired sessions encountered during iteration.
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
