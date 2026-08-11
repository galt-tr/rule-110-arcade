package engine

import (
	"testing"
	"time"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"

	"github.com/dymurray/rule-110-arcade/internal/history"
)

// atGeneration puts every cell at g, as if the automaton had reached it.
func atGeneration(e *Engine, g uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.state.Chains {
		e.state.Chains[i].Cell = i
		e.state.Chains[i].Generation = g
		e.lastMined[i] = g
	}
	e.state.Generation = g
	e.target = g
}

// TestFrontierIsTheSlowestCell covers the change from a barrier to independent
// workers: with no barrier the automaton's position is the newest row EVERY
// cell has proved, not the newest any cell has reached.
func TestFrontierIsTheSlowestCell(t *testing.T) {
	e := newTestEngine(t, 4)
	atGeneration(e, 10)

	e.mu.Lock()
	e.state.Chains[0].Generation = 14
	e.state.Chains[1].Generation = 12
	e.state.Chains[2].Generation = 11 // the laggard
	e.state.Chains[3].Generation = 13
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
	e.state.Chains[2].Generation = 3
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
	e.target = 1000                  // the clock wants far more
	e.state.Chains[0].Generation = 5 // already 5 deep, nothing mined
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
	e.state.Chains[0].Generation = 22
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
	e.indexTx("cc", 7, 1)
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
// The write-ahead record turns that into a cell the operator can see.
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

	uncertain, err := e.store.UnresolvedAttempts(ctx)
	if err != nil {
		t.Fatalf("unresolved attempts: %v", err)
	}
	if got, ok := uncertain[2]; !ok || got != 300 {
		t.Fatalf("unresolved attempts = %v, want cell 2 at generation 300", uncertain)
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

	uncertain, err := e.store.UnresolvedAttempts(ctx)
	if err != nil {
		t.Fatalf("unresolved attempts: %v", err)
	}
	if len(uncertain) != 0 {
		t.Errorf("unresolved attempts = %v, want none once the attempt resolved", uncertain)
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

	uncertain, err := e.store.UnresolvedAttempts(ctx)
	if err != nil {
		t.Fatalf("unresolved attempts: %v", err)
	}
	if len(uncertain) != 0 {
		t.Errorf("unresolved attempts = %v, want none after retraction", uncertain)
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

	e.setLeader(true)
	if !e.turnReady(0) {
		t.Error("the leader must advance once it holds the lease")
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
