package engine

import (
	"testing"
	"time"

	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// The rejection messages the live deployment actually recorded. A cell must
// survive every one of them, repeated past every budget.
//
// PROCESSING (4) is the one that took the ring down: 3,962 of them in four
// minutes. TX_LOCKED and the ancestor cascade came with it. UTXO_SPENT and the
// doubled-prefix form are from earlier incidents and are kept because the
// classifier reads these strings, so their exact shape is load-bearing.
var liveRejections = []struct {
	name   string
	reason string
}{
	{"failed to validate", "arcade: REJECTED: PROCESSING (4): " +
		"[ProcessTransaction][8b1f0c] failed to validate transaction"},
	{"tx locked", "arcade: REJECTED: TX_LOCKED (37): [SPEND_BATCH_LUA][597398] " +
		"transaction is locked, blockHeight 15043: 48861617 - TX is locked and cannot be spent"},
	{"ancestor cascade", "arcade: REJECTED: parent rejected (ancestor 19f2221706f2): " +
		"retryable — resubmit after the ancestor is accepted"},
	{"utxo spent", "arcade: REJECTED: UTXO_SPENT (70): UTXO_SPENT (70): " +
		"abcdef:0 utxo already spent by tx 0123456789[0]"},
	{"empty reason", ""},
}

// recoverable reports whether a cell can still make progress on its own.
//
// This is the property the whole plan turns on, so it is stated once, here: a
// cell is recoverable if it is not halted. A halted cell is terminal — nothing
// in the running process retries it, and only `rule110 recover` clears it.
func recoverable(e *Engine, cell int) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return !e.halted[cell]
}

// TestARingWideRejectionBurstNeverHaltsTheRing is the headline property.
//
// On 2026-08-13 a burst of 3,962 rejections arrived over roughly four minutes
// and then stopped. The burst passed; the halts did not. 248 of 256 cells ended
// terminal, recoverable only by hand, for a condition that had already cleared.
//
// A burst that hits every cell at once is environmental — the network, arcade,
// or our own restart — not 256 independent cells each choosing to fail. Charging
// it against per-cell budgets converts a transient fault into a permanent one,
// which is the single worst thing this program can do.
func TestARingWideRejectionBurstNeverHaltsTheRing(t *testing.T) {
	f := newFixture(t)
	e := engineOn(t, f)

	tips := make([]chain.CellChain, testCells)
	refused := make([]chain.CellChain, testCells)
	for cell := range testCells {
		tips[cell] = f.advance(t, cell, f.genesisTip(cell), history.StatusMined)
		refused[cell] = f.build(t, cell, tips[cell], 3)
	}

	// The burst: every cell, refused more times than maxRetries allows and more
	// deeply than maxWreckage allows, inside one window.
	for range maxRetries + maxWreckage + 2 {
		for cell := range testCells {
			refuse(t, f, e, cell, refused[cell], aRefusal)
		}
		// Backdate so the refusals span minHaltWindow, as a real outage does.
		// Without this the test would pass on the time bound alone and prove
		// nothing about the burst.
		e.mu.Lock()
		for cell := range testCells {
			st := e.retries[cell]
			st.first = st.first.Add(-2 * minHaltWindow)
			e.retries[cell] = st
		}
		e.mu.Unlock()
	}

	var halted []int
	for cell := range testCells {
		if !recoverable(e, cell) {
			halted = append(halted, cell)
		}
	}
	if len(halted) > 0 {
		t.Errorf("%d of %d cells halted on a ring-wide burst: %v\n"+
			"A burst that hits every cell at once is environmental, not 256 separate "+
			"cell failures. Halting on it turns a transient fault into a permanent "+
			"one — which is exactly how 248 of 256 cells were lost to a condition "+
			"that had already cleared.", len(halted), testCells, halted)
	}
}

// TestNoRejectionSequenceCanPermanentlyHaltACell is the same property stated
// per-cell and per-message: whatever arcade says, and however often it says it,
// the cell must still be able to make progress once the refusals stop.
//
// A cell that needs an operator is a cell the automaton has lost. There is no
// rejection message that should cost one.
func TestNoRejectionSequenceCanPermanentlyHaltACell(t *testing.T) {
	for _, rej := range liveRejections {
		t.Run(rej.name, func(t *testing.T) {
			f := newFixture(t)
			e := engineOn(t, f)
			const cell = 2

			tip := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)
			refused := f.build(t, cell, tip, 7)

			// Well past every budget, and spanning the halt window.
			for range 4 * (maxRetries + maxWreckage) {
				refuse(t, f, e, cell, refused, rej.reason)
				e.mu.Lock()
				st := e.retries[cell]
				st.first = st.first.Add(-2 * minHaltWindow)
				e.retries[cell] = st
				e.mu.Unlock()
			}

			if !recoverable(e, cell) {
				e.mu.RLock()
				why := e.haltReason[cell]
				e.mu.RUnlock()
				t.Errorf("the cell is terminal after repeated %q refusals (reason %q).\n"+
					"No sequence of rejections may leave a cell that only an operator can "+
					"restart: the refusals stop, and the cell must not have stopped with them.",
					rej.name, why)
			}
		})
	}
}

// A cell released from a burst has to actually go again, not merely be
// un-halted. "Recoverable" that never recovers is the same outage with a
// friendlier metric.
func TestACellRecoversOnceTheRefusalsStop(t *testing.T) {
	f := newFixture(t)
	e := engineOn(t, f)
	const cell = 5

	tip := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)
	refused := f.build(t, cell, tip, 11)

	for range 3 * maxRetries {
		refuse(t, f, e, cell, refused, aRefusal)
		e.mu.Lock()
		st := e.retries[cell]
		st.first = st.first.Add(-2 * minHaltWindow)
		e.retries[cell] = st
		e.mu.Unlock()
	}

	// The cell must be BACKED OFF rather than halted — that is what makes the
	// rest of this test possible at all.
	e.mu.RLock()
	halted, stalled := e.halted[cell], !e.stalledUntil[cell].IsZero()
	e.mu.RUnlock()
	if halted {
		t.Fatal("the cell halted; there is nothing left to recover")
	}
	if !stalled {
		t.Fatal("the cell is neither halted nor backed off, so nothing paced the retries")
	}

	// The fault clears and the backoff elapses. Nothing external intervenes —
	// the wait is simulated rather than slept through, exactly as the refusals
	// above were backdated rather than spread over a real minute.
	e.mu.Lock()
	e.mode = ModeRunning
	e.target = tip.Generation + 50
	e.stalledUntil[cell] = time.Now().Add(-time.Second)
	delete(e.retries, cell)
	delete(e.needsRepair, cell)
	e.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if e.turnReady(cell) {
			return // the cell can advance again, which is the whole point
		}
		if time.Now().After(deadline) {
			e.mu.RLock()
			halted, repair := e.halted[cell], e.needsRepair[cell]
			e.mu.RUnlock()
			t.Fatalf("the cell never became ready again (halted=%v needsRepair=%v). "+
				"Backing off is only useful if it ends.", halted, repair)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An isolated failure must still be caught. Item 1.3 trades per-cell strictness
// for burst tolerance, and this is the test that stops the trade going too far:
// one genuinely broken cell, refusing alone while every other cell is healthy,
// is exactly the signal the budgets exist to detect.
func TestAnIsolatedRefusalIsStillAttributedToItsCell(t *testing.T) {
	f := newFixture(t)
	e := engineOn(t, f)
	const broken = 3

	tip := f.advance(t, broken, f.genesisTip(broken), history.StatusMined)
	refused := f.build(t, broken, tip, 13)

	for range maxRetries + 1 {
		refuse(t, f, e, broken, refused, aRefusal)
		e.mu.Lock()
		st := e.retries[broken]
		st.first = st.first.Add(-2 * minHaltWindow)
		e.retries[broken] = st
		e.mu.Unlock()
	}

	e.mu.RLock()
	attempts := e.retries[broken].attempts
	e.mu.RUnlock()
	if attempts == 0 {
		t.Error("a lone failing cell recorded no attempts at all; burst tolerance has " +
			"been widened until nothing is ever attributed to a cell, and a genuinely " +
			"broken chain would now fail silently for ever")
	}
}
