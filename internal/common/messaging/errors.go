package messaging

import "errors"

// DeadLetterTopic is where the PoisonQueue middleware salvages a message
// the handler could not process — after retries are exhausted, or
// immediately for a [NonRetryable] error. A dedicated subscriber
// ([DeadLetterWriter]) persists these to common.dead_letter for
// inspection and replay; an ephemeral gochannel DLQ alone is useless to
// ops.
const DeadLetterTopic = "dead_letter"

// nonRetryableError marks an error as permanently unprocessable — a
// malformed/undecodable payload, a schema mismatch, a domain validation
// rejection. The retry middleware's ShouldRetry skips these so they reach
// the PoisonQueue at once instead of burning the whole backoff budget on
// an error that can never succeed.
type nonRetryableError struct{ err error }

func (e nonRetryableError) Error() string { return e.err.Error() }
func (e nonRetryableError) Unwrap() error { return e.err }

// NonRetryable wraps err so the retry middleware will not retry it (it is
// dead-lettered on the first failure). Returns nil if err is nil, so it
// is safe to wrap unconditionally: `return messaging.NonRetryable(json.Unmarshal(...))`.
func NonRetryable(err error) error {
	if err == nil {
		return nil
	}
	return nonRetryableError{err: err}
}

// IsNonRetryable reports whether err, or anything it wraps, was marked
// [NonRetryable].
func IsNonRetryable(err error) bool {
	_, ok := errors.AsType[nonRetryableError](err)
	return ok
}
