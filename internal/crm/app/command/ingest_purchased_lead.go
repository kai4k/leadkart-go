// Package command holds CRM command handlers per TDL canon — one file
// per use case, one struct per handler with NewX + Handle.
//
// Per ADR 0047 boundary discipline: this package may NOT import
// internal/crm/adapters/db, pgx, pgxpool, pgtype, or internal/crm/adapters
// (concrete). Handlers depend on domain repository interfaces and the
// platform `pg.UnitOfWork` interface only.
package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrPurchaseAlreadyIngested surfaces when the subscriber receives a
// retry / replay for a purchase that has already been turned into a
// CrmLead. Per ADR 0060 the natural-key (source_purchase_id UNIQUE)
// makes the path safely retriable — this typed error lets the
// subscriber log "duplicate, dropped" + return nil so Watermill marks
// the message as ACK'd.
var ErrPurchaseAlreadyIngested = errors.New("crm ingest: purchase already ingested")

// IngestPurchasedLeadCommand carries the data the subscriber decodes
// from `platform.lead-purchased.v1`. TenantID is the OWNING tenant —
// distinct from the Platform tenant; the subscriber sets it from the
// envelope's TenantID claim.
//
// PurchaseID is the natural idempotency key per ADR 0060 — the handler
// SHORT-CIRCUITS when GetBySourcePurchaseID(ctx, PurchaseID) returns a
// non-nil lead, leaving the existing row untouched.
type IngestPurchasedLeadCommand struct {
	PurchaseID              string
	TenantID                tenant.ID
	PlatformLeadID          string
	PurchasedByMembershipID string
	Snapshot                crmlead.PurchaseSnapshot
}

// IngestPurchasedLeadResult is the handler's success payload. LeadID is
// the freshly-created lead's ID (or the existing one's ID on duplicate
// — caller can distinguish via the AlreadyExisted flag).
type IngestPurchasedLeadResult struct {
	LeadID         crmlead.ID
	AlreadyExisted bool
}

// IngestPurchasedLeadHandler ingests a purchased lead into the
// consuming tenant's CRM. Idempotent: a same-PurchaseID retry returns
// the existing lead's ID with AlreadyExisted=true + no events emitted.
type IngestPurchasedLeadHandler struct {
	leads     crmlead.Repository
	now       func() time.Time
	newLeadID func() crmlead.ID
}

// NewIngestPurchasedLeadHandler wires the handler against the lead
// repository interface. `now` is the injected wall-clock (Pure Domain
// canon — ADR 0047); nil → time.Now.
//
// newLeadID is the CrmLead ID factory per the
// `TestArch_HandlersInjectIDFactory` discipline. Production passes
// `func() crmlead.ID { return crmlead.ID(ids.NewV7().String()) }`;
// tests inject a deterministic counter so the minted ID is pinnable.
func NewIngestPurchasedLeadHandler(leads crmlead.Repository, now func() time.Time, newLeadID func() crmlead.ID) IngestPurchasedLeadHandler {
	if leads == nil {
		panic("command: NewIngestPurchasedLeadHandler leads repository required")
	}
	if newLeadID == nil {
		panic("command: NewIngestPurchasedLeadHandler newLeadID required")
	}
	if now == nil {
		now = time.Now
	}
	return IngestPurchasedLeadHandler{leads: leads, now: now, newLeadID: newLeadID}
}

// Handle is the subscriber entrypoint. Returns nil + AlreadyExisted=true
// on duplicate; otherwise creates a fresh CrmLead + persists.
func (h IngestPurchasedLeadHandler) Handle(ctx context.Context, cmd IngestPurchasedLeadCommand) (IngestPurchasedLeadResult, error) {
	if cmd.PurchaseID == "" {
		return IngestPurchasedLeadResult{}, errors.New("crm ingest: purchase id required")
	}
	if cmd.TenantID == "" {
		return IngestPurchasedLeadResult{}, errors.New("crm ingest: tenant id required")
	}

	// Idempotency check first — survives broker replay AND cold rebuild.
	existing, err := h.leads.GetBySourcePurchaseID(ctx, cmd.PurchaseID)
	switch {
	case err == nil && existing != nil:
		return IngestPurchasedLeadResult{LeadID: existing.ID(), AlreadyExisted: true}, nil
	case errors.Is(err, crmlead.ErrNotFound):
		// expected path — fall through to create
	case err != nil:
		return IngestPurchasedLeadResult{}, fmt.Errorf("crm ingest: lookup by purchase id: %w", err)
	}

	// Hydrate the snapshot with provenance fields the wire envelope
	// supplied at the top level (PurchaseID + PlatformLeadID +
	// PurchasedByMembershipID) — the subscriber may pass them either
	// on the snapshot OR on the command top-level; we normalise here.
	snap := cmd.Snapshot
	if snap.PurchaseID == "" {
		snap.PurchaseID = cmd.PurchaseID
	}
	if snap.PlatformLeadID == "" {
		snap.PlatformLeadID = cmd.PlatformLeadID
	}
	if snap.PurchasedByMembershipID == "" {
		snap.PurchasedByMembershipID = cmd.PurchasedByMembershipID
	}

	leadID := h.newLeadID()
	lead, err := crmlead.NewFromPurchaseSnapshot(leadID, cmd.TenantID, snap, h.now())
	if err != nil {
		return IngestPurchasedLeadResult{}, fmt.Errorf("crm ingest: factory: %w", err)
	}
	if err := h.leads.Add(ctx, lead); err != nil {
		return IngestPurchasedLeadResult{}, fmt.Errorf("crm ingest: persist: %w", err)
	}
	return IngestPurchasedLeadResult{LeadID: leadID, AlreadyExisted: false}, nil
}
