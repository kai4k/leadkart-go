package messaging

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/ThreeDotsLabs/watermill/message"
)

// moduleTopicFromAlias derives a module's routing topic from a per-event wire
// alias. Aliases are "<module>.<event>.v<N>" (ADR 0059) and every module
// subscribe topic is "<module>.events", so the first segment + ".events" is
// the topic:
//
//	identity.person_created.v1 -> identity.events
//	platform.lead_purchased.v1 -> platform.events   (CRM consumes this)
//	orders.order_packed.v1     -> orders.events      (Dispatch consumes this)
//
// This lets one EventProcessor host every module's handlers — including
// cross-module consumers — with no per-module topic table.
func moduleTopicFromAlias(alias string) (string, error) {
	mod, _, ok := strings.Cut(alias, ".")
	if !ok || mod == "" {
		return "", fmt.Errorf("messaging: cannot derive module topic from event alias %q (want <module>.<event>.v<N>)", alias)
	}
	return mod + ".events", nil
}

// NewEventProcessor builds the cqrs EventProcessor for the consumer side
// (ADR 0067). It hosts every module's typed handlers on one router.
//
// GenerateSubscribeTopic derives each handler's topic from the event alias;
// SubscriberConstructor shares the one broker subscriber (gochannel in v0.2 —
// fan-out per Subscribe; a real consumer-group broker needs per-handler
// subscribers, see ADR 0067); AckOnUnknownEvent is true because many event
// types ride one module topic, so a handler must ack the ones that aren't its
// own. The per-handler resilience stack is attached by Router.AddCqrsHandler,
// not here.
func NewEventProcessor(router *message.Router, sub message.Subscriber, log watermill.LoggerAdapter) (*cqrs.EventProcessor, error) {
	if router == nil {
		return nil, errors.New("messaging: NewEventProcessor router required")
	}
	if sub == nil {
		return nil, errors.New("messaging: NewEventProcessor subscriber required")
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
