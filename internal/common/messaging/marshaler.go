package messaging

import (
	"encoding/json"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/ThreeDotsLabs/watermill/message"
)

// eventNamer is the minimal contract every module's integrationevents.Event
// satisfies. Topic() returns the per-event WIRE ALIAS (e.g.
// "identity.person_created.v1") — the frozen cross-module contract (ADR 0059)
// and exactly the value [PublishOutbox] stamps into the [HeaderEventType]
// metadata header. NOTE the two distinct "topics" in this codebase:
//   - the per-event alias, returned by the Event.Topic() METHOD (this), used
//     as the cqrs dispatch name + the event_type header; and
//   - the per-module routing topic, a package-level `const Topic` (e.g.
//     "identity.events"), used by the Forwarder destination + the
//     EventProcessor's GenerateSubscribeTopic.
type eventNamer interface {
	Topic() string
}

// WireAliasMarshaler is the [cqrs.CommandEventMarshaler] that adopts the
// Watermill cqrs component WITHOUT breaking the frozen wire contract:
//
//   - Name(event)        = event.Topic()  (the alias — NOT the Go struct name
//     the stock cqrs.JSONMarshaler would use, which would rename every event
//     and break cross-module consumers + ADR 0059).
//   - NameFromMessage(m) = m.Metadata.Get(HeaderEventType)  — the same header
//     [PublishOutbox] already stamps, so producer and consumer agree on the
//     dispatch key with zero payload changes.
//   - Unmarshal           = raw json.Unmarshal of the payload (the producer
//     writes json.Marshal(event); no cqrs "name" envelope metadata is present
//     on outbox-relayed messages, so we must NOT delegate to the stock
//     JSONMarshaler.Unmarshal which expects its own name metadata).
//
// This is the linchpin of the cqrs.EventProcessor adoption (ADR 0067): the
// byte-level wire format is identical to Phase 2; only consumer-side dispatch
// moves from hand-rolled metadata filters to typed cqrs handlers.
type WireAliasMarshaler struct {
	json cqrs.JSONMarshaler
}

// NewWireAliasMarshaler returns the one marshaler BOTH sides share: the
// per-tx [cqrs.EventBus] in [PublishOutbox] (produce) and the
// [cqrs.EventProcessor] in [NewEventProcessor] (consume). Zero-config: the
// embedded JSONMarshaler is used only by [Name]'s non-event fallback; the
// produce/consume hot paths use Marshal / Name / NameFromMessage / Unmarshal.
func NewWireAliasMarshaler() WireAliasMarshaler { return WireAliasMarshaler{} }

// Marshal encodes v to a message whose payload is the raw json.Marshal(v)
// and whose [HeaderEventType] is the Topic() alias. This IS the producer
// hot path: [PublishOutbox] drives a per-transaction [cqrs.EventBus]
// configured with this marshaler, so encode (here) and decode ([Unmarshal])
// share one definition of the wire format — they cannot drift.
//
// Deliberately NOT delegating to the embedded [cqrs.JSONMarshaler.Marshal]:
// that stamps an extra struct-name "name" metadata key the consumer never
// reads (dispatch is by [HeaderEventType] via [NameFromMessage]), which
// would be dead weight on every outbox row. We build the message directly
// so the on-wire metadata is exactly the set the subscriber + forwarder
// envelope expect.
func (m WireAliasMarshaler) Marshal(v any) (*message.Message, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	msg := message.NewMessage(watermill.NewUUID(), payload)
	msg.Metadata.Set(HeaderEventType, m.Name(v))
	return msg, nil
}

// Unmarshal decodes the JSON payload into v. No name validation — the
// EventProcessor has already selected the handler via [NameFromMessage].
func (m WireAliasMarshaler) Unmarshal(msg *message.Message, v any) error {
	return json.Unmarshal(msg.Payload, v)
}

// Name returns the per-event wire alias (Topic()). Falls back to the stock
// struct-name only for values that are not integration events (none on the
// real dispatch path — the fallback just keeps the contract total).
func (m WireAliasMarshaler) Name(v any) string {
	if e, ok := v.(eventNamer); ok {
		return e.Topic()
	}
	return m.json.Name(v)
}

// NameFromMessage returns the alias the producer stamped on [HeaderEventType]
// — the dispatch key the EventProcessor matches against each handler's Name.
func (m WireAliasMarshaler) NameFromMessage(msg *message.Message) string {
	return msg.Metadata.Get(HeaderEventType)
}

// Compile-time proof the marshaler satisfies the cqrs contract.
var _ cqrs.CommandEventMarshaler = WireAliasMarshaler{}
