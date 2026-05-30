package subscribers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/ThreeDotsLabs/watermill/message"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
)

// arch-test:idempotency-via-append-only-log — emits a WARN slog record only; duplicate dispatch produces duplicate audit lines which is the correct (lossy-tolerant) behaviour for SIEM ingest. No state mutation to dedup.

// ReuseDetectedSIEM is the security-incident subscriber for RFC 9700
// §4.13 reuse-detection events. Fires only when a
// `identity.refresh_token_family_revoked.v1` arrives with
// Reason="reuse_detected".
//
// v0.2 surface: structured WARN log via slog. The Notifications
// module (v0.6) will subscribe to a sibling alert event this handler
// emits, fanning out to operator email/SMS + the user's "we detected
// suspicious activity on your account" notification per
// `security.md` SIEM canon.
type ReuseDetectedSIEM struct {
	log *slog.Logger
}

// NewReuseDetectedSIEM wires the subscriber. log is mandatory — pass
// slog.New(slog.NewTextHandler(io.Discard, nil)) in tests that don't
// want output. Mat Ryer canon (NewServer takes the logger explicitly);
// no nil-fallback.
func NewReuseDetectedSIEM(log *slog.Logger) *ReuseDetectedSIEM {
	if log == nil {
		panic("subscribers: NewReuseDetectedSIEM log required")
	}
	return &ReuseDetectedSIEM{log: log}
}

// Handle is the [messaging.SubscriberHandler]. Filters by event_type
// header + Reason field.
func (h *ReuseDetectedSIEM) Handle(
	ctx context.Context, _ string, msg *message.Message,
) error {
	expected := integrationevents.RefreshTokenFamilyRevokedV1{}.Topic()
	if msg.Metadata.Get(messaging.HeaderEventType) != expected {
		return nil
	}
	var evt integrationevents.RefreshTokenFamilyRevokedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("subscribers: decode %s: %w", expected, err)
	}
	if evt.Reason != "reuse_detected" {
		return nil
	}
	// Structured WARN log — operator dashboards + log-aggregators
	// (Loki/Datadog/etc.) alert on this combination.
	h.log.WarnContext(ctx,
		"RFC 9700 §4.13 refresh-token reuse detected — security incident",
		"family_id", evt.FamilyID,
		"person_id", evt.PersonID,
		"tenant_id", evt.TenantIDClaim,
		"occurred_at_utc", evt.OccurredAtUTC,
	)
	return nil
}
