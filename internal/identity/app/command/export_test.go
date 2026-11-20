package command

import "context"

// WithPasswordVerifier swaps the password verifier so tests can count and
// inspect hash operations (timing parity is asserted structurally: the same
// number of verifies per failure branch, never a wall-clock threshold).
func (h LoginHandler) WithPasswordVerifier(verify func(password, encoded string) error) LoginHandler {
	h.verify = func(_ context.Context, password, encoded string) error { return verify(password, encoded) }
	return h
}

// WithPasswordVerifier swaps the current-password verifier so a test can run a
// competing writer between the verify and the write.
func (h ChangePasswordHandler) WithPasswordVerifier(verify func(password, encoded string) error) ChangePasswordHandler {
	h.verify = func(_ context.Context, password, encoded string) error { return verify(password, encoded) }
	return h
}

// WithGateAcquire swaps the slot acquisition so a test can refuse an attempt
// before it is registered, the way a saturated gate or a gone request does.
func (h LoginHandler) WithGateAcquire(acquire func(ctx context.Context) (func(), error)) LoginHandler {
	h.acquire = acquire
	return h
}
