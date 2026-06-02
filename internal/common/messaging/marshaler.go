package messaging

import (
	"encoding/json"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/ThreeDotsLabs/watermill/message"
)

// eventNamer is what every module's integration event satisfies: Topic()
// returns the per-event wire alias (e.g. "identity.person_created.v1"), the
// frozen cross-module contract (ADR 0059) and the value stamped into the
// event_type header.
//
// Two distinct "topics" live in this package: the per-event alias above, and
// the per-module routing topic (a package const like "identity.events") used
// by the Forwarder destination + the EventProcessor's GenerateSubscribeTopic.
type eventNamer interface {
	Topic() string
}

// WireAliasMarshaler is the cqrs marshaler that adopts the cqrs component
// without breaking the frozen wire contract (ADR 0059): the dispatch name is
// the event's Topic() alias, not the Go struct name the stock JSONMarshaler
// would use. Producer (per-tx EventBus) and consumer (EventProcessor) share
// one instance, so encode and decode cannot drift.
type WireAliasMarshaler struct {
	json cqrs.JSONMarshaler
}

// NewWireAliasMarshaler returns the marshaler shared by both sides. The
// embedded JSONMarshaler is used only by Name's non-event fallback.
func NewWireAliasMarshaler() WireAliasMarshaler { return WireAliasMarshaler{} }

// Marshal encodes v to a message: raw json.Marshal(v) payload + the Topic()
// alias on the event_type header. Deliberately does NOT delegate to the stock
// JSONMarshaler — that adds a struct-name "name" key the consumer never reads
// (dispatch is by event_type), dead weight on every outbox row.
func (m WireAliasMarshaler) Marshal(v any) (*message.Message, error) {
	payload, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	msg := message.NewMessage(watermill.NewUUID(), payload)
	msg.Metadata.Set(HeaderEventType, m.Name(v))
	return msg, nil
}

// Unmarshal decodes the JSON payload into v. The EventProcessor already
// selected the handler via NameFromMessage, so there is no name to validate.
func (m WireAliasMarshaler) Unmarshal(msg *message.Message, v any) error {
	return json.Unmarshal(msg.Payload, v)
}

// Name returns the per-event wire alias (Topic()), falling back to the stock
// struct name for non-event values (never hit on the dispatch path).
func (m WireAliasMarshaler) Name(v any) string {
	if e, ok := v.(eventNamer); ok {
		return e.Topic()
	}
	return m.json.Name(v)
}

// NameFromMessage returns the alias the producer stamped on event_type — the
// dispatch key the EventProcessor matches against each handler's Name.
func (m WireAliasMarshaler) NameFromMessage(msg *message.Message) string {
	return msg.Metadata.Get(HeaderEventType)
}

var _ cqrs.CommandEventMarshaler = WireAliasMarshaler{}
