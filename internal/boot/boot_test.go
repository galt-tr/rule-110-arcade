package boot

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/engine"
	"github.com/dymurray/rule-110-arcade/internal/metrics"
)

// fakeWallet records what it was asked to do, in order. Nothing here touches a
// wallet, an arcade or money — which is the point: every branch under test
// decides whether to SPEND, and that decision should be assertable without any.
type fakeWallet struct {
	mu sync.Mutex

	funds      chain.Funds
	fundsErr   error
	fanOutErr  error
	genesisErr error
	deployment *chain.Deployment
	loadErr    error

	calls []string
}

func (f *fakeWallet) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *fakeWallet) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeWallet) Funds(context.Context) (chain.Funds, error) {
	f.record("Funds")
	return f.funds, f.fundsErr
}

func (f *fakeWallet) FanOutFuel(_ context.Context, count, sats uint64) ([]chain.FuelResult, error) {
	f.record("FanOutFuel")
	if f.fanOutErr != nil {
		return nil, f.fanOutErr
	}
	// A real fan-out moves reserve into the pool; model that, so a second pass
	// does not mint again.
	f.mu.Lock()
	f.funds.PoolCoins += int(count)
	f.funds.Spendable += count * sats
	f.mu.Unlock()
	return []chain.FuelResult{{TxID: "fuel", Coins: count, Value: sats}}, nil
}

func (f *fakeWallet) Genesis(context.Context, *cellscript.Compiled, ca.Row) (*chain.Deployment, error) {
	f.record("Genesis")
	if f.genesisErr != nil {
		return nil, f.genesisErr
	}
	return &chain.Deployment{Cells: 8, GenesisTxID: "genesis"}, nil
}

func (f *fakeWallet) LoadDeployment() (*chain.Deployment, error) {
	f.record("LoadDeployment")
	if f.deployment != nil {
		return f.deployment, nil
	}
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return nil, errors.New("chain: read state.json: no such file or directory")
}

func (f *fakeWallet) FundingTarget() (*chain.FundingTarget, error) {
	return &chain.FundingTarget{Address: "mpz7rAwYignR5bybEGP4aZQbeikjxiRQ2U"}, nil
}

func testBoot(t *testing.T, w Wallet, mutate func(*Options)) *Boot {
	t.Helper()
	compiled, err := cellscript.Compile(8, ca.Rule110)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	seed, err := ca.NewRow(8)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	opts := Options{
		Seed:             seed,
		Throughput:       true,
		FuelDenomination: 1000,
		FirstFuelCoins:   16,
		MinBootstrap:     100_000,
		Network:          "ttn",
		Poll:             time.Millisecond,
	}
	if mutate != nil {
		mutate(&opts)
	}
	return New(w, compiled, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestNothingIsSpentBelowTheMinimum is the guard on the only irreversible thing
// this package does.
//
// A deposit smaller than the cold start needs buys a fuel pool and then cannot
// afford the genesis it was minted for — money spent for a deployment that
// still does not exist, and no way to get it back.
func TestNothingIsSpentBelowTheMinimum(t *testing.T) {
	w := &fakeWallet{funds: chain.Funds{Reserve: 99_999}}
	b := testBoot(t, w, nil)

	if d, _ := b.attempt(t.Context()); d != nil {
		t.Fatal("bootstrapped on a deposit below the minimum")
	}
	for _, call := range w.order() {
		if call == "FanOutFuel" || call == "Genesis" {
			t.Errorf("%s was called with %d satoshis against a minimum of 100000",
				call, w.funds.Reserve)
		}
	}
	if b.Phase() != PhaseFunding {
		t.Errorf("phase = %q, want %q", b.Phase(), PhaseFunding)
	}
}

// TestFuelIsMintedBeforeGenesis is the ordering assertion that is the whole
// reason this is a state machine and not two calls.
//
// Under the throughput strategy genesis is funded FROM the pool, so creating it
// first finds an empty pool and fails with "not enough funds" against a wallet
// that plainly holds money. That failure is the one this project already spent
// time on once, and the workaround on record was to disable the strategy.
func TestFuelIsMintedBeforeGenesis(t *testing.T) {
	w := &fakeWallet{funds: chain.Funds{Reserve: 5_000_000}}
	b := testBoot(t, w, nil)

	d, err := b.attempt(t.Context())
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if d == nil {
		t.Fatal("a funded wallet did not bootstrap")
	}

	var fuelAt, genesisAt = -1, -1
	for i, call := range w.order() {
		switch call {
		case "FanOutFuel":
			fuelAt = i
		case "Genesis":
			genesisAt = i
		}
	}
	if fuelAt < 0 {
		t.Fatal("no fuel was minted; genesis would be funded from an empty pool")
	}
	if genesisAt < 0 {
		t.Fatal("genesis was never attempted")
	}
	if fuelAt > genesisAt {
		t.Errorf("genesis ran before the fuel fan-out (calls: %v)", w.order())
	}
}

// With the privacy strategy there is no pool to fill, so the fuel phase must be
// skipped rather than minting into a basket the funder does not claim from.
func TestNoFuelPhaseWithoutAPool(t *testing.T) {
	w := &fakeWallet{funds: chain.Funds{Reserve: 5_000_000}}
	b := testBoot(t, w, func(o *Options) { o.Throughput = false })

	if _, err := b.attempt(t.Context()); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	for _, call := range w.order() {
		if call == "FanOutFuel" {
			t.Error("fuel was minted with no pool to mint it into")
		}
	}
}

// A genesis that already exists is the answer, not a failure. The machine must
// converge on it rather than retrying forever against a wallet that will keep
// refusing.
func TestAnExistingGenesisConvergesInsteadOfRetrying(t *testing.T) {
	w := &fakeWallet{
		funds: chain.Funds{Reserve: 5_000_000},
		genesisErr: errors.New(
			"chain: data/state.json already exists, so this deployment has already had a genesis; " +
				"move it aside deliberately if you really mean to abandon the existing cells"),
	}
	b := testBoot(t, w, nil)

	// The state file exists on the second look, which is exactly the race the
	// message describes.
	w.deployment = nil
	d, err := b.attempt(t.Context())
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if d != nil {
		t.Fatal("loaded a deployment the fake does not have")
	}

	w.deployment = &chain.Deployment{Cells: 8, GenesisTxID: "existing"}
	d, err = b.attempt(t.Context())
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if d == nil || d.GenesisTxID != "existing" {
		t.Fatalf("did not converge on the existing deployment: %+v", d)
	}
}

// A non-leader must spend nothing at all. Two instances bootstrapping means two
// fuel fan-outs and a race between two genesis transactions, on one wallet.
func TestANonLeaderSpendsNothing(t *testing.T) {
	w := &fakeWallet{funds: chain.Funds{Reserve: 5_000_000}}
	b := testBoot(t, w, func(o *Options) { o.Leader = func() bool { return false } })

	if d, _ := b.attempt(t.Context()); d != nil {
		t.Fatal("a non-leader bootstrapped")
	}
	if calls := w.order(); len(calls) != 0 {
		t.Errorf("a non-leader touched the wallet: %v", calls)
	}
	if b.Phase() != PhaseWaiting {
		t.Errorf("phase = %q, want %q", b.Phase(), PhaseWaiting)
	}
}

// A shortfall discovered during the fan-out means the estimate was optimistic,
// not that anything is broken. Going back to asking for money is recoverable;
// stopping is not.
func TestAShortfallDuringFuelGoesBackToAsking(t *testing.T) {
	w := &fakeWallet{
		funds:     chain.Funds{Reserve: 5_000_000},
		fanOutErr: errors.New("chain: fan out fuel (minted 0 of 16): not enough funds"),
	}
	b := testBoot(t, w, nil)

	if d, _ := b.attempt(t.Context()); d != nil {
		t.Fatal("bootstrapped despite the fan-out failing")
	}
	if b.Phase() != PhaseFunding {
		t.Errorf("phase = %q, want %q — a shortfall must ask for more, not stop", b.Phase(), PhaseFunding)
	}
	for _, call := range w.order() {
		if call == "Genesis" {
			t.Error("genesis was attempted after the fuel fan-out failed")
		}
	}
}

// The snapshot is the only thing a visitor can act on while the deployment does
// not exist, so it has to carry the address and the shortfall.
func TestTheSnapshotTellsAVisitorWhatToDo(t *testing.T) {
	w := &fakeWallet{funds: chain.Funds{Reserve: 25_000}}
	b := testBoot(t, w, nil)
	_, _ = b.attempt(t.Context())

	snap := b.Snapshot()
	if snap.Bootstrap == nil {
		t.Fatal("no bootstrap state on the snapshot; the UI cannot show a funding panel")
	}
	if snap.Bootstrap.Address == "" {
		t.Error("no address; there is nothing a visitor can do")
	}
	if snap.Bootstrap.MinSatoshis == 0 {
		t.Error("no minimum published, so a payer cannot know what will be refused")
	}
	if snap.Bootstrap.Have != 25_000 {
		t.Errorf("have = %d, want 25000", snap.Bootstrap.Have)
	}
	if snap.Cells != 8 {
		t.Errorf("cells = %d, want the compiled ring size", snap.Cells)
	}
}

// A phase change must wake subscribers, or a browser connected during the
// bootstrap sits on a stale frame until something else happens to notify.
func TestAPhaseChangeWakesSubscribers(t *testing.T) {
	w := &fakeWallet{funds: chain.Funds{Reserve: 1}}
	b := testBoot(t, w, nil)

	changed := b.Changed()
	b.setPhase(PhaseFuel, "")

	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("a phase change did not wake subscribers")
	}
}

// --- adoption -------------------------------------------------------------

// stubEngine stands in for the real engine after Adopt.
type stubEngine struct {
	mu      sync.Mutex
	changed chan struct{}
	calls   []string
}

func newStubEngine() *stubEngine { return &stubEngine{changed: make(chan struct{})} }

func (s *stubEngine) note(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, name)
}

// The adopted engine owns the real registry; the stub only has to satisfy the
// seam.
func (s *stubEngine) Metrics() *metrics.Registry { return metrics.NewRegistry() }

func (s *stubEngine) Snapshot() engine.Snapshot {
	s.note("Snapshot")
	return engine.Snapshot{Cells: 999}
}
func (s *stubEngine) SnapshotTail() engine.Snapshot {
	s.note("SnapshotTail")
	return engine.Snapshot{Cells: 999}
}
func (s *stubEngine) Stats() engine.Snapshot {
	s.note("Stats")
	return engine.Snapshot{Cells: 999}
}
func (s *stubEngine) PublishedTail() (*[]byte, bool) {
	s.note("PublishedTail")
	b := []byte(`{"engine":true}`)
	return &b, true
}
func (s *stubEngine) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}
func (s *stubEngine) SetMode(engine.Mode) { s.note("SetMode") }
func (s *stubEngine) SetRate(float64)     { s.note("SetRate") }
func (s *stubEngine) Step()               { s.note("Step") }

// TestAdoptHandsEverythingOverAndWakesSubscribers covers the handover, which
// has one failure mode that is silent and permanent.
//
// A browser connected during the bootstrap holds BOOT's change channel. Once
// the engine exists it notifies on its own, which that client never sees — so
// without a notification at the moment of adoption the connection blocks on a
// channel nothing will ever close again and the page stays on the last
// bootstrap frame for as long as it is open.
func TestAdoptHandsEverythingOverAndWakesSubscribers(t *testing.T) {
	w := &fakeWallet{}
	b := testBoot(t, w, nil)

	held := b.Changed()
	eng := newStubEngine()
	b.Adopt(eng)

	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("adoption did not wake a subscriber holding the bootstrap channel")
	}

	if got := b.Snapshot(); got.Cells != 999 {
		t.Errorf("Snapshot did not forward to the engine (cells = %d)", got.Cells)
	}
	if got := b.SnapshotTail(); got.Cells != 999 {
		t.Errorf("SnapshotTail did not forward (cells = %d)", got.Cells)
	}
	if got := b.Stats(); got.Cells != 999 {
		t.Errorf("Stats did not forward (cells = %d)", got.Cells)
	}
	data, ok := b.PublishedTail()
	if !ok || string(*data) != `{"engine":true}` {
		t.Error("PublishedTail did not forward to the engine")
	}

	b.SetMode(engine.ModeRunning)
	b.SetRate(2)
	b.Step()

	eng.mu.Lock()
	defer eng.mu.Unlock()
	for _, want := range []string{"SetMode", "SetRate", "Step"} {
		found := false
		for _, got := range eng.calls {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was not forwarded to the engine (saw %v)", want, eng.calls)
		}
	}
}

// Before adoption the clock controls must be harmless no-ops: there is no
// engine to drive, and a 500 from a button is worse than nothing happening.
func TestDrivingTheClockBeforeAdoptionIsHarmless(t *testing.T) {
	b := testBoot(t, &fakeWallet{}, nil)
	b.SetMode(engine.ModeRunning)
	b.SetRate(5)
	b.Step()
	if b.Snapshot().Bootstrap == nil {
		t.Error("driving the clock before adoption disturbed the bootstrap state")
	}
}

// The message genesis returns when it refuses a second run is matched on text,
// so this pins the text. If chain changes the wording, the cold start would
// silently stop converging and retry forever instead.
func TestTheAlreadyBootstrappedMessageIsRecognised(t *testing.T) {
	real := errors.New("chain: data/state.json already exists, so this deployment has already " +
		"had a genesis; move it aside deliberately if you really mean to abandon the existing cells")
	if !isAlreadyBootstrapped(real) {
		t.Error("the message genesis actually returns is no longer recognised")
	}
	if isAlreadyBootstrapped(errors.New("chain: not enough funds")) {
		t.Error("an unrelated failure was mistaken for an existing genesis")
	}
}

// A locked deployment must report itself locked from the FIRST byte it serves,
// not from the moment the engine is adopted. The browser reads this to decide
// whether to draw the clock controls at all, and a deployment that said false
// and then true would show them and take them away.
func TestALockedDeploymentIsLockedDuringTheBootstrap(t *testing.T) {
	b := testBoot(t, &fakeWallet{}, func(o *Options) { o.LockControls = true })

	if !b.Snapshot().Locked {
		t.Error("the bootstrap snapshot does not report the lock, so the UI would offer " +
			"controls the server refuses")
	}
	if !b.Stats().Locked {
		t.Error("Stats does not report the lock; handleControl reads it from there")
	}
}
