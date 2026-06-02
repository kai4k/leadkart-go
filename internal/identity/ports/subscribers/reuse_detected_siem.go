package subscribers

import (
	"context"
	"log/slog"

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

// Handle is the typed cqrs handler for
// `identity.refresh_token_family_revoked.v1`. Topic routing + payload
// decode are owned by the EventProcessor (ADR 0067); this filters by the
// business Reason field (only reuse-detection revokes are SIEM incidents)
// and emits the structured WARN record.
func (h *ReuseDetectedSIEM) Handle(
	ctx context.Context, evt *integrationevents.RefreshTokenFamilyRevokedV1,
) error {
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
