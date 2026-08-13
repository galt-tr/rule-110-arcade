package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"

	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// newTestEngine builds an engine with a real (temporary, SQLite) store and no
// network, which is enough to exercise the bookkeeping.
func newTestEngine(t *testing.T, cells int, gens ...uint64) *Engine {
	t.Helper()
	return newTestEngineOpts(t, cells, true, gens...)
}

// newTestEngineQueued is newTestEngine WITHOUT the status writer running, for
// tests that inspect the write queue itself — a running writer drains it before
// the test can look.
func newTestEngineQueued(t *testing.T, cells int, gens ...uint64) *Engine {
	t.Helper()
	return newTestEngineOpts(t, cells, false, gens...)
}

func newTestEngineOpts(t *testing.T, cells int, startWriter bool, gens ...uint64) *Engine {
	t.Helper()
	store, err := history.Open(t.Context(), "", t.TempDir(), 0)
	if err != nil {
		t.Fatalf("open history store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	e := &Engine{
		chain:       &chain.Chain{Config: chain.Config{ArcadeURL: "http://arcade.invalid"}},
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		deployment:  &chain.Deployment{Cells: cells},
		tips:        make([]chain.CellChain, cells),
		store:       store,
		mode:        ModePaused,
		rate:        1,
		changed:     make(chan struct{}),
		txIndex:     map[string]txLoc{},
		halted:      map[int]bool{},
		haltReason:  map[int]string{},
		lastMined:   map[int]uint64{},
		lastSeen:    map[int]uint64{},
		unseenSince: make([]atomic.Int64, cells),
		needsRepair: map[int]bool{},
		retries:     map[int]retryState{},
		// Persistence is asynchronous now, so the tests run the real writer
		// rather than a stand-in — otherwise they would assert against a queue
		// nothing ever drains. Assertions on the store go through
		// waitForPersistedStatus.
		statusWrites:   make(chan history.StatusUpdate, statusWriteQueue),
		persistQueue:   make(chan persistRequest, persistQueueSize),
		persistStopped: make(chan struct{}),
		owner:          "test",
		perf:           newPerf(),
		raisedAt:       map[uint64]time.Time{},
		// The tests exercise a writer that has already re-derived under its
		// lease; the non-leader and re-derive paths have their own tests.
		leader: true,
	}
	for _, n := range gens {
		g := Generation{Number: n, Cells: make([]CellTx, cells)}
		for i := range g.Cells {
			g.Cells[i] = CellTx{Cell: i, State: TxPending}
		}
		e.history = append(e.history, g)
	}

	if startWriter {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); e.writeStatuses(ctx) }()
		t.Cleanup(func() {
			cancel()
			<-done
		})
	}
	startCommitter(t, e)
	return e
}

// startCommitter runs the durable-write committer for the life of the test.
//
// Not optional, unlike the status writer above. persist is a group commit and
// its caller BLOCKS on the reply, so an engine whose committer is not running
// does not merely fail to persist — the first write-ahead record never returns
// and the test deadlocks.
func startCommitter(t *testing.T, e *Engine) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.commitTxs(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// waitForPersistedStatus blocks until the store shows txid at want, or fails.
//
// The status write path is deliberately asynchronous — it must never make the
// SSE applier wait on the database — so a test that reads the store immediately
// after applying is racing the writer, not asserting on it.
func waitForPersistedStatus(t *testing.T, e *Engine, txid string, want history.Status) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last history.Status
	for time.Now().Before(deadline) {
		rows, err := e.store.Load(t.Context(), 0, 4096)
		if err != nil {
			t.Fatalf("load history: %v", err)
		}
		for _, g := range rows {
			for _, c := range g.Cells {
				if c.TxID == txid {
					last = c.Status
					if c.Status == want {
						return
					}
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("status of %s never reached %q (last saw %q)", txid, want, last)
}

// TestStatusSurvivesHistoryTrim is the regression guard for txIndex holding
// slice indices instead of generation numbers.
//
// The history window is trimmed from the front, so every index shifts down. A
// transaction broadcast at generation 100 and confirmed after two trims would
// have its status written into generation 102's row — silently, and to a cell
// that had nothing to do with it. Nothing in the program would have noticed.
func TestStatusSurvivesHistoryTrim(t *testing.T) {
	e := newTestEngine(t, 4, 100, 101, 102, 103)
	const txid = "aa"

	// The row has to exist in the store for the persistence half of this test to
	// assert anything: without it Unsettled returns nothing and the check below
	// passes for the wrong reason.
	if err := e.store.RecordGeneration(t.Context(), 100, ""); err != nil {
		t.Fatalf("record generation: %v", err)
	}
	if err := e.store.RecordTx(t.Context(), history.CellTx{
		Generation: 100, Cell: 2, TxID: txid, Status: history.StatusBroadcast,
	}); err != nil {
		t.Fatalf("record tx: %v", err)
	}

	e.mu.Lock()
	e.history[0].Cells[2] = CellTx{Cell: 2, TxID: txid, State: TxBroadcast}
	e.indexTx(txid, 100, 2, time.Now())
	e.mu.Unlock()

	// Two generations fall out of the window, shifting every index down by two.
	e.mu.Lock()
	e.history = e.history[2:]
	e.mu.Unlock()

	e.applyStatus(txid, arcade.StatusMined, "")

	for _, g := range e.history {
		for _, c := range g.Cells {
			if c.State == TxMined {
				t.Fatalf("status landed on generation %d cell %d, which never broadcast it",
					g.Number, c.Cell)
			}
		}
	}

	// And it must still have been persisted, since the store is the record.
	waitForPersistedStatus(t, e, txid, history.StatusMined)
	settled, err := e.store.Unsettled(t.Context())
	if err != nil {
		t.Fatalf("unsettled: %v", err)
	}
	for _, c := range settled {
		if c.TxID == txid {
			t.Error("a mined transaction trimmed out of the window was not persisted as terminal")
		}
	}
}

// TestStatusReachesTheRightCellAfterATrim is the positive case: a generation
// still inside the window gets its status, at the right index.
func TestStatusReachesTheRightCellAfterATrim(t *testing.T) {
	e := newTestEngine(t, 4, 100, 101, 102, 103)
	const txid = "bb"

	e.mu.Lock()
	e.history[3].Cells[1] = CellTx{Cell: 1, TxID: txid, State: TxBroadcast}
	e.indexTx(txid, 103, 1, time.Now())
	e.history = e.history[2:] // 103 is now at index 1, not 3
	e.mu.Unlock()

	e.applyStatus(txid, arcade.StatusMined, "")

	e.mu.RLock()
	defer e.mu.RUnlock()
	if got := e.history[1].Cells[1].State; got != TxMined {
		t.Errorf("generation 103 cell 1 state = %q, want %q", got, TxMined)
	}
	if _, still := e.txIndex[txid]; still {
		t.Error("a mined transaction must be dropped from the live index")
	}
}

// TestSnapshotDoesNotShareCellArrays runs under -race and reproduces what the
// HTTP handlers do: Snapshot() returns, the lock is released, and the caller
// marshals the result while the engine keeps writing.
//
// A shallow copy of []Generation leaves every Cells backing array shared, so the
// marshaller reads a CellTx.TxID string header while a writer replaces it.
func TestSnapshotDoesNotShareCellArrays(t *testing.T) {
	const cells = 16
	e := newTestEngine(t, cells, 1, 2, 3)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() { // the writer, holding the lock exactly as recordCell does
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			e.mu.Lock()
			for c := range cells {
				e.history[i%len(e.history)].Cells[c] = CellTx{
					Cell: c, TxID: "0123456789abcdef", State: TxSeen,
				}
			}
			e.mu.Unlock()
		}
	}()

	for range 200 {
		// Marshal OUTSIDE the lock — the whole point of the test.
		if _, err := json.Marshal(e.Snapshot()); err != nil {
			t.Fatalf("marshal snapshot: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestLastSeenTracksAcceptance is the other half of the acceptance gate: the
// gate is only as good as the signal feeding it, and a lastSeen that never
// advances would stall every cell for ever.
func TestLastSeenTracksAcceptance(t *testing.T) {
	e := newTestEngine(t, 4, 1)
	e.mu.Lock()
	e.indexTx("t-seen", 5, 0, time.Now())
	e.mu.Unlock()

	e.applyStatus("t-seen", arcade.StatusSeenOnNetwork, "")

	e.mu.RLock()
	got := e.lastSeen[0]
	e.mu.RUnlock()
	if got != 5 {
		t.Errorf("lastSeen[0] = %d after SEEN of generation 5, want 5", got)
	}
}

// MINED implies SEEN, and it has to be handled explicitly: SSE delivery is not
// ordered — rank() exists precisely because SEEN can arrive after MINED — so a
// cell whose SEEN frame was dropped but whose MINED arrived must not sit behind
// the acceptance gate for ever.
func TestLastSeenAdvancesOnMinedToo(t *testing.T) {
	e := newTestEngine(t, 4, 1)
	e.mu.Lock()
	e.indexTx("t-mined", 9, 1, time.Now())
	e.mu.Unlock()

	e.applyStatus("t-mined", arcade.StatusMined, "")

	e.mu.RLock()
	got := e.lastSeen[1]
	e.mu.RUnlock()
	if got != 9 {
		t.Errorf("lastSeen[1] = %d after MINED of generation 9, want 9 — mined implies seen", got)
	}
}

// A late, weaker frame must not drag the gate backwards and re-block a cell
// that has already moved on.
func TestLastSeenNeverRegresses(t *testing.T) {
	e := newTestEngine(t, 4, 1)
	e.mu.Lock()
	e.indexTx("t-new", 20, 2, time.Now())
	e.indexTx("t-old", 3, 2, time.Now())
	e.mu.Unlock()

	e.applyStatus("t-new", arcade.StatusSeenOnNetwork, "")
	e.applyStatus("t-old", arcade.StatusSeenOnNetwork, "")

	e.mu.RLock()
	got := e.lastSeen[2]
	e.mu.RUnlock()
	if got != 20 {
		t.Errorf("lastSeen[2] = %d after an out-of-order older frame, want 20", got)
	}
}

// TestRetryableRejectionDoesNotCountAgainstTheHaltBudget covers the signal we
// were throwing away.
//
// Arcade labels this class in as many words — "retryable — resubmit after the
// ancestor is accepted" — and the engine counted it towards halting the cell
// anyway. A refusal the network itself says will resolve is not evidence that
// anything is wrong with the transition.
func TestRetryableRejectionDoesNotCountAgainstTheHaltBudget(t *testing.T) {
	e := newTestEngine(t, 4, 1)
	const retryable = "parent rejected (ancestor 629f0309): retryable — resubmit after the ancestor is accepted"

	e.mu.Lock()
	for range maxRetries * 3 {
		e.noteRefusalLocked(0, 7, "tx", "REJECTED", retryable)
	}
	halted := e.halted[0]
	repair := e.needsRepair[0]
	e.mu.Unlock()

	if halted {
		t.Error("a cell was halted over refusals arcade itself called retryable")
	}
	if !repair {
		t.Error("a retryable refusal must still schedule a rebuild")
	}
}

// The pinned message. Detection is on arcade's wording because there is no
// structured field for it, so a reworded upstream message must fail here
// loudly rather than silently re-arming the halt that cost the ring.
func TestArcadeRetryableWordingIsStillRecognised(t *testing.T) {
	real := "parent rejected (ancestor 59cb0014a0da2d297f1f36bbb2332a653b0ee56fb15f14f8d22b254831d824b0): " +
		"retryable — resubmit after the ancestor is accepted"
	if !isRetryableRejection(arcade.StatusRejected, real) {
		t.Error("the message arcade actually sends is no longer recognised as retryable")
	}
	// The generic validation failure is NOT retryable-labelled and must still
	// count, or a genuinely broken transition would never halt.
	generic := "PROCESSING (4): [ProcessTransaction][abc] failed to validate transaction"
	if isRetryableRejection(arcade.StatusRejected, generic) {
		t.Error("an unlabelled rejection was treated as retryable")
	}
}

// TestHaltNeedsTimeNotJustAttempts is the other half of what killed the ring.
//
// maxRetries counted attempts, and rebuilds are immediate, so all three burned
// inside a four-second parent-acceptance lag. That is not three pieces of
// evidence about a transition, it is one — sampled three times in the same
// second.
func TestHaltNeedsTimeNotJustAttempts(t *testing.T) {
	e := newTestEngine(t, 4, 1)
	const generic = "PROCESSING (4): [ProcessTransaction][abc] failed to validate transaction"

	e.mu.Lock()
	for range maxRetries + 2 {
		e.noteRefusalLocked(0, 7, "tx", "REJECTED", generic)
	}
	halted := e.halted[0]
	e.mu.Unlock()

	if halted {
		t.Error("a cell halted on refusals that all landed within the same instant; " +
			"the budget has to span real time to be evidence of anything")
	}
}

// ...but a fault that persists past the window is real, and the cell must
// still halt rather than retrying for ever.
func TestHaltStillHappensOnceTheWindowHasPassed(t *testing.T) {
	e := newTestEngine(t, 4, 1)
	const generic = "PROCESSING (4): [ProcessTransaction][abc] failed to validate transaction"

	e.mu.Lock()
	e.noteRefusalLocked(0, 7, "tx", "REJECTED", generic)
	// Backdate the first refusal past the window, as a real outage would.
	st := e.retries[0]
	st.first = st.first.Add(-2 * minHaltWindow)
	e.retries[0] = st
	for range maxRetries {
		e.noteRefusalLocked(0, 7, "tx", "REJECTED", generic)
	}
	halted := e.halted[0]
	reason := e.haltReason[0]
	e.mu.Unlock()

	if !halted {
		t.Fatal("a fault that persisted past the window never halted the cell")
	}
	if reason == "" {
		t.Error("the cell halted with nothing to say about why")
	}
}

// A cell must not rebuild flat out for the whole halt window. The budget now
// spans a minute, so without a backoff a persistently refused generation would
// hammer arcade for that minute — on every affected cell at once, which on a
// 256-cell ring is how a transient network wobble becomes a self-inflicted one.
func TestRebuildsBackOff(t *testing.T) {
	e := newTestEngine(t, 4, 1)
	const generic = "PROCESSING (4): [ProcessTransaction][abc] failed to validate transaction"

	var pauses []time.Duration
	e.mu.Lock()
	for range 4 {
		e.noteRefusalLocked(0, 7, "tx", "REJECTED", generic)
		pauses = append(pauses, e.rebuildPauseLocked(0))
	}
	e.mu.Unlock()

	for i := 1; i < len(pauses); i++ {
		if pauses[i] <= pauses[i-1] {
			t.Errorf("pause %d (%v) did not grow on the previous (%v); refusals are not backing off",
				i, pauses[i], pauses[i-1])
		}
	}
	if pauses[len(pauses)-1] > maxRebuildPause {
		t.Errorf("pause %v exceeded the cap %v", pauses[len(pauses)-1], maxRebuildPause)
	}
}

// Turning FullStatusUpdates off has to cost the diagram nothing, and this is
// where that is decided: stateFor maps the three pre-mining statuses onto the
// SAME displayed state, so subscribing to all of them renders an identical
// picture to subscribing to the milestones.
//
// That is the entire argument for the default, and it is an argument about
// arcade's wire vocabulary rather than about our code — so it is worth a test
// that fails loudly if arcade ever adds a transition that means something new.
// The cost of getting it wrong is not cosmetic: every extra event is one the
// single-goroutine SSE fan-out cannot spend on an event we need.
func TestMilestoneStatusesReachEveryDisplayedState(t *testing.T) {
	full := map[arcade.Status]TxState{}
	for _, s := range []arcade.Status{
		arcade.StatusAcceptedByNetwork,
		arcade.StatusSeenOnNetwork,
		arcade.StatusSeenMultipleNodes,
		arcade.StatusMined,
		arcade.StatusRejected,
	} {
		st, ok := stateFor(s)
		if !ok {
			t.Fatalf("%s maps to no state at all", s)
		}
		full[s] = st
	}

	// The milestones: what arcade sends when FullStatusUpdates is off.
	milestones := []arcade.Status{
		arcade.StatusSeenOnNetwork,
		arcade.StatusMined,
		arcade.StatusRejected,
	}
	reachable := map[TxState]bool{}
	for _, s := range milestones {
		st, _ := stateFor(s)
		reachable[st] = true
	}

	for s, st := range full {
		if !reachable[st] {
			t.Errorf("%s displays as %q, which no milestone status reaches — turning "+
				"-full-status off would now lose a state from the diagram, and the "+
				"default must be reconsidered", s, st)
		}
	}

	// And the converse, which is the part that makes the extra events waste:
	// the two we stop paying for say nothing the milestones do not.
	for _, s := range []arcade.Status{arcade.StatusAcceptedByNetwork, arcade.StatusSeenMultipleNodes} {
		if got := full[s]; got != TxSeen {
			t.Errorf("%s now displays as %q rather than %q; it carries information the "+
				"milestones do not and is no longer free to drop", s, got, TxSeen)
		}
	}
}

// The event-budget warning must fire on the configuration that can still
// outrun arcade with the multiplier already off. Guarding it on
// FullStatusUpdates meant the one case an operator could not otherwise see was
// the one case that stayed silent.
func TestRateWarningDoesNotDependOnFullStatusUpdates(t *testing.T) {
	// SetRate logs inline on the calling goroutine, so a plain buffer is safe.
	var buf bytes.Buffer
	e := newTestEngine(t, 256)
	e.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	e.chain.Config.FullStatusUpdates = false
	e.deployment.Cells = 256

	// 256 cells x 2 milestones x 8 gen/s = 4,096 events/s, well past the budget.
	e.SetRate(8)

	if got := buf.String(); !strings.Contains(got, "arcade event budget") {
		t.Errorf("no budget warning with full-status off at a rate that exceeds it by "+
			"nearly 3x; logged: %q", got)
	}
}
