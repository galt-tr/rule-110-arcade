package engine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"

	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// atGeneration puts every cell at g, as if the automaton had reached it.
func atGeneration(e *Engine, g uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.tips {
		e.tips[i].Cell = i
		e.tips[i].Generation = g
		e.lastMined[i] = g
		// A cell that has REACHED g has had g accepted; otherwise every test
		// would trip the acceptance gate rather than what it means to exercise.
		e.lastSeen[i] = g
	}
	e.generation = g
	e.target = g
}

// TestFrontierIsTheSlowestCell covers the change from a barrier to independent
// workers: with no barrier the automaton's position is the newest row EVERY
// cell has proved, not the newest any cell has reached.
func TestFrontierIsTheSlowestCell(t *testing.T) {
	e := newTestEngine(t, 4)
	atGeneration(e, 10)

	e.mu.Lock()
	e.tips[0].Generation = 14
	e.tips[1].Generation = 12
	e.tips[2].Generation = 11 // the laggard
	e.tips[3].Generation = 13
	got := e.frontierLocked()
	e.mu.Unlock()

	if got != 11 {
		t.Errorf("frontier = %d, want 11 (the slowest cell)", got)
	}
}

// A halted cell can never advance again, so holding the frontier at its
// generation would freeze the automaton's reported position forever.
func TestFrontierIgnoresHaltedCells(t *testing.T) {
	e := newTestEngine(t, 4)
	atGeneration(e, 10)

	e.mu.Lock()
	e.tips[2].Generation = 3
	e.halted[2] = true
	got := e.frontierLocked()
	e.mu.Unlock()

	if got != 10 {
		t.Errorf("frontier = %d, want 10; a halted cell must not hold the frontier back", got)
	}
}

// TestDepthGateBlocksAndReleases is the governor that keeps a cell's unbroken
// chain of unconfirmed transactions inside the node's mempool ancestor limit.
// Past that limit the deepest transaction is rejected and the rejection
// cascades to every descendant, so this is the difference between backpressure
// and losing a whole run.
func TestDepthGateBlocksAndReleases(t *testing.T) {
	e := newTestEngine(t, 2)
	e.chain.Config.MaxUnconfirmedDepth = 5
	atGeneration(e, 0)

	e.mu.Lock()
	e.mode = ModeRunning
	e.target = 1000          // the clock wants far more
	e.tips[0].Generation = 5 // already 5 deep, nothing mined
	e.lastMined[0] = 0
	e.mu.Unlock()

	if e.turnReady(0) {
		t.Fatal("a cell at the depth limit must wait for a block, not keep building")
	}

	// A block confirms generation 3, shortening the chain to 2.
	e.mu.Lock()
	e.lastMined[0] = 3
	e.mu.Unlock()

	if !e.turnReady(0) {
		t.Error("the gate must reopen once mining shortens the unconfirmed chain")
	}
}

// TestStarvedEngineProbesAndResumes pins the behaviour the deployment is
// graded on: running out of coin stops the automaton cleanly, and it comes back
// by itself when coin arrives — no restart, no operator action beyond paying.
func TestStarvedEngineProbesAndResumes(t *testing.T) {
	e := newTestEngine(t, 2)
	atGeneration(e, 0)

	e.mu.Lock()
	e.mode = ModeStarved
	e.starvedSince = time.Now().Add(-time.Minute)
	e.fundingAddress = "mfaKEaddr"
	e.mu.Unlock()

	// Exactly one cell may retry per interval — not all of them, every loop.
	if !e.claimProbe() {
		t.Fatal("a starved engine must let one cell retry, or it can never resume")
	}
	if e.claimProbe() {
		t.Error("the retry must be rate limited, not granted to every cell at once")
	}

	// A retry that succeeds is what resumes the automaton.
	e.clearStarvation()

	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.mode != ModeRunning {
		t.Errorf("mode = %q after funding returned, want %q", e.mode, ModeRunning)
	}
	if !e.starvedSince.IsZero() {
		t.Error("the shortfall timer must be cleared, or the next blip starves immediately")
	}
	if e.lastError != "" {
		t.Errorf("lastError = %q, want it cleared once running again", e.lastError)
	}
}

// The snapshot has to say so, because a stopped automaton that looks healthy is
// worse than one that has crashed.
func TestStarvedSnapshotCarriesTheFundingAddress(t *testing.T) {
	e := newTestEngine(t, 2)
	atGeneration(e, 0)

	e.mu.Lock()
	e.mode = ModeStarved
	e.fundingAddress = "mfaKEaddr"
	e.mu.Unlock()

	s := e.Snapshot()
	if !s.Starved {
		t.Error("snapshot must report starved")
	}
	if s.FundingAddress != "mfaKEaddr" {
		t.Errorf("funding address = %q, want it surfaced so an operator can pay", s.FundingAddress)
	}
}

// TestSnapshotReportsBackpressure: lag means the cells cannot keep up with the
// requested rate; depth means mining cannot keep up with the cells. Both are
// backpressure, and both have to be visible or the reported rate is fiction.
func TestSnapshotReportsBackpressure(t *testing.T) {
	e := newTestEngine(t, 2)
	atGeneration(e, 10)

	e.mu.Lock()
	e.target = 30
	e.tips[0].Generation = 22
	e.lastMined[0] = 4
	e.mu.Unlock()

	s := e.Snapshot()
	if s.Lag != 20 { // target 30 - frontier 10
		t.Errorf("lag = %d, want 20", s.Lag)
	}
	if s.Depth != 18 { // cell 0 at 22, mined 4
		t.Errorf("depth = %d, want 18", s.Depth)
	}
}

// A mined status is what shortens a cell's unconfirmed chain, so it must move
// lastMined — otherwise the depth gate clamps shut and never reopens.
func TestMinedStatusReleasesDepth(t *testing.T) {
	e := newTestEngine(t, 2, 7)
	atGeneration(e, 0)

	e.mu.Lock()
	e.history[0].Cells[1] = CellTx{Cell: 1, TxID: "cc", State: TxBroadcast}
	e.indexTx("cc", 7, 1, time.Now())
	e.mu.Unlock()

	e.applyStatus("cc", arcade.StatusMined, "")

	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.lastMined[1] != 7 {
		t.Errorf("lastMined[1] = %d, want 7", e.lastMined[1])
	}
}

// TestPausedStillFinishesAStep: Step raises the target once, and pausing only
// freezes the target. A paused engine must therefore still let a cell that is
// behind catch up, or a single step would never complete and pausing would
// strand a half-finished generation.
func TestPausedStillFinishesAStep(t *testing.T) {
	e := newTestEngine(t, 2)
	atGeneration(e, 5)

	e.mu.Lock()
	e.mode = ModePaused
	e.mu.Unlock()

	if e.turnReady(0) {
		t.Fatal("nothing has been asked for yet, so no cell should advance")
	}

	e.Step()

	if !e.turnReady(0) {
		t.Error("a step must reach the cells even while paused")
	}
}

// A step asks for exactly one generation, not an open-ended run.
func TestStepAdvancesTheTargetByOne(t *testing.T) {
	e := newTestEngine(t, 2)
	atGeneration(e, 5)

	e.Step()
	e.mu.RLock()
	got := e.target
	e.mu.RUnlock()

	if got != 6 {
		t.Errorf("target = %d after one step from generation 5, want 6", got)
	}
}

// TestWriteAheadSurvivesACrash covers the failure that killed cells 34 and 51.
//
// Signing broadcasts, so there is a window where a transition is on the network
// and we have not recorded it. A process killed there used to come back a
// generation behind, re-spend an output the network had already consumed, and
// lose the cell to a rejection indistinguishable from a real failure.
//
// The write-ahead record turns that into a cell the operator can see: the
// newest record for the cell is the unresolved attempt, which is what marks its
// tip UNKNOWN at the next start.
func TestWriteAheadSurvivesACrash(t *testing.T) {
	e := newTestEngine(t, 4)
	ctx := t.Context()

	// A transition is about to be attempted for cell 2...
	if err := e.store.RecordTx(ctx, history.CellTx{
		Generation: 300, Cell: 2, Status: history.StatusAttempting,
	}); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	// ...and the process dies here, before the result is known.

	tips, err := e.store.CellTips(ctx, 4, tipDepth)
	if err != nil {
		t.Fatalf("cell tips: %v", err)
	}
	got := tips[2]
	if len(got) != 1 || got[0].Generation != 300 || got[0].Status != history.StatusAttempting {
		t.Fatalf("cell 2 tips = %v, want one unresolved attempt at generation 300", got)
	}
}

// A resolved attempt is not a crash, and must not stall the cell.
func TestResolvedAttemptIsNotFlagged(t *testing.T) {
	e := newTestEngine(t, 4)
	ctx := t.Context()

	for _, tx := range []history.CellTx{
		{Generation: 300, Cell: 2, Status: history.StatusAttempting},
		{Generation: 300, Cell: 2, TxID: "abc", Status: history.StatusBroadcast},
	} {
		if err := e.store.RecordTx(ctx, tx); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	tips, err := e.store.CellTips(ctx, 4, tipDepth)
	if err != nil {
		t.Fatalf("cell tips: %v", err)
	}
	got := tips[2]
	if len(got) != 1 || got[0].Status != history.StatusBroadcast {
		t.Fatalf("cell 2 tips = %v, want the resolved broadcast record to have replaced the attempt", got)
	}
}

// A failure before broadcast leaves the UTXO untouched, so its write-ahead
// record must be retracted — otherwise every funding shortfall would look like
// a lost broadcast at the next startup, and shortfalls are routine.
func TestRetractedAttemptIsNotFlagged(t *testing.T) {
	e := newTestEngine(t, 4)
	ctx := t.Context()

	if err := e.store.RecordTx(ctx, history.CellTx{
		Generation: 300, Cell: 2, Status: history.StatusAttempting,
	}); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	e.retractAttempt(300, 2)

	tips, err := e.store.CellTips(ctx, 4, tipDepth)
	if err != nil {
		t.Fatalf("cell tips: %v", err)
	}
	if len(tips[2]) != 0 {
		t.Errorf("cell 2 tips = %v, want none after retraction", tips[2])
	}
}

// Retraction must never touch a real transaction.
func TestRetractionSparesARealTransaction(t *testing.T) {
	e := newTestEngine(t, 4)
	ctx := t.Context()

	if err := e.store.RecordGeneration(ctx, 300, "00"); err != nil {
		t.Fatalf("record generation: %v", err)
	}
	if err := e.store.RecordTx(ctx, history.CellTx{
		Generation: 300, Cell: 2, TxID: "abc", Status: history.StatusBroadcast,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	e.retractAttempt(300, 2)

	gens, err := e.store.Load(ctx, 300, 1)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	found := false
	for _, g := range gens {
		for _, c := range g.Cells {
			if c.Cell == 2 && c.TxID == "abc" {
				found = true
			}
		}
	}
	if !found {
		t.Error("retraction deleted a broadcast transaction; it must only remove write-ahead records")
	}
}

// engineOn builds an engine over a fixture's store, deployment and contract.
//
// Distinct from newTestEngine because the property the next two tests are about
// spans both halves of the program: the worker decides what to write, and
// DERIVATION — the same code the next startup runs — decides whether what was
// written halts the cell for ever. That question can only be asked of a store
// the fixture is also able to derive from.
func engineOn(t *testing.T, f *fixture) *Engine {
	t.Helper()
	e := &Engine{
		chain:        &chain.Chain{Config: chain.Config{ArcadeURL: "http://arcade.invalid"}},
		compiled:     f.compiled,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		deployment:   f.facts,
		tips:         make([]chain.CellChain, f.facts.Cells),
		store:        f.store,
		mode:         ModeRunning,
		rate:         1,
		changed:      make(chan struct{}),
		txIndex:      map[string]txLoc{},
		halted:       map[int]bool{},
		haltReason:   map[int]string{},
		lastMined:    map[int]uint64{},
		refusedAt:    map[int]time.Time{},
		stalledUntil: map[int]time.Time{},
		stallStreak:  map[int]int{},
		stallReason:  map[int]string{},
		unseenSince:  make([]atomic.Int64, f.facts.Cells),
		// The recovery seams, so a repair can be driven against the fixture's
		// ledger rather than a wallet. See Engine.ledger.
		ledger:         f.ledger,
		oracle:         noArcade,
		retries:        map[int]retryState{},
		needsRepair:    map[int]bool{},
		waitingOnCoin:  map[int]bool{},
		statusWrites:   make(chan history.StatusUpdate, statusWriteQueue),
		persistQueue:   make(chan persistRequest, persistQueueSize),
		persistStopped: make(chan struct{}),
		owner:          "test",
		leader:         true,
		perf:           newPerf(),
		raisedAt:       map[uint64]time.Time{},
	}
	for cell := range f.facts.Cells {
		e.tips[cell] = f.genesisTip(cell)
	}
	startCommitter(t, e)
	return e
}

// TestALocalFailureDoesNotHaltACell is the live bug.
//
// chain.ErrNotBroadcast is set immediately before SignAction, and signing is what
// broadcasts, so it means with certainty that nothing was signed and nothing
// reached the network. The worker retracted its write-ahead record on it — and
// then recorded a `failed` row anyway, which is exactly what DeriveTips halts a
// cell on, permanently and at every startup after. 21 of the live deployment's
// 128 cells were killed this way by one burst of "sorry, too many clients
// already" inside CreateAction, every one of them with an empty txid, none of
// them having spent anything.
//
// A database hiccup must cost a retry, not a cell.
func TestALocalFailureDoesNotHaltACell(t *testing.T) {
	f := newFixture(t)
	e := engineOn(t, f)
	const cell = 4

	// One real generation, so the shape under test is a failure sitting directly
	// on a tip rather than a cell that has never done anything.
	good := f.advance(t, cell, f.genesisTip(cell), history.StatusBroadcast)
	e.mu.Lock()
	e.tips[cell] = good
	// Unreadable stored bytes: a local fault that stops AdvanceCell before it can
	// sign. WHICH local fault does not matter — CreateAction failing on a database
	// connection is the one that happened — only that it comes back wrapped in
	// ErrNotBroadcast, which is the claim being acted on.
	e.tips[cell].RawTxHex = "not hex"
	e.target = good.Generation + 1
	e.mu.Unlock()

	e.advanceCell(t.Context(), cell)

	// The failure is still visible to whoever is watching...
	e.mu.RLock()
	lastErr, halted := e.lastError, e.halted[cell]
	e.mu.RUnlock()
	if !strings.Contains(lastErr, chain.ErrNotBroadcast.Error()) {
		t.Errorf("lastError = %q, want the failure surfaced", lastErr)
	}
	if halted {
		t.Error("the cell was halted in memory for a failure that never reached the network")
	}
	if !e.turnReady(cell) {
		t.Error("the cell is not free to try that generation again on its next turn")
	}

	// ...and nothing durable was written. This is the assertion that matters: a
	// restart must not find a reason to halt.
	p := f.derive(t)[cell]
	if p.Halted || p.Unknown {
		t.Fatalf("cell %d came back halted after a purely local failure: %+v", cell, p)
	}
	if p.Tip.TxID != good.TxID || p.Tip.Generation != good.Generation {
		t.Errorf("tip = %s at generation %d, want the unchanged %s at %d",
			p.Tip.TxID, p.Tip.Generation, good.TxID, good.Generation)
	}
	tips, err := f.store.CellTips(t.Context(), f.facts.Cells, tipDepth)
	if err != nil {
		t.Fatalf("cell tips: %v", err)
	}
	for _, row := range tips[cell] {
		if row.Generation > good.Generation {
			t.Errorf("the store holds a %q row at generation %d above the tip; the write-ahead record "+
				"must be retracted and no failure recorded", row.Status, row.Generation)
		}
	}

	// And the cell really does go on to make the generation it failed at.
	next := f.advance(t, cell, p.Tip, history.StatusBroadcast)
	again := f.derive(t)[cell]
	if again.Halted || again.Tip.TxID != next.TxID {
		t.Errorf("cell %d = %+v, want advanced onto %s", cell, again, next.TxID)
	}
}

// TestAFailureThatMayHaveBroadcastStillHaltsTheCell is the other half, and the
// half that must not move.
//
// This error carries no sentinel, so it comes from at or after SignAction — the
// point past which the transaction may exist on the network whatever the error
// says. Retrying that generation would be a second transaction spending an
// output the network may already have consumed, which is the double spend the
// whole of this design exists to avoid. The cell halts, durably, and a human or
// `rule110 recover` decides.
func TestAFailureThatMayHaveBroadcastStillHaltsTheCell(t *testing.T) {
	f := newFixture(t)
	e := engineOn(t, f)
	const cell = 4

	good := f.advance(t, cell, f.genesisTip(cell), history.StatusBroadcast)
	e.mu.Lock()
	e.tips[cell] = good
	e.mu.Unlock()

	gen := good.Generation + 1
	if !e.persist(history.CellTx{Generation: gen, Cell: cell, Status: history.StatusAttempting}) {
		t.Fatal("write-ahead record could not be written")
	}
	e.transitionFailed(t.Context(), gen, cell, errors.New(
		"chain: sign step action for cell 4: rpc error: connection reset by peer"))

	p := f.derive(t)[cell]
	if !p.Halted {
		t.Fatal("a cell whose transition may have been signed came back running; " +
			"it would re-spend an output the network may already hold")
	}
	if p.Tip.TxID != good.TxID {
		t.Errorf("tip = %s, want it left at the last transaction actually broadcast (%s)",
			p.Tip.TxID, good.TxID)
	}
	if !strings.Contains(p.HaltReason, "connection reset") {
		t.Errorf("the halt should carry the failure that caused it, got: %q", p.HaltReason)
	}
}

// TestNonLeaderNeverAdvances is the guard against the worst failure this
// deployment can have.
//
// The engine owns 128 live UTXO chains. Two instances advancing them would
// double-spend every one, and a Kubernetes rolling update runs two pods at once
// by design — so an instance that does not hold the lease must not advance a
// cell for any reason, whatever the clock says.
func TestNonLeaderNeverAdvances(t *testing.T) {
	e := newTestEngine(t, 4)
	atGeneration(e, 10)

	e.mu.Lock()
	e.mode = ModeRunning
	e.target = 1000 // the clock is asking for plenty
	e.leader = false
	e.mu.Unlock()

	for cell := range 4 {
		if e.turnReady(cell) {
			t.Fatalf("cell %d advanced without holding the writer lease", cell)
		}
	}

	// Starved must not be a way around it either: the probe would broadcast.
	e.mu.Lock()
	e.mode = ModeStarved
	e.mu.Unlock()
	if e.turnReady(0) {
		t.Error("a starved non-leader probed anyway; the probe broadcasts a real transaction")
	}

	// Gaining the lease is not on its own permission to advance: see
	// TestReacquiringTheLeaseForcesARederive.
	e.setLeader(true)
	if e.turnReady(0) {
		t.Error("a leader advanced before re-deriving its tips under the lease it now holds")
	}
	e.mu.Lock()
	e.needsRederive = false
	e.mu.Unlock()
	if !e.turnReady(0) {
		t.Error("the leader must advance once it holds the lease and has re-derived")
	}
}

// TestReacquiringTheLeaseForcesARederive is bug 9b.
//
// holdLease flips leader back to true after any store error, and it cannot tell
// a database hiccup from a lease that genuinely expired while another writer
// took over and advanced all 128 chains. In the second case every tip in memory
// is stale, so resuming from them double-spends the whole ring at once. The
// engine therefore treats every acquisition as the second case and refuses to
// advance until the tips have been re-derived from the store.
func TestReacquiringTheLeaseForcesARederive(t *testing.T) {
	e := newTestEngine(t, 4)
	atGeneration(e, 10)

	e.mu.Lock()
	e.mode = ModeRunning
	e.target = 1000
	e.needsRederive = false // settled: this instance has been advancing happily
	e.mu.Unlock()

	if !e.turnReady(0) {
		t.Fatal("a settled leader should be advancing")
	}

	// The store blinks; the next renewal succeeds.
	e.setLeader(false)
	e.setLeader(true)

	e.mu.RLock()
	needs := e.needsRederive
	e.mu.RUnlock()
	if !needs {
		t.Fatal("re-acquiring the lease must force a re-derivation, or a stale tip is spent")
	}
	for cell := range 4 {
		if e.turnReady(cell) {
			t.Fatalf("cell %d advanced on tips that were never re-derived under the new lease", cell)
		}
	}
}

// TestLeaseIsExclusiveAndReclaimable pins the two properties the deployment
// depends on: only one holder at a time, and a dead holder does not wedge the
// automaton forever.
func TestLeaseIsExclusiveAndReclaimable(t *testing.T) {
	e := newTestEngine(t, 2)
	ctx := t.Context()

	held, err := e.store.AcquireLease(ctx, "l", "pod-a", time.Minute)
	if err != nil || !held {
		t.Fatalf("first acquire: held=%v err=%v", held, err)
	}
	if held, err := e.store.AcquireLease(ctx, "l", "pod-b", time.Minute); err != nil || held {
		t.Fatalf("a second instance took a live lease: held=%v err=%v", held, err)
	}
	if held, err := e.store.AcquireLease(ctx, "l", "pod-a", time.Minute); err != nil || !held {
		t.Fatalf("the holder could not renew: held=%v err=%v", held, err)
	}

	// An expired lease is reclaimable, or a crashed pod would stop the
	// automaton permanently.
	if _, err := e.store.AcquireLease(ctx, "l", "pod-a", -time.Second); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if held, err := e.store.AcquireLease(ctx, "l", "pod-b", time.Minute); err != nil || !held {
		t.Fatalf("an expired lease was not reclaimed: held=%v err=%v", held, err)
	}

	// A clean release hands over without waiting out the TTL.
	if err := e.store.ReleaseLease(ctx, "l", "pod-b"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if held, err := e.store.AcquireLease(ctx, "l", "pod-a", time.Minute); err != nil || !held {
		t.Fatalf("a released lease was not available: held=%v err=%v", held, err)
	}
}

// TestWriteAheadFailureDoesNotBroadcast is the other half of bug 9d, and the
// half that was still open.
//
// Halting a cell stops its NEXT turn. It does not unwind the turn already
// running. So persist could mark a cell halted for an unwritable write-ahead
// record and advanceCell would carry straight on to build, sign and broadcast
// the transition anyway — spending the tip with nothing durable saying the
// attempt was ever made. The next startup then derives the tip one generation
// back and re-spends an output the network has already consumed, which is the
// exact double spend the write-ahead record exists to prevent.
//
// Not hypothetical: an exhausted database connection pool halted 70 cells this
// way on the live deployment, every one of which had broadcast regardless.
//
// The signal is which error surfaces. Returning early leaves the persist
// failure as the last error; going on to build a transition replaces it with
// one from the chain layer, and reaching the chain layer at all is the bug.
func TestWriteAheadFailureDoesNotBroadcast(t *testing.T) {
	e := newTestEngine(t, 4)
	atGeneration(e, 10)

	// Break the store the way an exhausted connection pool does.
	if err := e.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	e.advanceCell(t.Context(), 2)

	e.mu.RLock()
	halted, lastErr, tipGen := e.halted[2], e.lastError, e.tips[2].Generation
	e.mu.RUnlock()

	if strings.Contains(lastErr, "chain:") {
		t.Errorf("advanceCell built a transition after its write-ahead record could not be "+
			"written, so the tip would be spent with no durable record of the attempt: %s", lastErr)
	}
	if !strings.Contains(lastErr, "could not be recorded") {
		t.Errorf("the write-ahead failure should be the reported error, got: %q", lastErr)
	}
	if !halted {
		t.Error("the cell must also be halted, so it does not simply retry on its next turn")
	}
	if tipGen != 10 {
		t.Errorf("the tip moved to generation %d despite nothing being recorded", tipGen)
	}
}

// TestPersistFailureHaltsTheCell is bug 9d.
//
// The history store is now the ONLY record of where a cell is — there is no
// state file behind it. A row that cannot be written therefore means the next
// startup derives the tip one generation back and spends an output the network
// has already consumed, which is the same double spend this whole rework exists
// to prevent, arrived at from the other direction. Logging and carrying on is
// not an option any more.
func TestPersistFailureHaltsTheCell(t *testing.T) {
	e := newTestEngine(t, 4)
	atGeneration(e, 10)

	// Break the store the way a lost connection does.
	if err := e.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	e.persist(history.CellTx{
		Generation: 11, Cell: 2, TxID: "abc", Status: history.StatusBroadcast,
	})

	e.mu.RLock()
	halted, reason, lastErr := e.halted[2], e.haltReason[2], e.lastError
	e.mu.RUnlock()

	if !halted {
		t.Fatal("a cell whose transaction could not be recorded kept advancing; " +
			"its next start would re-spend a spent output")
	}
	if reason == "" || lastErr == "" {
		t.Errorf("the halt must be explained (reason=%q lastError=%q)", reason, lastErr)
	}
	if e.turnReady(2) {
		t.Error("a halted cell must not be ready to advance")
	}
}

// TestHaltingTheSlowestCellDoesNotFreezeTheClock is the underflow that took the
// whole automaton down on the live deployment.
//
// frontierLocked is the minimum generation over cells that are NOT halted, so
// halting the slowest cell removes it from that minimum and the frontier jumps
// UPWARD — past the target, which had been tracking the laggard. The clock's
// `target - frontier < maxLag` is unsigned, so it underflowed to about 2^64,
// which is not less than maxLag, and the clock never raised the target again.
//
// Nothing said so. Snapshot's Lag saturates, so the UI reported a calm lag of 0
// while the clock was dead, and derivation reproduced the same shape on restart.
// Observed: cell 44 was the slowest cell at generation 905, was refused, halted,
// and 115 healthy cells stopped with it.
func TestHaltingTheSlowestCellDoesNotFreezeTheClock(t *testing.T) {
	e := newTestEngine(t, 4)
	atGeneration(e, 100)

	e.mu.Lock()
	e.mode = ModeRunning
	e.chain.Config.MaxLag = 32
	// Cell 0 is the laggard the target is tracking; everyone else is far ahead,
	// which is the shape recovery produces when it resumes long-halted cells.
	e.tips[0].Generation = 100
	for i := 1; i < 4; i++ {
		e.tips[i].Generation = 900
	}
	e.target = 100
	e.mu.Unlock()

	// The laggard is refused and halts. The frontier is now 900, past the target.
	e.haltCell(0, "refused")

	e.mu.RLock()
	frontier, target := e.frontierLocked(), e.target
	e.mu.RUnlock()
	if frontier <= target {
		t.Fatalf("frontier %d must exceed target %d for this test to be about anything", frontier, target)
	}

	ctx, cancel := context.WithCancel(t.Context())
	go e.clock(ctx)
	defer cancel()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.RLock()
		moved := e.target > target
		e.mu.RUnlock()
		if moved {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.mu.RLock()
	got := e.target
	e.mu.RUnlock()
	t.Fatalf("the clock never raised the target past %d after the slowest cell halted and the "+
		"frontier jumped to %d; the automaton is frozen and Lag reports 0", got, frontier)
}

// TestSeenGateBlocksUntilTheParentIsAccepted is the regression guard for the
// outage that cost all 256 cells of the public deployment.
//
// Arcade's 202 means "accepted for processing", NOT "validated" and NOT "in a
// mempool". The engine used to advance a cell the moment that 202 came back, so
// generation N+1 was submitted while N was still in arcade's intake queue —
// and the network rejected the child for spending an output it had not yet
// heard of. Measured on the live deployment: children rejected 0.9 to 21.7
// seconds BEFORE their parents were accepted, every one of those parents
// perfectly valid and `seen` today.
//
// The toolbox already holds every wallet-managed coin at TierSending until
// arcade reports SEEN, for exactly this reason. The cell's continuation output
// is not a wallet-managed coin, so this is that same rule applied by hand.
func TestSeenGateBlocksUntilTheParentIsAccepted(t *testing.T) {
	e := newTestEngine(t, 2)
	e.chain.Config.MaxUnseenDepth = 1
	atGeneration(e, 0)

	e.mu.Lock()
	e.mode = ModeRunning
	e.target = 1000
	// Generation 1 was broadcast and arcade returned 202, so the tip advanced.
	// Nothing has been seen beyond generation 0.
	e.tips[0].Generation = 1
	e.lastSeen[0] = 0
	e.mu.Unlock()

	if e.turnReady(0) {
		t.Fatal("a cell built generation 2 while generation 1 was still unaccepted; " +
			"that is the race that destroyed the ring")
	}

	// Arcade reports SEEN for generation 1.
	e.mu.Lock()
	e.lastSeen[0] = 1
	e.mu.Unlock()

	if !e.turnReady(0) {
		t.Error("the gate must reopen the moment the parent is accepted")
	}
}

// Zero disables the gate, the way MaxUnconfirmedDepth does. A deployment that
// wants the old behaviour must be able to ask for it explicitly.
func TestSeenGateDisabledByZero(t *testing.T) {
	e := newTestEngine(t, 2)
	e.chain.Config.MaxUnseenDepth = 0
	atGeneration(e, 0)

	e.mu.Lock()
	e.mode = ModeRunning
	e.target = 1000
	e.tips[0].Generation = 40
	e.lastSeen[0] = 0
	e.mu.Unlock()

	if !e.turnReady(0) {
		t.Error("MaxUnseenDepth=0 must mean unbounded, not stopped — an unset field " +
			"that silently freezes the automaton is the worst way to fail")
	}
}

// A repair pulls a tip BACKWARD, so tip-lastSeen underflows on unsigned
// integers and wraps to an enormous number. Without an explicit guard that
// clamps the cell shut for ever, and the cause is invisible.
func TestSeenGateSurvivesATipPulledBackwards(t *testing.T) {
	e := newTestEngine(t, 2)
	e.chain.Config.MaxUnseenDepth = 1
	atGeneration(e, 0)

	e.mu.Lock()
	e.mode = ModeRunning
	e.target = 1000
	// repairCell re-derived the tip to 5 while the status pipeline had already
	// recorded 7 as seen.
	e.tips[0].Generation = 5
	e.lastSeen[0] = 7
	e.mu.Unlock()

	if !e.turnReady(0) {
		t.Error("a tip behind lastSeen wrapped the unsigned subtraction and clamped the cell shut")
	}
}

// The two gates are different questions and must both be enforced: one asks
// whether the network ACCEPTED the parent, the other whether it CONFIRMED it.
func TestSeenGateAndDepthGateAreIndependent(t *testing.T) {
	e := newTestEngine(t, 2)
	e.chain.Config.MaxUnseenDepth = 1
	e.chain.Config.MaxUnconfirmedDepth = 5
	atGeneration(e, 0)

	e.mu.Lock()
	e.mode = ModeRunning
	e.target = 1000
	// Everything seen, but far past the confirmation depth limit.
	e.tips[0].Generation = 10
	e.lastSeen[0] = 10
	e.lastMined[0] = 0
	e.mu.Unlock()

	if e.turnReady(0) {
		t.Error("the depth gate stopped working when the seen gate was satisfied")
	}
}

// TestChildIsNeverBuiltBeforeTheParentIsAccepted is the incident, reproduced.
//
// This is the whole outage in one property. Model the network as arcade
// behaved: a transaction is refused if its parent has not yet been accepted at
// the moment it arrives. Then drive a cell forward and assert it never
// submits into that condition.
//
// Without the acceptance gate the cell walks straight into it — which is what
// happened to all 256 cells of the public deployment inside two minutes, over
// parents that were all perfectly valid and are all `seen` today.
func TestChildIsNeverBuiltBeforeTheParentIsAccepted(t *testing.T) {
	e := newTestEngine(t, 2)
	e.chain.Config.MaxUnseenDepth = 1
	atGeneration(e, 0)

	e.mu.Lock()
	e.mode = ModeRunning
	e.target = 50
	e.mu.Unlock()

	// The network: a generation is accepted only once its parent has been.
	accepted := uint64(0) // generation 0 is genesis, already on chain
	submitted := []uint64{}

	for range 200 {
		if !e.turnReady(0) {
			// Blocked. The only thing that can unblock it is the network
			// accepting what we already sent — so let it.
			e.mu.Lock()
			tip := e.tips[0].Generation
			e.mu.Unlock()
			if tip > accepted {
				accepted = tip
				e.mu.Lock()
				e.lastSeen[0] = accepted
				e.mu.Unlock()
			}
			continue
		}

		e.mu.Lock()
		next := e.tips[0].Generation + 1
		parent := next - 1
		e.mu.Unlock()

		// THE ASSERTION. Submitting a child whose parent the network has not
		// taken is the bug; arcade refuses it for an input that does not exist.
		if parent > accepted {
			t.Fatalf("submitted generation %d while its parent %d was still unaccepted "+
				"(network has accepted up to %d) — this is the rejection cascade",
				next, parent, accepted)
		}
		submitted = append(submitted, next)

		e.mu.Lock()
		e.tips[0].Generation = next
		e.mu.Unlock()
	}

	if len(submitted) < 10 {
		t.Fatalf("the gate deadlocked: only %d generations advanced, so this test would "+
			"pass by never doing anything", len(submitted))
	}
}
