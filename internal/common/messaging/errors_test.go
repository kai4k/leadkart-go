package messaging_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	wmw "github.com/ThreeDotsLabs/watermill/message/router/middleware"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
)

func TestFailureCode_IsStableAndNeverTheRawText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"plain", errors.New(`pq: duplicate key "uq_internal"`), messaging.FailureHandlerFailed},
		{"wrapped plain", fmt.Errorf("repo: %w", errors.New("boom")), messaging.FailureHandlerFailed},
		{"non retryable", messaging.NonRetryable(errors.New("bad payload")), messaging.FailureNonRetryable},
		{"panic", wmw.RecoveredPanicError{V: "x"}, messaging.FailurePanic},
		{"deadline", fmt.Errorf("q: %w", context.DeadlineExceeded), messaging.FailureTimeout},
		{"canceled", context.Canceled, messaging.FailureCanceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := messaging.FailureCode(tt.err); got != tt.want {
				t.Errorf("FailureCode = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRetryBudget_EveryAttemptAtItsTimeoutPlusTheBackoffs(t *testing.T) {
	t.Parallel()
	r := messaging.RetryConfig{MaxRetries: 2, InitialInterval: 10 * time.Millisecond, MaxInterval: 20 * time.Millisecond, Multiplier: 2}
	if got, want := messaging.RetryBudget(r, 300*time.Millisecond), 3*300*time.Millisecond+2*20*time.Millisecond; got != want {
		t.Fatalf("budget = %s, want %s", got, want)
	}
	// The production default (5 retries, 5s attempts, 5s max backoff) is 55s; the
	// forwarder AckDeadline is that plus a margin, never below it.
	if got := messaging.RetryBudget(messaging.DefaultRetry, 5*time.Second); got != 55*time.Second {
		t.Fatalf("default budget = %s", got)
	}
}
