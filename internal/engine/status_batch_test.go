package engine

import (
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"

	"github.com/dymurray/rule-110-arcade/internal/history"
)

// lockProbe answers "was the engine lock held at this moment?". Keeping the lock
// off the persistence path is the whole point of the batched apply: holding the global lock
// across a Postgres round trip serialised all 128 cell workers, every snapshot
// reader and the clock behind the network, once per status event.
//
// "Is the lock held?" is not directly observable on a sync.RWMutex, so the probe
// asks the question the other way round: TryLock succeeds only if nobody holds
// it, for reading or writing.
type lockProbe struct {
	e      *Engine
	mu     sync.Mutex
	locked bool
}

func (p *lockProbe) check() {
	if p.e.mu.TryLock() {
		p.e.mu.Unlock()
		return
	}
	p.mu.Lock()
	p.locked = true
	p.mu.Unlock()
}

func (p *lockProbe) tripped() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.locked
}

// TestApplyStatusBatchDoesNotWriteUnderTheLock is the regression guard for the
// engine-wide stall. Against the previous code — applyStatus taking e.mu.Lock()
// and calling store.UpdateStatus inside it — this fails.
func TestApplyStatusBatchDoesNotWriteUnderTheLock(t *testing.T) {
	e := newTestEngineQueued(t, 8, 1)
	probe := &lockProbe{e: e}

	// Drain the queue ourselves, checking the lock at the moment the write would
	// have happened.
	const txid = "cafe"
	e.mu.Lock()
	e.history[0].Cells[3] = CellTx{Cell: 3, TxID: txid, State: TxBroadcast}
	e.indexTx(txid, 1, 3, time.Now())
	e.mu.Unlock()

	e.applyStatusBatch([]arcade.TxRecord{{TxID: txid, Status: arcade.StatusMined}})

	// The write is queued, not done: by the time anything reaches the store the
	// apply has long since released the lock.
	select {
	case u := <-e.statusWrites:
		probe.check()
		if u.TxID != txid || u.Status != history.StatusMined {
			t.Fatalf("queued write = %+v, want mined status for %s", u, txid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no status write was queued")
	}

	if probe.tripped() {
		t.Error("the engine lock was still held when the status write became available")
	}
}

// TestApplyStatusBatchAppliesTheWholeBatchAtomically covers what a subscriber
// sees: a settling generation lands as one observable change carrying every
// cell, not a sequence of partial states.
//
// A characterization test, not a regression guard, and worth being explicit
// about why. The obvious guard — count notifications and expect 1 — cannot fail
// against per-event notification: notify() is called with the write lock held,
// and a subscriber needs the read lock to observe the channel, so it is parked
// for the whole apply either way and sees the last state regardless. The
// per-batch lock discipline is pinned by the two tests either side of this one.
func TestApplyStatusBatchAppliesTheWholeBatchAtomically(t *testing.T) {
	const cells = 8
	e := newTestEngine(t, cells, 1)

	recs := make([]arcade.TxRecord, 0, cells)
	e.mu.Lock()
	for c := range cells {
		txid := "tx" + strconv.Itoa(c)
		e.history[0].Cells[c] = CellTx{Cell: c, TxID: txid, State: TxBroadcast}
		e.indexTx(txid, 1, c, time.Now())
		recs = append(recs, arcade.TxRecord{TxID: txid, Status: arcade.StatusSeenOnNetwork})
	}
	e.mu.Unlock()

	// Count notifications by watching the channel get replaced.
	notifications := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			ch := e.Changed()
			select {
			case <-ch:
				notifications++
			case <-time.After(500 * time.Millisecond):
				return
			}
		}
	}()

	time.Sleep(20 * time.Millisecond) // let the watcher subscribe
	e.applyStatusBatch(recs)
	<-done

	if notifications != 1 {
		t.Errorf("applying a batch of %d was observed as %d changes, want 1", cells, notifications)
	}

	e.mu.RLock()
	defer e.mu.RUnlock()
	for c := range cells {
		if got := e.history[0].Cells[c].State; got != TxSeen {
			t.Errorf("cell %d state = %q, want %q", c, got, TxSeen)
		}
	}
}

// TestApplyStatusBatchIgnoresForeignTxidsWithoutTheWriteLock covers the traffic
// the stream is mostly made of. With FullStatusUpdates the feed carries every
// status of every transaction on the token, and a record we do not own used to
// take the engine's global WRITE lock before being discarded.
//
// A held READ lock is the discriminator: it is compatible with the ownership
// filter (which reads) and incompatible with the apply (which writes). So a
// foreign-only batch completes against it and an owned batch does not.
func TestApplyStatusBatchIgnoresForeignTxidsWithoutTheWriteLock(t *testing.T) {
	e := newTestEngineQueued(t, 4, 1)

	const ours = "ours"
	e.mu.Lock()
	e.history[0].Cells[1] = CellTx{Cell: 1, TxID: ours, State: TxBroadcast}
	e.indexTx(ours, 1, 1, time.Now())
	e.mu.Unlock()

	apply := func(recs []arcade.TxRecord) chan struct{} {
		done := make(chan struct{})
		go func() { defer close(done); e.applyStatusBatch(recs) }()
		return done
	}

	e.mu.RLock()

	foreign := apply([]arcade.TxRecord{
		{TxID: "not-ours-1", Status: arcade.StatusMined},
		{TxID: "not-ours-2", Status: arcade.StatusRejected},
	})
	select {
	case <-foreign:
	case <-time.After(2 * time.Second):
		e.mu.RUnlock()
		t.Fatal("a batch of foreign txids escalated to the engine write lock")
	}

	// The control: a batch we DO own must wait for the write lock, which proves
	// the check above distinguishes the two paths rather than passing trivially.
	owned := apply([]arcade.TxRecord{{TxID: ours, Status: arcade.StatusMined}})
	select {
	case <-owned:
		e.mu.RUnlock()
		t.Fatal("an owned batch applied without taking the write lock")
	case <-time.After(100 * time.Millisecond):
	}

	e.mu.RUnlock()
	select {
	case <-owned:
	case <-time.After(2 * time.Second):
		t.Fatal("the owned batch never completed after the read lock was released")
	}

	select {
	case u := <-e.statusWrites:
		if u.TxID != ours {
			t.Errorf("a foreign txid produced a status write: %+v", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the owned batch queued no write")
	}
}

// TestStatusWriterBatchesAndCollapses proves the writer coalesces: many queued
// updates become few store round trips, and a txid updated twice in one batch
// keeps the LAST value rather than colliding in the VALUES join.
func TestStatusWriterBatchesAndCollapses(t *testing.T) {
	e := newTestEngine(t, 4, 1)

	if err := e.store.RecordGeneration(t.Context(), 1, ""); err != nil {
		t.Fatalf("record generation: %v", err)
	}
	for c := range 2 {
		if err := e.store.RecordTx(t.Context(), history.CellTx{
			Generation: 1, Cell: c, TxID: "t" + strconv.Itoa(c), Status: history.StatusBroadcast,
		}); err != nil {
			t.Fatalf("record tx: %v", err)
		}
	}

	// t0 is written twice in quick succession; mined must win.
	e.enqueueStatusWrites([]history.StatusUpdate{
		{TxID: "t0", Status: history.StatusSeen},
		{TxID: "t1", Status: history.StatusSeen},
		{TxID: "t0", Status: history.StatusMined},
	})

	waitForPersistedStatus(t, e, "t0", history.StatusMined)
	waitForPersistedStatus(t, e, "t1", history.StatusSeen)
}

// TestEnqueueStatusWritesNeverBlocks is the contract that keeps the SSE reader
// alive. The enqueue runs on the monitor's applier goroutine; blocking it stalls
// the applier, the hand-off queue fills, the reader stops draining its socket,
// and arcade drops events for a slow client.
func TestEnqueueStatusWritesNeverBlocks(t *testing.T) {
	// A bare engine with a deliberately tiny, already-full queue and no writer
	// draining it. Built here rather than via newTestEngine so that swapping the
	// queue is not a race against a running writer.
	e := &Engine{
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		statusWrites: make(chan history.StatusUpdate, 2),
		perf:         newPerf(),
	}
	for range 2 {
		e.statusWrites <- history.StatusUpdate{TxID: "filler"}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.enqueueStatusWrites([]history.StatusUpdate{
			{TxID: "overflow-1", Status: history.StatusMined},
			{TxID: "overflow-2", Status: history.StatusMined},
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue blocked on a full queue; this stalls the arcade SSE reader")
	}
}
