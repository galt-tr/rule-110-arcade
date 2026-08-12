package engine

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// The invariant the write-ahead record exists for, and the one batching could
// most easily have broken: when persist returns true the row is ALREADY in the
// store. A queue that returned as soon as the row was accepted would let a cell
// broadcast with nothing durable saying it had tried.
func TestPersistReturnsOnlyAfterTheRowIsDurable(t *testing.T) {
	e := newTestEngine(t, 4)
	if err := e.store.RecordGeneration(t.Context(), 12, "aa"); err != nil {
		t.Fatal(err)
	}

	if !e.persist(history.CellTx{Generation: 12, Cell: 1, Status: history.StatusAttempting}) {
		t.Fatal("persist failed")
	}

	// No waiting, no polling: if this is not already there, the guarantee is
	// broken. Read through the store rather than any in-memory state.
	rows, err := e.store.Load(t.Context(), 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range rows {
		if g.Number != 12 {
			continue
		}
		for _, c := range g.Cells {
			if c.Cell == 1 && c.Status == history.StatusAttempting {
				return
			}
		}
	}
	t.Fatal("persist returned true but the row was not in the store")
}

// A cell whose record cannot be written must stop advancing. This is the whole
// reason persist reports anything at all: halting stops the cell's NEXT turn, so
// the caller has to check the result before broadcasting.
func TestPersistHaltsTheCellWhenTheCommitFails(t *testing.T) {
	e := newTestEngine(t, 4)
	// Every write from here on fails.
	if err := e.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	ok := e.persist(history.CellTx{Generation: 3, Cell: 2, Status: history.StatusAttempting})

	if ok {
		t.Fatal("persist reported success after the commit failed; the caller would broadcast")
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.halted[2] {
		t.Error("the cell was not halted, so it would try again with no durable record")
	}
	if e.haltReason[2] == "" {
		t.Error("a halted cell with no reason tells an operator nothing")
	}
}

// Batching means one commit failure is shared by everything committed with it,
// so every cell in a failed batch must halt. Getting this wrong in the other
// direction — assuming some rows landed — is the double spend the record exists
// to prevent.
func TestAFailedBatchHaltsEveryCellInIt(t *testing.T) {
	e := newTestEngine(t, 8)
	if err := e.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	var wg sync.WaitGroup
	for cell := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e.persist(history.CellTx{
				Generation: 4, Cell: cell, Status: history.StatusAttempting,
			}) {
				t.Errorf("cell %d: persist reported success against a closed store", cell)
			}
		}()
	}
	wg.Wait()

	e.mu.RLock()
	defer e.mu.RUnlock()
	for cell := range 4 {
		if !e.halted[cell] {
			t.Errorf("cell %d was not halted by the failed batch", cell)
		}
	}
}

// The point of the exercise: cells arriving together must share a round trip.
// A generation releases 128 cells at once, and if each still cost its own commit
// this whole path would be pointless.
func TestConcurrentPersistsShareACommit(t *testing.T) {
	const cells = 64
	e := newTestEngine(t, cells)
	if err := e.store.RecordGeneration(t.Context(), 20, "aa"); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for cell := range cells {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if !e.persist(history.CellTx{
				Generation: 20, Cell: cell, Status: history.StatusAttempting,
			}) {
				t.Errorf("cell %d: persist failed", cell)
			}
		}()
	}
	close(start)
	wg.Wait()

	rows := e.perf.persistRows.Value()
	batches := e.perf.persistBatches.Value()
	if rows != cells {
		t.Errorf("committed %d rows, want %d", rows, cells)
	}
	// Not asserting a precise ratio — that is a race against the linger, and a
	// slow machine legitimately produces more batches. Asserting the property
	// that matters: this is fewer round trips than rows.
	if batches >= cells {
		t.Errorf("%d rows took %d batches; they are not being grouped at all", rows, batches)
	}
	t.Logf("%d rows in %d commits (%.1f rows per round trip)", rows, batches, float64(rows)/float64(batches))
}

// A cancelled context must stop a cell worker, not make it spin.
//
// awaitTurn returns true the instant a cell's turn is ready, without reaching
// the select that observes cancellation — so runCell has to check the context
// itself. It did not, and while persist blocked on the database each turn of
// the loop was slow enough to hide it. Group-committing made the failure
// return instantly and one shutdown wrote 7.3 million log lines.
//
// Asserted as a bound on iterations rather than by watching the log, because
// the log line is not the bug; the loop is.
func TestACancelledWorkerStopsInsteadOfSpinning(t *testing.T) {
	e := newTestEngine(t, 4)
	atGeneration(e, 0)

	e.mu.Lock()
	e.mode = ModeRunning
	e.target = 1_000_000 // every cell's turn is ready, and stays ready
	e.mu.Unlock()

	// Cancelled before the worker ever starts: the loop must notice on its own.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	before := e.perf.advanceTotal.Count()
	done := make(chan struct{})
	go func() { defer close(done); e.runCell(ctx, 0) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runCell did not return on a cancelled context")
	}

	if spins := e.perf.advanceTotal.Count() - before; spins > 1 {
		t.Errorf("worker attempted %d transitions against a cancelled context, want at most 1", spins)
	}
}

// Shutting down must not leave a caller blocked for ever waiting for a reply
// from a committer that has stopped.
func TestPersistDoesNotBlockAfterTheCommitterStops(t *testing.T) {
	// A bare engine with NO committer, rather than newTestEngine with its
	// channel closed: commitTxs closes persistStopped itself on the way out, so
	// closing it underneath a running one is a double close, and the committer
	// would serve the request before persist noticed anyway.
	e := &Engine{
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		tips:           make([]chain.CellChain, 4),
		halted:         map[int]bool{},
		haltReason:     map[int]string{},
		changed:        make(chan struct{}),
		perf:           newPerf(),
		persistQueue:   make(chan persistRequest, persistQueueSize),
		persistStopped: make(chan struct{}),
	}
	close(e.persistStopped)

	done := make(chan bool, 1)
	go func() {
		done <- e.persist(history.CellTx{Generation: 1, Cell: 0, Status: history.StatusAttempting})
	}()

	select {
	case ok := <-done:
		if ok {
			t.Error("persist reported success with no committer running")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("persist blocked after the committer stopped")
	}

	// Deliberately NOT a halt: the cell is stopping anyway, and a reason
	// invented on the way out is the first thing an operator sees next start.
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.halted[0] {
		t.Error("shutting down must not halt a cell")
	}
}
