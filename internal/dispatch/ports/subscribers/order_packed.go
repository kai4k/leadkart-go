package subscribers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// HandlerName constants — CI-stable per messaging.md "stable handler
// names". Changing one of these makes every previously-processed
// message "fresh" against the inbox dedup table.
const (
	HandlerCreateConsignmentNote = "dispatch.subscribers.CreateConsignmentNoteOnOrderPacked"
)

// arch-test:idempotency-via-natural-key-precheck — dedup happens one
// call-frame down: [command.CreateConsignmentNoteHandler.Handle] runs
// GetByOrderID inside the same tx and short-circuits with
// AlreadyExisted=true on replay. The handler returns nil on that
// branch so Watermill ACKs the duplicate.

// OrderPackedIngestor turns `orders.order_packed.v1` envelopes into
// ConsignmentNote rows via [command.CreateConsignmentNoteHandler].
// Idempotent — the natural-key (OrderID) precheck inside the command
// short-circuits replays.
//
// Per ADR 0063 §4 fulfillment-saga: this subscriber is one of the
// dispatch-side participants in the order-fulfillment flow. The
// emitted ConsignmentNoteCreatedV1 propagates to subscribers in
// Notifications (operator alert) + future Carrier-integration
// subscribers (e.g. BlueDart API call).
type OrderPackedIngestor struct {
	cmd command.CreateConsignmentNoteHandler
	log *slog.Logger
}

// NewOrderPackedIngestor wires the subscriber. log is mandatory.
func NewOrderPackedIngestor(
	cmd command.CreateConsignmentNoteHandler,
	log *slog.Logger,
) *OrderPackedIngestor {
	if log == nil {
		panic("subscribers: NewOrderPackedIngestor log required")
	}
	return &OrderPackedIngestor{cmd: cmd, log: log}
}

// Handle decodes the envelope + dispatches to the command handler.
func (h *OrderPackedIngestor) Handle(ctx context.Context, _ string, msg *message.Message) error {
	if msg.Metadata.Get(messaging.HeaderEventType) != OrderPackedTopic {
		// Not our event — the orders.events topic carries every Orders
		// integration event; we only care about order_packed.
		return nil
	}
	var evt OrderPackedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		// retry — malformed envelope is a producer-side bug; the
		// natural-key idempotency check makes the retry-after-fix safe.
		return fmt.Errorf("dispatch subscribers: decode %s: %w", OrderPackedTopic, err)
	}
	if strings.TrimSpace(evt.OrderID) == "" || strings.TrimSpace(evt.TenantID) == "" {
		// retry — defensively reject malformed payload (missing IDs).
		// Producer-side bug; same rationale as decode failure.
		return errors.New("dispatch subscribers: order_packed envelope missing tenant_id or order_id")
	}

	var eta *time.Time
	if evt.ExpectedDeliveryAtUTC != nil && !evt.ExpectedDeliveryAtUTC.IsZero() {
		t := evt.ExpectedDeliveryAtUTC.UTC()
		eta = &t
	}
	out, err := h.cmd.Handle(ctx, command.CreateConsignmentNoteCommand{
		TenantID:              tenant.ID(evt.TenantID),
		OrderID:               consignmentnote.OrderID(evt.OrderID),
		CarrierName:           defaultCarrierName(evt.CarrierName),
		BoxCount:              evt.BoxCount,
		WeightGrams:           evt.WeightGrams,
		ExpectedDeliveryAt:    eta,
		CreatedByMembershipID: membership.ID(evt.PackedByMembershipID),
	})
	if err != nil {
		// retry — DB hiccup or transient; idempotent so safe.
		return fmt.Errorf("dispatch subscribers: create consignment note: %w", err)
	}
	if out.AlreadyExisted {
		h.log.InfoContext(ctx, "dispatch: consignment note already existed (idempotency hit)",
			"order_id", evt.OrderID, "consignment_note_id", out.ConsignmentNoteID.String())
		return nil
	}
	h.log.InfoContext(ctx, "dispatch: consignment note slot created",
		"order_id", evt.OrderID, "consignment_note_id", out.ConsignmentNoteID.String(),
		"carrier", defaultCarrierName(evt.CarrierName))
	return nil
}

// defaultCarrierName returns "Unassigned" when the OrderPacked payload
// doesn't pre-select a carrier. Operator-side reassignment via a
// separate HTTP route updates the row in-place; the pending state
// makes this safe.
func defaultCarrierName(supplied string) string {
	s := strings.TrimSpace(supplied)
	if s == "" {
		return "Unassigned"
	}
	return s
}
