package messaging

import (
	"fmt"
	"strings"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/ThreeDotsLabs/watermill/message"
)

// moduleTopicFromAlias derives the module routing topic from a per-event
// wire alias. Every alias is `<module>.<event>.v<N>` (ADR 0059) and every
// module subscribe topic is `<module>.events` (e.g. identityevents.Topic),
// so the alias self-encodes its routing topic: the first dotted segment +
// ".events".
//
//	identity.person_created.v1   -> identity.events
//	platform.lead_purchased.v1   -> platform.events   (CRM consumes this)
//	orders.order_packed.v1       -> orders.events      (Dispatch consumes this)
//
// This lets a SINGLE cqrs.EventProcessor host handlers for every module —
// including cross-module consumers — without a per-module topic table: the
// marshaler's Name(event)=event.Topic() yields the alias, and this maps it
// back to the topic the OutboxForwarder republishes to.
func moduleTopicFromAlias(alias string) (string, error) {
	mod, _, ok := strings.Cut(alias, ".")
	if !ok || mod == "" {
		return "", fmt.Errorf("messaging: cannot derive module topic from event alias %q (want <module>.<event>.v<N>)", alias)
	}
	return mod + ".events", nil
}

// NewEventProcessor builds the canonical cqrs.EventProcessor for the
// consumer side (ADR 0067). It hosts every module's typed event handlers
// (cqrs.NewEventHandler[T]) on one router, replacing the hand-rolled
// metadata-filter + json.Unmarshal boilerplate in every subscriber.
//
//   - GenerateSubscribeTopic  — module topic derived from the event alias
//     (see [moduleTopicFromAlias]); cross-module handlers subscribe to the
//     producer's topic automatically.
//   - SubscriberConstructor   — every handler shares the one broker
//     subscriber (gochannel in v0.2). The per-handler resilience stack is
//     router-global middleware (see [NewRouter]); cqrs does not own it.
//   - Marshaler               — [WireAliasMarshaler]: dispatch key is the
//     frozen wire alias, payload is raw JSON (matches [PublishOutbox]).
//   - AckOnUnknownEvent: true — many event types ride one module topic;
//     a handler only matches its own alias, so every other event on the
//     topic is "unknown" to it and MUST be acked, not retried forever.
//
// Handlers are registered via [cqrs.EventProcessor.AddHandlersToRouter]
// AFTER all module Register funcs have contributed their handlers — see
// cmd/worker wiring.
func NewEventProcessor(router *message.Router, sub message.Subscriber, log watermill.LoggerAdapter) (*cqrs.EventProcessor, error) {
	if router == nil {
		return nil, fmt.Errorf("messaging: NewEventProcessor router required")
	}
	if sub == nil {
		return nil, fmt.Errorf("messaging: NewEventProcessor subscriber required")
	}
	if log == nil {
		log = watermill.NopLogger{}
	}
	return cqrs.NewEventProcessorWithConfig(router, cqrs.EventProcessorConfig{
		GenerateSubscribeTopic: func(p cqrs.EventProcessorGenerateSubscribeTopicParams) (string, error) {
			return moduleTopicFromAlias(p.EventName)
		},
		SubscriberConstructor: func(cqrs.EventProcessorSubscriberConstructorParams) (message.Subscriber, error) {
			return sub, nil
		},
		AckOnUnknownEvent: true,
		Marshaler:         NewWireAliasMarshaler(),
		Logger:            log,
	})
}
