package messaging

import (
	"context"
	"errors"

	wmw "github.com/ThreeDotsLabs/watermill/message/router/middleware"
)

// DeadLetterTopic is where PoisonQueue salvages a message the handler can't
// process — after retries exhaust, or immediately for a NonRetryable error.
// DeadLetterWriter persists these to common.dead_letter for inspection/replay.
const DeadLetterTopic = "dead_letter"

// nonRetryableError marks an error as permanently unprocessable (malformed
// payload, schema mismatch, domain rejection). Retry's ShouldRetry skips it so
// it dead-letters at once instead of burning the backoff budget.
type nonRetryableError struct{ err error }

func (e nonRetryableError) Error() string { return e.err.Error() }
func (e nonRetryableError) Unwrap() error { return e.err }

// NonRetryable wraps err so the retry middleware won't retry it. Returns nil
// for nil err, so it's safe to wrap unconditionally:
// `return messaging.NonRetryable(json.Unmarshal(...))`.
func NonRetryable(err error) error {
	if err == nil {
		return nil
	}
	return nonRetryableError{err: err}
}

// IsNonRetryable reports whether err, or anything it wraps, was marked
// NonRetryable.
func IsNonRetryable(err error) bool {
	_, ok := errors.AsType[nonRetryableError](err)
	return ok
}

// Stable failure codes stored in the audit row's failure_reason. They are the
// only failure text the audit feed exposes.
const (
	FailureHandlerFailed = "handler_failed"
	FailureNonRetryable  = "non_retryable"
	FailurePanic         = "handler_panic"
	FailureTimeout       = "timeout"
	FailureCanceled      = "canceled"
)

// FailureCode maps a handler error to a stable, safe code. The raw error text
// is for logs, never for the audit row.
func FailureCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return FailureTimeout
	case errors.Is(err, context.Canceled):
		return FailureCanceled
	}
	if _, ok := errors.AsType[wmw.RecoveredPanicError](err); ok {
		return FailurePanic
	}
	if IsNonRetryable(err) {
		return FailureNonRetryable
	}
	return FailureHandlerFailed
}
