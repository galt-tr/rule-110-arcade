package engine

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

// fillHistory gives the engine n generations of live-looking cells, so the
// snapshot copy has real work to do.
func fillHistory(e *Engine, n, cells int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.history = make([]Generation, 0, n)
	for g := range n {
		gen := Generation{Number: uint64(g), Cells: make([]CellTx, cells)}
		for c := range gen.Cells {
			gen.Cells[c] = CellTx{Cell: c, TxID: "0123456789abcdef0123456789abcdef", State: TxSeen}
		}
		e.history = append(e.history, gen)
	}
}

// TestSnapshotTailCopiesOnlyTheTail is the regression guard for the cost of a
// push. SnapshotTail used to call Snapshot and slice the result, so emitting 48
// generations copied all of maxHistory — at 2048 x 128 that is a quarter of a
// million CellTx structs, under the read lock, per push per client.
//
// Allocation count is the honest measure here: the old code's cost is invisible
// in the returned value, which was correctly sized either way.
func TestSnapshotTailCopiesOnlyTheTail(t *testing.T) {
	const (
		cells = 128
		gens  = 2048
	)
	e := newTestEngineQueued(t, cells)
	fillHistory(e, gens, cells)

	tailAllocs := testing.AllocsPerRun(5, func() { _ = e.SnapshotTail() })
	fullAllocs := testing.AllocsPerRun(5, func() { _ = e.Snapshot() })

	// The tail is 48 of 2048 generations. Allowing a generous 4x margin over the
	// ideal ratio still fails decisively against the old implementation, where
	// the two were identical.
	limit := fullAllocs * float64(TailGenerations) / float64(gens) * 4
	if tailAllocs > limit {
		t.Errorf("SnapshotTail made %.0f allocations against Snapshot's %.0f; "+
			"want under %.0f — it is copying the whole history to emit %d generations",
			tailAllocs, fullAllocs, limit, TailGenerations)
	}

	// And it still returns what it should.
	s := e.SnapshotTail()
	if len(s.History) != TailGenerations {
		t.Errorf("tail carried %d generations, want %d", len(s.History), TailGenerations)
	}
	if s.History[len(s.History)-1].Number != gens-1 {
		t.Errorf("tail ends at generation %d, want %d", s.History[len(s.History)-1].Number, gens-1)
	}
}

// TestStatsCarriesNoHistory covers /metrics and /readyz, which are scraped on a
// timer whether or not anyone is watching the diagram.
func TestStatsCarriesNoHistory(t *testing.T) {
	const cells = 32
	e := newTestEngineQueued(t, cells)
	fillHistory(e, 512, cells)

	s := e.Stats()
	if len(s.History) != 0 {
		t.Errorf("Stats carried %d generations, want none", len(s.History))
	}
	// The scalars still have to be right — they are what /metrics reports.
	if s.Cells != cells {
		t.Errorf("Stats.Cells = %d, want %d", s.Cells, cells)
	}
	if s.ProvedCells != cells {
		t.Errorf("Stats.ProvedCells = %d, want %d — counted off the newest row, "+
			"which Stats does not copy", s.ProvedCells, cells)
	}
}

// TestPublishTailIsMarshalledOnce pins the shape the SSE handler depends on: the
// published value is bytes, so N subscribers write the same slice instead of
// each rebuilding and re-marshalling the same state.
func TestPublishTailIsMarshalledOnce(t *testing.T) {
	e := newTestEngineQueued(t, 8)
	fillHistory(e, 4, 8)

	if _, ok := e.PublishedTail(); ok {
		t.Fatal("nothing should be published before the first publish")
	}
	if !e.PublishTail() {
		t.Fatal("PublishTail failed")
	}

	first, ok := e.PublishedTail()
	if !ok {
		t.Fatal("nothing published after PublishTail")
	}
	second, _ := e.PublishedTail()
	if first != second {
		t.Error("each reader got its own copy; the marshal is supposed to be shared")
	}

	var snap Snapshot
	if err := json.Unmarshal(*first, &snap); err != nil {
		t.Fatalf("published bytes are not a snapshot: %v", err)
	}
	if len(snap.History) != 4 {
		t.Errorf("published tail carried %d generations, want 4", len(snap.History))
	}
}

// TestPublishTailsPublishesIsolatedChangesImmediately is the leading-edge
// requirement. The old handler waited for a change and THEN slept the coalescing
// interval before sending, which taxed every update — including an isolated one
// with no burst to coalesce — with a fixed delay.
func TestPublishTailsPublishesIsolatedChangesImmediately(t *testing.T) {
	e := newTestEngineQueued(t, 4, 1)

	const interval = 300 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.PublishTails(ctx, interval)

	// Wait for the initial publish.
	waitFor(t, func() bool { _, ok := e.PublishedTail(); return ok }, 2*time.Second,
		"nothing was published at startup")

	// Let the throttle window pass, so this change is genuinely isolated.
	time.Sleep(interval + 50*time.Millisecond)

	e.mu.Lock()
	e.history[0].Cells[2] = CellTx{Cell: 2, TxID: "isolated", State: TxMined}
	e.notify()
	e.mu.Unlock()

	// It must appear well inside the coalescing interval, which it cannot if the
	// interval is being applied as a delay before publishing.
	start := time.Now()
	waitFor(t, func() bool {
		data, ok := e.PublishedTail()
		if !ok {
			return false
		}
		var snap Snapshot
		if json.Unmarshal(*data, &snap) != nil || len(snap.History) == 0 {
			return false
		}
		return snap.History[0].Cells[2].TxID == "isolated"
	}, 2*time.Second, "an isolated change was never published")

	if elapsed := time.Since(start); elapsed > interval {
		t.Errorf("an isolated change took %v to publish, longer than the %v coalescing "+
			"interval — the interval is being applied as a delay, not a floor", elapsed, interval)
	}
}

// TestPublishTailsCoalescesBursts is the other side of the same knob: a
// generation of many cells settling must not become one publish per cell.
func TestPublishTailsCoalescesBursts(t *testing.T) {
	e := newTestEngineQueued(t, 64, 1)

	const interval = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.PublishTails(ctx, interval)

	waitFor(t, func() bool { _, ok := e.PublishedTail(); return ok }, 2*time.Second,
		"nothing was published at startup")

	// 64 notifications back to back, as a settling generation produces. The
	// watcher counts DISTINCT published slices: a fresh publish is a fresh
	// allocation, so identity is the cheap way to spot one.
	var publishes atomic.Int64
	stop := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		var prev *[]byte
		for {
			select {
			case <-stop:
				return
			default:
			}
			if data, ok := e.PublishedTail(); ok && data != prev {
				publishes.Add(1)
				prev = data
			}
			time.Sleep(time.Millisecond)
		}
	}()

	for c := range 64 {
		e.mu.Lock()
		e.history[0].Cells[c] = CellTx{Cell: c, TxID: "burst", State: TxSeen}
		e.notify()
		e.mu.Unlock()
	}
	time.Sleep(2 * interval)
	close(stop)
	<-watcherDone

	// Far fewer than 64. The exact count depends on scheduling; the point is
	// that the burst is coalesced rather than amplified.
	if got := publishes.Load(); got > 8 {
		t.Errorf("a 64-cell burst produced %d publishes, want it coalesced", got)
	}
}

func waitFor(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}
