// Package boot brings a cold deployment up to generation 0.
//
// A fresh pod has an empty volume: no keys, no state file, no coin. Before this
// existed, `run` on such a volume failed immediately telling the operator to go
// away and run three other subcommands in order, which is a workable answer for
// a laptop and not one for a container that will simply restart and say it
// again. It also meant a public deployment could not be brought up by the
// public: the Fund button had nowhere to appear, because there was no process
// serving a UI to put it on.
//
// So the sequence — wait for money, mint fuel, create generation 0 — runs here,
// in-process, while the UI is already being served. The one thing that cannot
// be automated is the money, and that is exactly what the UI now asks for.
//
// It is NOT in package engine. The engine owns a live ring of UTXO chains and a
// lease protocol; genesis orchestration is a different job with a different
// failure model, and putting it beside advanceCell would mean every future
// reader of the hottest code in the repository walking past it.
package boot

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/engine"
)

// Phase is how far the cold start has got.
type Phase string

const (
	// PhaseWaiting means another instance holds the writer lease. Nothing is
	// spent; this one serves the UI and watches.
	PhaseWaiting Phase = "waiting"
	// PhaseFunding means there is not yet enough coin to start.
	PhaseFunding Phase = "funding"
	// PhaseFuel means the first fuel coins are being minted.
	PhaseFuel Phase = "fuel"
	// PhaseGenesis means generation 0 is being created.
	PhaseGenesis Phase = "genesis"
	// PhaseReady means there is a deployment to run.
	PhaseReady Phase = "ready"
)

// Wallet is what the bootstrapper needs of the chain.
//
// Narrow, and an interface for the same reason chain.Ledger is one: every
// branch below decides whether to spend money, and a decision about spending
// money should be testable without any.
type Wallet interface {
	Funds(ctx context.Context) (chain.Funds, error)
	FanOutFuel(ctx context.Context, count, satoshis uint64) ([]chain.FuelResult, error)
	Genesis(ctx context.Context, compiled *cellscript.Compiled, seed ca.Row) (*chain.Deployment, error)
	LoadDeployment() (*chain.Deployment, error)
	FundingTarget() (*chain.FundingTarget, error)
}

// Options are what the cold start needs to know.
type Options struct {
	// Seed is generation 0's row.
	Seed ca.Row
	// Throughput reports whether there is a fuel pool to fill at all. With the
	// privacy strategy there is no pool and the fuel phase is skipped.
	Throughput bool
	// FuelDenomination and FirstFuelCoins size the first mint.
	FuelDenomination uint64
	FirstFuelCoins   uint64
	// MinBootstrap is what must arrive before anything is spent.
	MinBootstrap uint64
	// Network names the chain, for the UI to show a payer.
	Network string
	// LockControls mirrors the engine's setting onto the bootstrap snapshot.
	//
	// Needed even though the clock does not exist yet: the browser reads this
	// to decide whether to offer play, pause, step and a rate slider, and a
	// deployment that reported false here and true a moment later would show
	// those controls and then take them away. It is also what makes the HTTP
	// handler refuse from the very first request rather than from adoption.
	LockControls bool
	// Poll is how often funds are re-read. Defaults to 5s, matching the
	// engine's own funds tracker — the interval an operator watches after
	// sending a payment.
	Poll time.Duration
	// Leader reports whether this instance holds the writer lease. Nil means
	// "always", which is right for a single process and wrong for a cluster.
	Leader func() bool
}

// Boot runs the cold start and, until it finishes, stands in for the engine.
//
// Standing in is what lets internal/web keep exactly one Automaton and no
// notion of a deployment that does not exist yet. The SSE handler, which is the
// most delicately tested code in the repository, needs no change at all.
type Boot struct {
	wallet   Wallet
	compiled *cellscript.Compiled
	opts     Options
	logger   *slog.Logger

	mu      sync.RWMutex
	phase   Phase
	funds   chain.Funds
	lastErr string
	target  *chain.FundingTarget

	// engine is installed by Adopt; every Automaton method forwards to it once
	// it is non-nil.
	engine atomic.Pointer[engineRef]

	changed   chan struct{}
	published atomic.Pointer[[]byte]
}

// engineRef boxes the adopted Automaton so it can live in an atomic.Pointer.
type engineRef struct{ a Automaton }

// Automaton is the same seam internal/web declares, restated here so this
// package does not import the web layer in order to satisfy it. Go interfaces
// are structural: *engine.Engine satisfies both, and *Boot satisfies web's.
type Automaton interface {
	Snapshot() engine.Snapshot
	SnapshotTail() engine.Snapshot
	Stats() engine.Snapshot
	PublishedTail() (*[]byte, bool)
	Changed() <-chan struct{}

	SetMode(engine.Mode)
	SetRate(float64)
	Step()
}

// New creates a bootstrapper in the funding phase.
func New(w Wallet, compiled *cellscript.Compiled, opts Options, logger *slog.Logger) *Boot {
	if opts.Poll == 0 {
		opts.Poll = 5 * time.Second
	}
	if opts.Leader == nil {
		opts.Leader = func() bool { return true }
	}
	b := &Boot{
		wallet:   w,
		compiled: compiled,
		opts:     opts,
		logger:   logger,
		phase:    PhaseFunding,
		changed:  make(chan struct{}),
	}
	if t, err := w.FundingTarget(); err == nil {
		b.target = t
	}
	b.publish()
	return b
}

// Run drives the machine until generation 0 exists, or ctx ends.
//
// It returns the deployment, so the caller can build the engine from it. A
// deployment that already exists is returned immediately and nothing is spent.
func (b *Boot) Run(ctx context.Context) (*chain.Deployment, error) {
	// Already bootstrapped is the common case on every restart after the first.
	if d, err := b.wallet.LoadDeployment(); err == nil {
		b.setPhase(PhaseReady, "")
		return d, nil
	}

	b.logger.InfoContext(ctx, "no deployment yet; waiting for funds",
		"minimum", b.opts.MinBootstrap, "address", b.address())

	ticker := time.NewTicker(b.opts.Poll)
	defer ticker.Stop()

	for {
		d, err := b.attempt(ctx)
		if err != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if d != nil {
			b.setPhase(PhaseReady, "")
			return d, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// attempt is one pass of the machine. It returns a deployment once there is
// one, or nil to be called again.
//
// Every failure here is a reason to wait rather than to stop: a shortfall, a
// lease held elsewhere, a network hiccup during the fan-out. The one thing that
// would justify giving up — a genesis that already exists — is not a failure at
// all, and is handled by loading it.
func (b *Boot) attempt(ctx context.Context) (*chain.Deployment, error) {
	if !b.opts.Leader() {
		// Another instance is bootstrapping, or running. Spending here would
		// mint a second wallet's worth of fuel and race a second genesis.
		b.setPhase(PhaseWaiting, "")
		return nil, nil
	}

	funds, err := b.wallet.Funds(ctx)
	if err != nil {
		b.setPhase(PhaseFunding, err.Error())
		return nil, err
	}
	b.setFunds(funds)

	// Spendable is the fuel pool and Reserve is everything else; before the
	// first fan-out a deposit is entirely in reserve, so the sum is what
	// "has any money arrived" means here.
	if funds.Spendable+funds.Reserve < b.opts.MinBootstrap {
		b.setPhase(PhaseFunding, "")
		return nil, nil
	}

	// Fuel BEFORE genesis, and the order is not cosmetic: under the throughput
	// strategy genesis is funded from the pool, so creating it first finds an
	// empty pool and fails with "not enough funds" against a wallet that
	// plainly holds money.
	if b.opts.Throughput && funds.PoolCoins < int(b.opts.FirstFuelCoins) {
		b.setPhase(PhaseFuel, "")
		b.logger.InfoContext(ctx, "minting first fuel",
			"coins", b.opts.FirstFuelCoins, "satoshis", b.opts.FuelDenomination)
		if _, err := b.wallet.FanOutFuel(ctx, b.opts.FirstFuelCoins, b.opts.FuelDenomination); err != nil {
			// A shortfall here means the deposit was smaller than the estimate,
			// not that anything is broken. Fall back to asking for more.
			b.setPhase(PhaseFunding, err.Error())
			return nil, nil
		}
	}

	b.setPhase(PhaseGenesis, "")
	b.logger.InfoContext(ctx, "creating generation 0")
	d, err := b.wallet.Genesis(ctx, b.compiled, b.opts.Seed)
	if err != nil {
		// Genesis refuses to run twice while the state file exists. That is not
		// a failure to recover from, it is the answer: load what is there.
		if isAlreadyBootstrapped(err) {
			if loaded, lerr := b.wallet.LoadDeployment(); lerr == nil {
				return loaded, nil
			}
		}
		b.setPhase(PhaseFunding, err.Error())
		return nil, nil
	}
	return d, nil
}

// isAlreadyBootstrapped recognises genesis refusing a second run.
//
// Matched on the message because the chain package returns a formatted error
// rather than a sentinel; the alternative was to add one for a single caller,
// and the message is asserted by a test in this package so it cannot drift
// silently.
func isAlreadyBootstrapped(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already had a genesis")
}

// Adopt installs the engine. Every Automaton method forwards to it from here on.
func (b *Boot) Adopt(a Automaton) {
	b.engine.Store(&engineRef{a: a})
	// Wake subscribers holding OUR change channel. Without this an SSE client
	// connected during the bootstrap blocks on a channel nothing will ever
	// close again — the engine notifies on its own channel, which that client
	// does not hold — and sits on the last bootstrap frame forever.
	b.notify()
}

func (b *Boot) adopted() Automaton {
	if ref := b.engine.Load(); ref != nil {
		return ref.a
	}
	return nil
}

// --- Automaton, before and after Adopt ------------------------------------

func (b *Boot) Snapshot() engine.Snapshot {
	if a := b.adopted(); a != nil {
		return a.Snapshot()
	}
	return b.snapshot()
}

func (b *Boot) SnapshotTail() engine.Snapshot {
	if a := b.adopted(); a != nil {
		return a.SnapshotTail()
	}
	return b.snapshot()
}

func (b *Boot) Stats() engine.Snapshot {
	if a := b.adopted(); a != nil {
		return a.Stats()
	}
	return b.snapshot()
}

func (b *Boot) PublishedTail() (*[]byte, bool) {
	if a := b.adopted(); a != nil {
		return a.PublishedTail()
	}
	p := b.published.Load()
	if p == nil {
		return nil, false
	}
	return p, true
}

func (b *Boot) Changed() <-chan struct{} {
	if a := b.adopted(); a != nil {
		return a.Changed()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.changed
}

// The clock does not exist yet, so driving it is a no-op rather than an error:
// the UI hides these controls during the bootstrap anyway, and a 500 from a
// button nobody can see is worse than nothing happening.
func (b *Boot) SetMode(m engine.Mode) {
	if a := b.adopted(); a != nil {
		a.SetMode(m)
	}
}

func (b *Boot) SetRate(r float64) {
	if a := b.adopted(); a != nil {
		a.SetRate(r)
	}
}

func (b *Boot) Step() {
	if a := b.adopted(); a != nil {
		a.Step()
	}
}

// --- state ----------------------------------------------------------------

// snapshot is what the UI sees before there is an engine: no history, no
// generation, and the one thing that matters — where to send coin.
func (b *Boot) snapshot() engine.Snapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.snapshotLocked()
}

func (b *Boot) snapshotLocked() engine.Snapshot {
	var addr string
	if b.target != nil {
		addr = b.target.Address
	}
	return engine.Snapshot{
		Cells:          b.compiled.Cells(),
		Rule:           int(b.compiled.Rule()),
		Mode:           engine.ModePaused,
		FundingAddress: addr,
		Balance:        b.funds.Spendable,
		Reserve:        b.funds.Reserve,
		PoolCoins:      b.funds.PoolCoins,
		Locked:         b.opts.LockControls,
		Bootstrap: &engine.Bootstrap{
			Phase:       string(b.phase),
			Address:     addr,
			Network:     b.opts.Network,
			MinSatoshis: b.opts.MinBootstrap,
			Have:        b.funds.Spendable + b.funds.Reserve,
			Err:         b.lastErr,
		},
	}
}

func (b *Boot) setPhase(p Phase, msg string) {
	b.mu.Lock()
	if b.phase == p && b.lastErr == msg {
		b.mu.Unlock()
		return
	}
	b.phase = p
	b.lastErr = msg
	b.mu.Unlock()
	b.publish()
}

func (b *Boot) setFunds(f chain.Funds) {
	b.mu.Lock()
	if b.funds == f {
		b.mu.Unlock()
		return
	}
	b.funds = f
	b.mu.Unlock()
	b.publish()
}

// Phase reports where the cold start has got to. For tests and for logging.
func (b *Boot) Phase() Phase {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.phase
}

func (b *Boot) address() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.target == nil {
		return ""
	}
	return b.target.Address
}

// publish marshals the current state once, for every subscriber, exactly as the
// engine's publisher does — so the SSE handler's "is this the frame I already
// sent" pointer check works across the handover as well as after it.
func (b *Boot) publish() {
	b.mu.RLock()
	snap := b.snapshotLocked()
	b.mu.RUnlock()

	data, err := json.Marshal(snap)
	if err != nil {
		b.logger.Error("marshal bootstrap snapshot", "err", err)
		return
	}
	b.published.Store(&data)
	b.notify()
}

func (b *Boot) notify() {
	b.mu.Lock()
	close(b.changed)
	b.changed = make(chan struct{})
	b.mu.Unlock()
}

// ErrNotLeader is returned by a Leader func that wants to say why.
var ErrNotLeader = errors.New("boot: another instance holds the writer lease")
