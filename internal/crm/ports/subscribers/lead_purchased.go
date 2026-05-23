package subscribers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/ThreeDotsLabs/watermill/message"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

// HandlerName constants — CI-stable per messaging.md "stable handler
// names". Changing one of these makes every previously-processed
// message "fresh" against the inbox dedup table.
const (
	HandlerIngestLeadPurchased = "crm.subscribers.IngestLeadPurchased"
)

// PurchasedLeadIngestor is the CRM-side subscriber that turns
// `platform.lead-purchased.v1` envelopes into CrmLead aggregates via
// the [command.IngestPurchasedLeadHandler] command. Idempotent — the
// natural-key (PurchaseID) check in the command dedups across broker
// replays + cold rebuilds.
//
// Failure modes (Watermill canon — must-succeed):
//   - JSON decode failure → return error → Watermill retries (a
//     malformed payload is a producer-side bug; the inbox dedup means
//     the retry won't double-spend the row).
//   - Command failure → return error → retry.
//   - Topic mismatch (handler wired against wrong event type) →
//     short-circuit silently per the established subscriber pattern.
type PurchasedLeadIngestor struct {
	cmd command.IngestPurchasedLeadHandler
	log *slog.Logger
}

// NewPurchasedLeadIngestor wires the subscriber. Both args required.
func NewPurchasedLeadIngestor(cmd command.IngestPurchasedLeadHandler, log *slog.Logger) *PurchasedLeadIngestor {
	if log == nil {
		log = slog.Default()
	}
	return &PurchasedLeadIngestor{cmd: cmd, log: log}
}

// Handle decodes the envelope + dispatches to the command handler.
// Returns nil on duplicate (the command's AlreadyExisted result short-
// circuits without error) — Watermill ACKs the message.
func (h *PurchasedLeadIngestor) Handle(ctx context.Context, _ string, msg *message.Message) error {
	if msg.Metadata.Get(messaging.HeaderEventType) != LeadPurchasedTopic {
		// Topic-mismatch on the shared platform.events topic — not for us.
		return nil
	}
	var evt LeadPurchasedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("crm subscribers: decode %s: %w", LeadPurchasedTopic, err)
	}
	out, err := h.cmd.Handle(ctx, command.IngestPurchasedLeadCommand{
		PurchaseID:              evt.PurchaseID,
		TenantID:                evt.TenantID.String(),
		PlatformLeadID:          evt.PlatformLeadID,
		PurchasedByMembershipID: evt.PurchasedByMembershipID,
		Snapshot:                snapshotFromV1(evt),
	})
	if err != nil {
		return fmt.Errorf("crm subscribers: ingest: %w", err)
	}
	if out.AlreadyExisted {
		h.log.InfoContext(ctx, "crm: lead-purchased duplicate (idempotency hit)",
			"purchase_id", evt.PurchaseID, "lead_id", out.LeadID.String())
		return nil
	}
	h.log.InfoContext(ctx, "crm: lead ingested",
		"purchase_id", evt.PurchaseID, "lead_id", out.LeadID.String(),
		"tenant_id", evt.TenantID.String())
	return nil
}

// snapshotFromV1 converts the wire payload into the domain factory's
// [crmlead.PurchaseSnapshot] shape. Just field-by-field copy; the
// domain factory does the validation.
func snapshotFromV1(evt LeadPurchasedV1) crmlead.PurchaseSnapshot {
	s := evt.LeadSnapshot
	return crmlead.PurchaseSnapshot{
		PurchaseID:              evt.PurchaseID,
		PlatformLeadID:          evt.PlatformLeadID,
		PurchasedByMembershipID: evt.PurchasedByMembershipID,
		ContactName:             s.ContactName,
		MobileE164:              s.MobileE164,
		Email:                   s.Email,
		PinCode:                 s.PinCode,
		City:                    s.City,
		District:                s.District,
		State:                   s.State,
		Street:                  s.Street,
		HasDrugLicence:          s.HasDrugLicence,
		HasGst:                  s.HasGst,
		GstNumber:               s.GstNumber,
		HasPan:                  s.HasPan,
		PanNumber:               s.PanNumber,
		BusinessType:            s.BusinessType,
		MedicineSystem:          s.MedicineSystem,
		ProductRanges:           s.ProductRanges,
		DosageForms:             s.DosageForms,
		OrderValue:              s.OrderValue,
		BuyTimeline:             s.BuyTimeline,
	}
}
