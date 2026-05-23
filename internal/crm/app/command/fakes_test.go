package command_test

import (
	"context"
	"errors"
	"sync"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/crm/domain/assignmenthistory"
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

// fakeLeads is an in-memory crmlead.Repository for unit tests. Safe
// for concurrent use; emits no outbox events (caller drains via
// PullEvents on the held aggregate if needed).
type fakeLeads struct {
	mu                 sync.Mutex
	byID               map[crmlead.ID]*crmlead.CrmLead
	byPurchase         map[string]crmlead.ID
	emittedEventsByLead map[crmlead.ID][]crmlead.Event
}

func newFakeLeads() *fakeLeads {
	return &fakeLeads{
		byID:                map[crmlead.ID]*crmlead.CrmLead{},
		byPurchase:          map[string]crmlead.ID{},
		emittedEventsByLead: map[crmlead.ID][]crmlead.Event{},
	}
}

func (r *fakeLeads) Add(_ context.Context, l *crmlead.CrmLead) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byID[l.ID()]; exists {
		return errors.New("fake: lead already exists")
	}
	if l.SourcePurchaseID() != "" {
		if _, dup := r.byPurchase[l.SourcePurchaseID()]; dup {
			return errors.New("fake: source_purchase_id collision")
		}
		r.byPurchase[l.SourcePurchaseID()] = l.ID()
	}
	r.byID[l.ID()] = l
	r.emittedEventsByLead[l.ID()] = append(r.emittedEventsByLead[l.ID()], l.PullEvents()...)
	return nil
}

func (r *fakeLeads) UpdateByID(_ context.Context, id crmlead.ID, fn func(*crmlead.CrmLead) (bool, error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.byID[id]
	if !ok {
		return crmlead.ErrNotFound
	}
	persist, err := fn(l)
	if err != nil {
		return err
	}
	if persist {
		r.emittedEventsByLead[id] = append(r.emittedEventsByLead[id], l.PullEvents()...)
	} else {
		// Drain emitted-but-not-persisted to keep state clean.
		_ = l.PullEvents()
	}
	return nil
}

func (r *fakeLeads) GetByID(_ context.Context, id crmlead.ID) (*crmlead.CrmLead, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.byID[id]
	if !ok {
		return nil, crmlead.ErrNotFound
	}
	return l, nil
}

func (r *fakeLeads) GetBySourcePurchaseID(_ context.Context, purchaseID string) (*crmlead.CrmLead, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byPurchase[purchaseID]
	if !ok {
		return nil, crmlead.ErrNotFound
	}
	return r.byID[id], nil
}

func (r *fakeLeads) ListPage(_ context.Context, _ crmlead.ListFilter, _ pagination.Cursor, pageSize int) (pagination.Page[*crmlead.CrmLead], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*crmlead.CrmLead, 0, len(r.byID))
	for _, l := range r.byID {
		out = append(out, l)
	}
	if pageSize > 0 && len(out) > pageSize {
		out = out[:pageSize]
	}
	return pagination.Page[*crmlead.CrmLead]{Items: out, HasMore: false}, nil
}

// fakeCallLogs is an in-memory calllog.Repository.
type fakeCallLogs struct {
	mu   sync.Mutex
	byID map[calllog.ID]*calllog.CallLog
}

func newFakeCallLogs() *fakeCallLogs {
	return &fakeCallLogs{byID: map[calllog.ID]*calllog.CallLog{}}
}

func (r *fakeCallLogs) Add(_ context.Context, c *calllog.CallLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[c.ID()] = c
	_ = c.PullEvents() // discard for fake
	return nil
}

func (r *fakeCallLogs) GetByID(_ context.Context, id calllog.ID) (*calllog.CallLog, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.byID[id]
	if !ok {
		return nil, calllog.ErrNotFound
	}
	return c, nil
}

func (r *fakeCallLogs) ListByLead(_ context.Context, leadID crmlead.ID) ([]*calllog.CallLog, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []*calllog.CallLog{}
	for _, c := range r.byID {
		if c.LeadID() == leadID {
			out = append(out, c)
		}
	}
	return out, nil
}

// fakeHistory is an in-memory assignmenthistory.Repository.
type fakeHistory struct {
	mu   sync.Mutex
	byID map[assignmenthistory.ID]*assignmenthistory.Entry
}

func newFakeHistory() *fakeHistory {
	return &fakeHistory{byID: map[assignmenthistory.ID]*assignmenthistory.Entry{}}
}

func (r *fakeHistory) Add(_ context.Context, e *assignmenthistory.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[e.ID()] = e
	return nil
}

func (r *fakeHistory) GetByID(_ context.Context, id assignmenthistory.ID) (*assignmenthistory.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	if !ok {
		return nil, assignmenthistory.ErrNotFound
	}
	return e, nil
}

func (r *fakeHistory) ListByLead(_ context.Context, leadID crmlead.ID) ([]*assignmenthistory.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []*assignmenthistory.Entry{}
	for _, e := range r.byID {
		if e.LeadID() == leadID {
			out = append(out, e)
		}
	}
	return out, nil
}

// fakeUoW satisfies pg.UnitOfWork without opening a real tx. Just
// runs the closure inline.
type fakeUoW struct{}

func (fakeUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(ctx context.Context) error) error {
	return fn(ctx)
}
