package command

// WithPasswordVerifier swaps the password verifier so tests can count and
// inspect hash operations (timing parity is asserted structurally: the same
// number of verifies per failure branch, never a wall-clock threshold).
func (h LoginHandler) WithPasswordVerifier(verify func(password, encoded string) error) LoginHandler {
	h.verify = verify
	return h
}
