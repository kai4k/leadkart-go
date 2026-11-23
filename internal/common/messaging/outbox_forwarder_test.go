package messaging

import (
	"testing"
	"time"
)

// Review L2: the outbox subscriber redelivers a message that is not acknowledged within its
// AckDeadline. The forwarder waits for each handler, and a handler may hold a message for its whole
// retry budget (every attempt at its timeout plus the backoffs), so the deadline must be longer than
// that budget: below it the message is delivered a second time while the first delivery is still
// being retried.
func TestOutboxSubscriberConfig_AckDeadlineExceedsTheRetryBudget(t *testing.T) {
	t.Parallel()

	cfg := outboxSubscriberConfig()
	if cfg.AckDeadline == nil {
		t.Fatal("no AckDeadline: the library default (30s) is shorter than the retry budget, so a slow handler's message is delivered twice")
	}
	budget := RetryBudget(DefaultRetry, defaultHandlerTimeout)
	if *cfg.AckDeadline <= budget {
		t.Fatalf("AckDeadline %s must exceed the retry budget %s", *cfg.AckDeadline, budget)
	}
	if want := budget + ackDeadlineMargin; *cfg.AckDeadline < want {
		t.Fatalf("AckDeadline %s, want at least the budget plus the margin %s", *cfg.AckDeadline, want)
	}
	if *cfg.AckDeadline <= 30*time.Second {
		t.Fatalf("AckDeadline %s is no longer than the library default", *cfg.AckDeadline)
	}
	if cfg.InitializeSchema {
		t.Fatal("goose owns the schema: the library must not create the outbox table")
	}
}
