package messaging

import "errors"

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
