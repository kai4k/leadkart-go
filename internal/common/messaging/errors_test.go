package messaging_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

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
