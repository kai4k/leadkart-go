package subscribers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/ThreeDotsLabs/watermill/message"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
)

// SecurityStampInvalidator is the single capability this subscriber
// needs from the cache facade. Declared at the consumer (Cheney "accept
// interfaces, return structs") rather than importing the concrete
// *adapters.SecurityStampCache — keeps the subscriber decoupled from
// the cache impl, lets tests inject a fake without spinning up Redis.
type SecurityStampInvalidator interface {
	Invalidate(ctx context.Context, id person.ID) error
}

// arch-test:idempotency-via-noop-on-replay — cache.Invalidate is unconditional Del; replaying it 1..N times yields identical post-state. Documented in the doctrine block below ("The handler is idempotent: cache.Invalidate is unconditional Del...").

// InvalidateSecurityStampCache evicts the cached SecurityStamp for a
// Person whenever a stamp-rotation event fires
// (PasswordChanged / Anonymised / GloballySuspended / EmailChanged).
//
// Single responsibility: cache eviction only. Refresh-token family
// revocation runs as an INDEPENDENT subscriber
// ([RevokeFamiliesOnSecurityChange]) against the same events. The
// two concerns are intentionally split:
//
//   - Cache eviction is an OPPORTUNISTIC fast-path optimisation. The
//     30s SecurityStampCache TTL is the actual revocation contract
//     (per Microsoft Learn "Hybrid cache library in ASP.NET Core" +
//     `audit-checklist.md §12b`: "TTL is the safety contract;
//     explicit invalidation is best-effort acceleration"). If
//     Invalidate fails, the TTL still closes the window within 30s.
//   - Family revocation is server-side state that downstream
//     subscribers (SIEM, audit, refresh-token reuse detection)
//     depend on. It MUST succeed.
//
// Failure mode (Camp 1 / Microsoft canon for our profile):
//
//   - L1 / L2 transport failures: log at WARN, return nil.
//     Watermill does NOT retry — the TTL covers eventual consistency.
//     Retrying would be expensive (re-runs idempotency + audit
//     middleware) for a guarantee we don't need.
//   - The handler is idempotent: cache.Invalidate is unconditional
//     Del, safe to re-run any number of times. So even though we
//     don't request retries, accidental re-delivery (router upgrade,
//     replay) is harmless.
//
// Doctrine sources:
//   - Microsoft Learn — HybridCache RemoveAsync semantics (best-effort).
//   - Auth0 / Okta session-validation refresh window (~30s) — the
//     industry baseline for JWT revocation latency in Camp-1
//     SaaS profiles.
//   - LeadKart `.NET .claude/rules/audit-checklist.md §12b` —
//     "Cache facade per concern" + per-key eviction over tags.
//   - Three Dots Labs / Wolverine canon — one subscriber per concern;
//     each carries its own retry/failure semantics matching its
//     correctness profile.
type InvalidateSecurityStampCache struct {
	stampCache SecurityStampInvalidator
	log        *slog.Logger
}

// NewInvalidateSecurityStampCache wires the subscriber against the
// shared cache facade. Production passes *adapters.SecurityStampCache;
// tests pass a fake that implements [SecurityStampInvalidator].
func NewInvalidateSecurityStampCache(
	stampCache SecurityStampInvalidator,
	log *slog.Logger,
) *InvalidateSecurityStampCache {
	if stampCache == nil {
		panic("subscribers: NewInvalidateSecurityStampCache stampCache required")
	}
	if log == nil {
		panic("subscribers: NewInvalidateSecurityStampCache log required")
	}
	return &InvalidateSecurityStampCache{stampCache: stampCache, log: log}
}

// HandlePasswordChanged is the [messaging.SubscriberHandler] for the
// `identity.person_password_changed.v1` topic. Other events on the
// shared topic short-circuit silently.
func (h *InvalidateSecurityStampCache) HandlePasswordChanged(
	ctx context.Context, _ string, msg *message.Message,
) error {
	expected := integrationevents.PersonPasswordChangedV1{}.Topic()
	if msg.Metadata.Get(messaging.HeaderEventType) != expected {
		return nil
	}
	var evt integrationevents.PersonPasswordChangedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("subscribers: decode %s: %w", expected, err)
	}
	h.invalidate(ctx, evt.PersonID.String(), "password_changed")
	return nil
}

// HandleAnonymised is the handler for `identity.person_anonymised.v1`.
func (h *InvalidateSecurityStampCache) HandleAnonymised(
	ctx context.Context, _ string, msg *message.Message,
) error {
	expected := integrationevents.PersonAnonymisedV1{}.Topic()
	if msg.Metadata.Get(messaging.HeaderEventType) != expected {
		return nil
	}
	var evt integrationevents.PersonAnonymisedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("subscribers: decode %s: %w", expected, err)
	}
	h.invalidate(ctx, evt.PersonID.String(), "person_anonymised")
	return nil
}

// HandleGloballySuspended is the handler for
// `identity.person_globally_suspended.v1`.
func (h *InvalidateSecurityStampCache) HandleGloballySuspended(
	ctx context.Context, _ string, msg *message.Message,
) error {
	expected := integrationevents.PersonGloballySuspendedV1{}.Topic()
	if msg.Metadata.Get(messaging.HeaderEventType) != expected {
		return nil
	}
	var evt integrationevents.PersonGloballySuspendedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("subscribers: decode %s: %w", expected, err)
	}
	h.invalidate(ctx, evt.PersonID.String(), "globally_suspended")
	return nil
}

// HandleEmailChanged is the handler for
// `identity.person_email_changed.v1`. The flow that triggers this
// event is currently disabled at the product level; the handler
// stays wired so the cascade works correctly when the flow lands.
func (h *InvalidateSecurityStampCache) HandleEmailChanged(
	ctx context.Context, _ string, msg *message.Message,
) error {
	expected := integrationevents.PersonEmailChangedV1{}.Topic()
	if msg.Metadata.Get(messaging.HeaderEventType) != expected {
		return nil
	}
	var evt integrationevents.PersonEmailChangedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("subscribers: decode %s: %w", expected, err)
	}
	h.invalidate(ctx, evt.PersonID.String(), "email_changed")
	return nil
}

// invalidate is the shared cache-eviction body. Always returns nil
// to the caller — see the type docstring for the
// "WARN-and-continue, TTL is the contract" rationale.
func (h *InvalidateSecurityStampCache) invalidate(
	ctx context.Context, personID, reason string,
) {
	pid := person.ID(personID)
	if err := h.stampCache.Invalidate(ctx, pid); err != nil {
		h.log.WarnContext(ctx, "security stamp cache invalidate failed (TTL fallback covers it)",
			"person_id", personID,
			"reason", reason,
			"err", err,
		)
		return
	}
	h.log.InfoContext(ctx, "security stamp cache invalidated",
		"person_id", personID,
		"reason", reason,
	)
}
