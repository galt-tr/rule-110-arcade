package engine

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
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
	store, err := history.Open(t.Context(), "", t.TempDir())
	if err != nil {
		t.Fatalf("open history store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	e := &Engine{
		chain:      &chain.Chain{Config: chain.Config{ArcadeURL: "http://arcade.invalid"}},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		deployment: &chain.Deployment{Cells: cells},
		tips:       make([]chain.CellChain, cells),
		store:      store,
		mode:       ModePaused,
		rate:       1,
		changed:    make(chan struct{}),
		txIndex:    map[string]txLoc{},
		halted:     map[int]bool{},
		haltReason: map[int]string{},
		lastMined:  map[int]uint64{},
		lastSeen:   map[int]uint64{},
		// Persistence is asynchronous now, so the tests run the real writer
		// rather than a stand-in — otherwise they would assert against a queue
		// nothing ever drains. Assertions on the store go through
		// waitForPersistedStatus.
		statusWrites: make(chan history.StatusUpdate, statusWriteQueue),
		owner:        "test",
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
	return e
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
	e.indexTx(txid, 100, 2)
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
	e.indexTx(txid, 103, 1)
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
	e.indexTx("t-seen", 5, 0)
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
	e.indexTx("t-mined", 9, 1)
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
	e.indexTx("t-new", 20, 2)
	e.indexTx("t-old", 3, 2)
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
