package argon2

import (
	"context"
	"errors"
	"time"
)

// ErrBusy is returned when a password operation could not start within the
// queue timeout because every slot of the [Gate] was taken. The HTTP layer maps
// it to 503 + Retry-After, the same answer on every login branch.
var ErrBusy = errors.New("argon2: too many concurrent password operations")

// Gate bounds how many Argon2id operations run at once in this process.
//
// Each hash or verify allocates [Memory] KiB (about 19 MiB) for its whole
// duration, so memory is proportional to concurrency: the API container's 1 GiB
// limit is reached by roughly fifty simultaneous logins, and anonymous login is
// reachable by anyone. The Gate is a counting semaphore (size slots); a caller
// that cannot get a slot within queueTimeout (or whose request context ends
// first) gets [ErrBusy] / the context error and does no work.
//
// A nil *Gate runs without a bound; it exists for tests and command-line tools.
// Production wiring (cmd/api) passes the configured one to every handler that
// hashes or verifies, and a wiring test saturates it to prove each such command
// refuses with [ErrBusy].
type Gate struct {
	slots        chan struct{}
	queueTimeout time.Duration

	// hash and verify are the work under the gate; tests swap them for blocking
	// fakes so the bound is observed with barriers, not stopwatches.
	hash   func(password string) (string, error)
	verify func(password, stored string) error
}

// NewGate returns a gate with size slots. size must be at least 1 and
// queueTimeout positive.
func NewGate(size int, queueTimeout time.Duration) *Gate {
	if size < 1 {
		panic("argon2: NewGate size must be >= 1")
	}
	if queueTimeout <= 0 {
		panic("argon2: NewGate queueTimeout must be positive")
	}
	return &Gate{slots: make(chan struct{}, size), queueTimeout: queueTimeout, hash: Hash, verify: Verify} // arch-test:bounded-justified size is the configured slot count (1..64, validated by config.Validate); the channel is the semaphore itself
}

// Size is the number of slots.
func (g *Gate) Size() int {
	if g == nil {
		return 0
	}
	return cap(g.slots)
}

// Acquire takes a slot, waiting at most the queue timeout and never past ctx, and
// returns the function that gives it back. Hash and Verify use it; it is exported
// so a wiring test can saturate the real gate.
func (g *Gate) Acquire(ctx context.Context) (release func(), err error) {
	if g == nil {
		return func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Fast path: a free slot costs no timer.
	select {
	case g.slots <- struct{}{}:
		return g.release, nil
	default:
	}
	timer := time.NewTimer(g.queueTimeout)
	defer timer.Stop()
	select {
	case g.slots <- struct{}{}:
		return g.release, nil
	case <-timer.C:
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *Gate) release() { <-g.slots }

// Hash is [Hash] under the gate.
func (g *Gate) Hash(ctx context.Context, password string) (string, error) {
	release, err := g.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	if g == nil {
		return Hash(password)
	}
	return g.hash(password)
}

// Verify is [Verify] under the gate.
func (g *Gate) Verify(ctx context.Context, password, stored string) error {
	release, err := g.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if g == nil {
		return Verify(password, stored)
	}
	return g.verify(password, stored)
}
