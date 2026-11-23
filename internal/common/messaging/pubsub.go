package messaging

import (
	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
)

// NewPubSub returns the in-process bus the outbox forwarder publishes to and the
// router subscribes from.
//
// Publish BLOCKS until a subscriber has acknowledged the message
// (BlockPublishUntilSubscriberAck). The forwarder deletes an outbox row as soon
// as its Publish returns, so a non-blocking bus would lose the event whenever it
// is published with no subscriber attached (the forwarder starts before the
// router) or while a handler is still running at shutdown: no outbox row, no
// dead letter. Blocking keeps the row until a handler has taken the message.
func NewPubSub(logger watermill.LoggerAdapter) *gochannel.GoChannel {
	return gochannel.NewGoChannel(gochannel.Config{BlockPublishUntilSubscriberAck: true}, logger)
}
