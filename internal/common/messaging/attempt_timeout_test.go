// arch-test:no-timeout-needed — every handler below returns when its own attempt deadline fires.

package messaging

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
)

// runStack runs fn through the delivery stack's timeout part (ledger, retry, attempt
// timeout) the way AddSubscriber orders them, and returns the delivery's final error.
func runStack(fn func(ctx context.Context, attempt int) error) (int, error) {
	attempts := 0
	h := func(msg *message.Message) ([]*message.Message, error) {
		attempts++
		return nil, fn(msg.Context(), attempts)
	}
	retry := RetryConfig{MaxRetries: 5, InitialInterval: time.Millisecond, MaxInterval: 2 * time.Millisecond, Multiplier: 2}
	chain := timeoutLedger(retryMiddleware(retry)(attemptTimeout(20 * time.Millisecond)(h)))
	_, err := chain(message.NewMessage("m", nil))
	return attempts, err
}

// Review G2c B3: a timed-out attempt whose one retry then fails for another transient
// reason is still a timed-out event. It must keep the timeout reason, or the replay job
// (which reads the reason) would lose it for good.
func TestAttemptTimeout_ATransientFailureOnTheLastAttemptKeepsTheTimeoutReason(t *testing.T) {
	t.Parallel()
	serialization := errors.New("SQLSTATE 40001")
	attempts, err := runStack(func(ctx context.Context, attempt int) error {
		if attempt == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return serialization
	})
	if attempts != 2 {
		t.Fatalf("attempts %d, want 2 (the timeout and one retry)", attempts)
	}
	if err == nil || !strings.HasPrefix(err.Error(), TimeoutReasonPrefix) {
		t.Fatalf("reason %v, want the timeout prefix", err)
	}
	if !IsNonRetryable(err) {
		t.Fatal("the last attempt must be non-retryable")
	}
	if !errors.Is(err, serialization) {
		t.Fatal("the transient cause must stay reachable")
	}
}

// A failure that was already permanent keeps its own reason: it is not a stuck event.
func TestAttemptTimeout_APermanentFailureAfterATimeoutIsNotMadeATimeout(t *testing.T) {
	t.Parallel()
	_, err := runStack(func(ctx context.Context, attempt int) error {
		if attempt == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return NonRetryable(errors.New("stock expired"))
	})
	if err == nil || strings.HasPrefix(err.Error(), TimeoutReasonPrefix) {
		t.Fatalf("reason %v, want the permanent reason", err)
	}
}
