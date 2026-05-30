package messaging

import (
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/google/uuid"
)

// fakeMarshalEvent mimics a module integrationevents.Event: a Topic() wire
// alias + an OccurredAt, with json-tagged payload fields.
type fakeMarshalEvent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (fakeMarshalEvent) Topic() string         { return "test.fake_happened.v1" }
func (fakeMarshalEvent) OccurredAt() time.Time { return time.Time{} }

func TestWireAliasMarshaler_NameIsTopicAlias(t *testing.T) {
	t.Parallel()
	m := NewWireAliasMarshaler()
	// Works on a zero value — cqrs calls Name(new(T)) at handler registration.
	if got := m.Name(fakeMarshalEvent{}); got != "test.fake_happened.v1" {
		t.Fatalf("Name: got %q want test.fake_happened.v1 (the Topic() alias, not the Go struct name)", got)
	}
}

func TestWireAliasMarshaler_NameFromMessageReadsEventTypeHeader(t *testing.T) {
	t.Parallel()
	m := NewWireAliasMarshaler()
	msg := message.NewMessage(uuid.NewString(), []byte(`{}`))
	msg.Metadata.Set(HeaderEventType, "identity.person_created.v1")
	if got := m.NameFromMessage(msg); got != "identity.person_created.v1" {
		t.Fatalf("NameFromMessage: got %q want identity.person_created.v1", got)
	}
}

// The load-bearing case: a message produced by PublishOutbox (raw json
// payload + HeaderEventType, NO cqrs name envelope) must Unmarshal cleanly —
// dispatch name agrees with the producer's stamped alias.
func TestWireAliasMarshaler_UnmarshalsProducerStyleMessage(t *testing.T) {
	t.Parallel()
	m := NewWireAliasMarshaler()
	msg := message.NewMessage(uuid.NewString(), []byte(`{"id":"abc","name":"Acme"}`))
	msg.Metadata.Set(HeaderEventType, fakeMarshalEvent{}.Topic())

	if got := m.NameFromMessage(msg); got != m.Name(fakeMarshalEvent{}) {
		t.Fatalf("dispatch mismatch: NameFromMessage %q != Name %q", got, m.Name(fakeMarshalEvent{}))
	}
	var got fakeMarshalEvent
	if err := m.Unmarshal(msg, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ID != "abc" || got.Name != "Acme" {
		t.Fatalf("payload round-trip: got %+v want {abc Acme}", got)
	}
}

// Marshal (EventBus path) round-trips through Unmarshal and stamps the alias.
func TestWireAliasMarshaler_MarshalRoundTrip(t *testing.T) {
	t.Parallel()
	m := NewWireAliasMarshaler()
	msg, err := m.Marshal(fakeMarshalEvent{ID: "x1", Name: "n1"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if got := msg.Metadata.Get(HeaderEventType); got != "test.fake_happened.v1" {
		t.Fatalf("Marshal event_type header: got %q", got)
	}
	var back fakeMarshalEvent
	if err := m.Unmarshal(msg, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back != (fakeMarshalEvent{ID: "x1", Name: "n1"}) {
		t.Fatalf("round-trip: got %+v", back)
	}
}
