package argon2

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrBusy is returned when a password operation could not start within the
// queue timeout because every slot of the [Gate] was taken, or because the client
// already has its share of operations in flight. The HTTP layer maps it to
// 503 + Retry-After, the same answer on every login branch.
var ErrBusy = errors.New("argon2: too many concurrent password operations")

// Class says who is asking for password work. Anonymous work (login, tenant
// registration) can be started by anyone, so it may use only part of the gate;
// authenticated work (change-password, create user, a reset confirmation with a
// valid token) is work somebody who proved who they are asked for, and keeps a share
// of the gate that anonymous load cannot take.
type Class int

const (
	// Anonymous is the default for a context that says nothing.
	Anonymous Class = iota
	// Authenticated work may use every slot, including the reserved ones.
	Authenticated
)

type ctxKey int

const (
	classKey ctxKey = iota
	clientKey
)

// WithAuthenticated marks ctx as asking for [Authenticated] work. Handlers call
// it only after the caller proved who they are (a verified token, a valid reset
// token).
func WithAuthenticated(ctx context.Context) context.Context {
	return context.WithValue(ctx, classKey, Authenticated)
}

// WithClient names the client the work is done for (the same key the rate limiters
// use: the address, IPv6 masked to its /64). The gate allows each client key a
// bounded number of operations at once. A context without a client (a worker, a
// command-line tool, a test) is not limited per client.
func WithClient(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, clientKey, key)
}

func classOf(ctx context.Context) Class {
	if c, ok := ctx.Value(classKey).(Class); ok {
		return c
	}
	return Anonymous
}

func clientOf(ctx context.Context) string {
	k, _ := ctx.Value(clientKey).(string)
	return k
}

// Gate bounds how many Argon2id operations run at once in this process.
//
// Each hash or verify allocates [Memory] KiB (about 19 MiB) for its whole
// duration, so memory is proportional to concurrency: the API container's 1 GiB
// limit is reached by roughly fifty simultaneous logins, and anonymous login is
// reachable by anyone. The Gate is a counting semaphore (size slots); a caller
// that cannot get a slot within queueTimeout (or whose request context ends
// first) gets [ErrBusy] / the context error and does no work.
//
// Two more bounds keep one source from starving everyone (fix round 12, item 3).
// Per client: at most perClient operations of one client key are in flight (running
// OR queued); the next is refused at once with [ErrBusy], before it queues, so a
// source that holds its share cannot also fill the queue. Per class: anonymous work
// can use at most size-reserved slots, so authenticated work always finds one.
//
// A nil *Gate runs without a bound; it exists for tests and command-line tools.
// Production wiring (cmd/api) passes the configured one to every handler that
// hashes or verifies, and a wiring test saturates it to prove each such command
// refuses with [ErrBusy].
type Gate struct {
	slots        chan struct{}
	anon         chan struct{} // nil when nothing is reserved
	queueTimeout time.Duration
	perClient    int // 0 = unlimited

	mu       sync.Mutex
	inflight map[string]int // client key -> operations running or queued; deleted at zero

	// hash and verify are the work under the gate; tests swap them for blocking
	// fakes so the bound is observed with barriers, not stopwatches.
	hash   func(password string) (string, error)
	verify func(password, stored string) error
}

// GateOption tunes a [Gate].
type GateOption func(*Gate)

// WithPerClient bounds the operations of one client key in flight at once.
func WithPerClient(n int) GateOption {
	return func(g *Gate) { g.perClient = n }
}

// WithReserved keeps n of the slots for [Authenticated] work.
func WithReserved(n int) GateOption {
	return func(g *Gate) {
		if n > 0 {
			g.anon = make(chan struct{}, cap(g.slots)-n) // arch-test:bounded-justified size - reserved, both configured and validated (0 <= reserved < size)
		}
	}
}

// NewGate returns a gate with size slots. size must be at least 1 and
// queueTimeout positive; a reservation must leave at least one anonymous slot.
func NewGate(size int, queueTimeout time.Duration, opts ...GateOption) *Gate {
	if size < 1 {
		panic("argon2: NewGate size must be >= 1")
	}
	if queueTimeout <= 0 {
		panic("argon2: NewGate queueTimeout must be positive")
	}
	g := &Gate{slots: make(chan struct{}, size), queueTimeout: queueTimeout, inflight: map[string]int{}, hash: Hash, verify: Verify} // arch-test:bounded-justified size is the configured slot count (1..64, validated by config.Validate); the channel is the semaphore itself
	for _, o := range opts {
		o(g)
	}
	if g.anon != nil && cap(g.anon) < 1 {
		panic("argon2: NewGate reserved must be < size")
	}
	return g
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
// so a wiring test can saturate the real gate. The client and class come from ctx
// ([WithClient], [WithAuthenticated]).
//
// The per-client check comes first and never waits: a client at its cap is refused
// with [ErrBusy] before it takes a queue place.
func (g *Gate) Acquire(ctx context.Context) (release func(), err error) {
	if g == nil {
		return func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dropClient, err := g.admitClient(clientOf(ctx))
	if err != nil {
		return nil, err
	}
	dropSlots, err := g.takeSlots(ctx, classOf(ctx))
	if err != nil {
		dropClient()
		return nil, err
	}
	return func() {
		dropSlots()
		dropClient()
	}, nil
}

// admitClient counts one more operation for the client or refuses at the cap. The
// map holds only clients with an operation running or queued: an entry is deleted
// when its count reaches zero, so memory is bounded by the requests in flight.
func (g *Gate) admitClient(key string) (drop func(), err error) {
	if g.perClient <= 0 || key == "" {
		return func() {}, nil
	}
	g.mu.Lock()
	if g.inflight[key] >= g.perClient {
		g.mu.Unlock()
		return nil, ErrBusy
	}
	g.inflight[key]++
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		if g.inflight[key] <= 1 {
			delete(g.inflight, key)
		} else {
			g.inflight[key]--
		}
		g.mu.Unlock()
	}, nil
}

// trackedClients is the number of client keys with an operation in flight.
func (g *Gate) trackedClients() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.inflight)
}

// takeSlots acquires the class's place (anonymous work first takes one of the
// size-reserved anonymous places) and then a slot, both within the one queue
// timeout.
func (g *Gate) takeSlots(ctx context.Context, class Class) (drop func(), err error) {
	timer := time.NewTimer(g.queueTimeout)
	defer timer.Stop()
	var drops []func()
	undo := func() {
		for i := len(drops) - 1; i >= 0; i-- {
			drops[i]()
		}
	}
	if class == Anonymous && g.anon != nil {
		if err := take(ctx, g.anon, timer); err != nil {
			return nil, err
		}
		drops = append(drops, func() { <-g.anon })
	}
	if err := take(ctx, g.slots, timer); err != nil {
		undo()
		return nil, err
	}
	drops = append(drops, func() { <-g.slots })
	return undo, nil
}

// take puts one token in sem: at once when there is room (no timer wait), else
// waiting for the shared timer or the request's end.
func take(ctx context.Context, sem chan struct{}, timer *time.Timer) error {
	select {
	case sem <- struct{}{}:
		return nil
	default:
	}
	select {
	case sem <- struct{}{}:
		return nil
	case <-timer.C:
		return ErrBusy
	case <-ctx.Done():
		return ctx.Err()
	}
}

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

// VerifyHeld is [Verify] for a caller that already holds a slot from [Gate.Acquire]
// (the login takes the slot before it registers the attempt and verifies inside it).
func (g *Gate) VerifyHeld(password, stored string) error {
	if g == nil {
		return Verify(password, stored)
	}
	return g.verify(password, stored)
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
