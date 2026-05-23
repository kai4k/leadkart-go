package subscribers_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/ports/subscribers"
)

// fakeLeads is a minimal crmlead.Repository for the subscriber tests.
// Only the methods the command path touches are implemented.
type fakeLeads struct {
	mu          sync.Mutex
	byID        map[crmlead.ID]*crmlead.CrmLead
	byPurchase  map[string]crmlead.ID
}

func newFakeLeads() *fakeLeads {
	return &fakeLeads{
		byID:       map[crmlead.ID]*crmlead.CrmLead{},
		byPurchase: map[string]crmlead.ID{},
	}
}

func (r *fakeLeads) Add(_ context.Context, l *crmlead.CrmLead) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l.SourcePurchaseID() != "" {
		r.byPurchase[l.SourcePurchaseID()] = l.ID()
	}
	r.byID[l.ID()] = l
	_ = l.PullEvents()
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

func (r *fakeLeads) GetBySourcePurchaseID(_ context.Context, p string) (*crmlead.CrmLead, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byPurchase[p]
	if !ok {
		return nil, crmlead.ErrNotFound
	}
	return r.byID[id], nil
}

func (r *fakeLeads) UpdateByID(_ context.Context, _ crmlead.ID, _ func(*crmlead.CrmLead) (bool, error)) error {
	return nil
}

func (r *fakeLeads) ListPage(_ context.Context, _ crmlead.ListFilter, _ pagination.Cursor, _ int) (pagination.Page[*crmlead.CrmLead], error) {
	return pagination.Page[*crmlead.CrmLead]{}, nil
}

func buildEnvelope(t *testing.T, evt subscribers.LeadPurchasedV1) *message.Message {
	t.Helper()
	body, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	msg := message.NewMessage(uuid.NewString(), body)
	msg.Metadata.Set(messaging.HeaderEventType, subscribers.LeadPurchasedTopic)
	msg.Metadata.Set(messaging.HeaderTenantID, evt.TenantID.String())
	return msg
}

func validEvent(tenantID uuid.UUID, purchase string) subscribers.LeadPurchasedV1 {
	return subscribers.LeadPurchasedV1{
		PurchaseID:              purchase,
		TenantID:                tenantID,
		PlatformLeadID:          uuid.NewString(),
		PurchasedAt:             time.Now().UTC(),
		PurchasedByMembershipID: uuid.NewString(),
		AmountPaisa:             50000,
		LeadSnapshot: subscribers.LeadSnapshotV1{
			ContactName:    "Test Pharma",
			MobileE164:     "+919812345678",
			Email:          "x@example.com",
			PinCode:        "411001",
			City:           "Pune",
			District:       "Pune",
			State:          "Maharashtra",
			HasDrugLicence: true,
			HasGst:         true,
			BusinessType:   "PCD",
			MedicineSystem: "Allopathic",
			OrderValue:     "Upto25000",
			BuyTimeline:    "WithinWeek",
		},
	}
}

func TestPurchasedLeadIngestor_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	h := subscribers.NewPurchasedLeadIngestor(command.NewIngestPurchasedLeadHandler(leads), nil)
	tenantID := uuid.New()
	purchase := uuid.NewString()
	if err := h.Handle(t.Context(), "", buildEnvelope(t, validEvent(tenantID, purchase))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, err := leads.GetBySourcePurchaseID(t.Context(), purchase)
	if err != nil {
		t.Fatalf("GetBySourcePurchaseID: %v", err)
	}
	if got.TenantID() != tenantID.String() {
		t.Fatalf("tenant: %q", got.TenantID())
	}
}

func TestPurchasedLeadIngestor_IdempotentOnReplay(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	h := subscribers.NewPurchasedLeadIngestor(command.NewIngestPurchasedLeadHandler(leads), nil)
	tenantID := uuid.New()
	purchase := uuid.NewString()
	env := buildEnvelope(t, validEvent(tenantID, purchase))

	if err := h.Handle(t.Context(), "", env); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := h.Handle(t.Context(), "", env); err != nil {
		t.Fatalf("replay: %v", err)
	}
	// Still ONE lead.
	if len(leads.byPurchase) != 1 {
		t.Fatalf("byPurchase entries: %d", len(leads.byPurchase))
	}
}

func TestPurchasedLeadIngestor_WrongTopicShortCircuits(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	h := subscribers.NewPurchasedLeadIngestor(command.NewIngestPurchasedLeadHandler(leads), nil)
	msg := buildEnvelope(t, validEvent(uuid.New(), uuid.NewString()))
	msg.Metadata.Set(messaging.HeaderEventType, "platform.unrelated.v1")
	if err := h.Handle(t.Context(), "", msg); err != nil {
		t.Fatalf("Handle wrong topic: %v", err)
	}
	if len(leads.byPurchase) != 0 {
		t.Fatal("short-circuit failed: a lead was ingested for a non-matching topic")
	}
}

func TestPurchasedLeadIngestor_MalformedPayloadErrors(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	h := subscribers.NewPurchasedLeadIngestor(command.NewIngestPurchasedLeadHandler(leads), nil)
	msg := message.NewMessage(uuid.NewString(), []byte("{not json"))
	msg.Metadata.Set(messaging.HeaderEventType, subscribers.LeadPurchasedTopic)
	if err := h.Handle(t.Context(), "", msg); err == nil {
		t.Fatal("want decode error")
	}
}
