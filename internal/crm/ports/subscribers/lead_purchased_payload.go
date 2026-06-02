// Package subscribers holds CRM-side Watermill subscriber handlers.
package subscribers

// arch-test:idempotency-via-wire-shape-only — topic constant only; no handler logic, nothing to dedup.

import (
	platformevents "github.com/leadkart/leadkart-go/internal/platform/integrationevents"
)

// LeadPurchasedTopic is the event_type the subscriber filters on. It
// aliases the producer's exported constant, and the wire payload is the
// producer's LeadPurchasedV1 struct (imported, not mirrored) — so
// producer and consumer share one source of truth and cannot drift.
const LeadPurchasedTopic = platformevents.TopicLeadPurchasedV1
