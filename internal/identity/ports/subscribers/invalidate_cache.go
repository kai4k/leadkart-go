package subscribers

import (
	"context"
	"log/slog"

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
// Post-cqrs (ADR 0067): the EventProcessor + WireAliasMarshaler own
// topic routing + payload decode, so each handler is just the typed
// business reaction — no event_type filter, no json.Unmarshal. The
// handler stays idempotent (cache.Invalidate is unconditional Del), so
// re-delivery / replay is harmless even though this concern does not
// request Watermill retries (the TTL is the contract).
//
// Doctrine sources:
//   - Microsoft Learn — HybridCache RemoveAsync semantics (best-effort).
//   - Auth0 / Okta session-validation refresh window (~30s).
//   - LeadKart `.NET .claude/rules/audit-checklist.md §12b`.
//   - Three Dots Labs / Wolverine — one subscriber per concern.
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

// HandlePasswordChanged is the typed cqrs handler for
// `identity.person_password_changed.v1`.
func (h *InvalidateSecurityStampCache) HandlePasswordChanged(
	ctx context.Context, evt *integrationevents.PersonPasswordChangedV1,
) error {
	h.invalidate(ctx, evt.PersonID.String(), "password_changed")
	return nil
}

// HandleAnonymised handles `identity.person_anonymised.v1`.
func (h *InvalidateSecurityStampCache) HandleAnonymised(
	ctx context.Context, evt *integrationevents.PersonAnonymisedV1,
) error {
	h.invalidate(ctx, evt.PersonID.String(), "person_anonymised")
	return nil
}

// HandleGloballySuspended handles `identity.person_globally_suspended.v1`.
func (h *InvalidateSecurityStampCache) HandleGloballySuspended(
	ctx context.Context, evt *integrationevents.PersonGloballySuspendedV1,
) error {
	h.invalidate(ctx, evt.PersonID.String(), "globally_suspended")
	return nil
}

// HandleEmailChanged handles `identity.person_email_changed.v1`. The flow
// that triggers this event is currently disabled at the product level;
// the handler stays wired so the cascade works when the flow lands.
func (h *InvalidateSecurityStampCache) HandleEmailChanged(
	ctx context.Context, evt *integrationevents.PersonEmailChangedV1,
) error {
	h.invalidate(ctx, evt.PersonID.String(), "email_changed")
	return nil
}

// invalidate is the shared cache-eviction body. Always succeeds from the
// caller's view — see the type docstring for the "WARN-and-continue, TTL
// is the contract" rationale.
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
