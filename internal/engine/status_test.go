package engine

import (
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"

	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// newTestEngine builds an engine with a real (temporary, SQLite) store and no
// network, which is enough to exercise the bookkeeping.
func newTestEngine(t *testing.T, cells int, gens ...uint64) *Engine {
	t.Helper()
	store, err := history.Open(t.Context(), "", t.TempDir())
	if err != nil {
		t.Fatalf("open history store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	e := &Engine{
		chain:   &chain.Chain{Config: chain.Config{ArcadeURL: "http://arcade.invalid"}},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		state:   &chain.State{Cells: cells, Chains: make([]chain.CellChain, cells)},
		store:   store,
		mode:    ModePaused,
		rate:    1,
		changed: make(chan struct{}),
		txIndex: map[string]txLoc{},
		halted:  map[int]bool{},
	}
	for _, n := range gens {
		g := Generation{Number: n, Cells: make([]CellTx, cells)}
		for i := range g.Cells {
			g.Cells[i] = CellTx{Cell: i, State: TxPending}
		}
		e.history = append(e.history, g)
	}
	return e
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
