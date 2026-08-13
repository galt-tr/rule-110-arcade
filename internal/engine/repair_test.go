package engine

import (
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"

	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// refuse records a refusal of `refused` and tells the engine about it exactly as
// the arcade status stream would.
func refuse(t *testing.T, f *fixture, e *Engine, cell int, refused chain.CellChain, reason string) {
	t.Helper()
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Generation: refused.Generation, Cell: cell, TxID: refused.TxID,
		Status: history.StatusFailed, Err: reason,
	}); err != nil {
		t.Fatalf("record the refusal: %v", err)
	}
	e.mu.Lock()
	e.noteRefusalLocked(cell, refused.Generation, refused.TxID, string(arcade.StatusRejected), reason)
	e.mu.Unlock()
}

// TestARefusalSchedulesARebuildRatherThanHalting is the behaviour change. One
// refusal used to kill a cell until an operator ran `rule110 recover`, and since
// refusals are intermittent — roughly 2 per 16,000 transactions — that is
// monotonic erosion: the ring loses cells one at a time to a fault that usually
// goes away by itself.
func TestARefusalSchedulesARebuildRatherThanHalting(t *testing.T) {
	f := newFixture(t)
	e := engineOn(t, f)
	const cell = 6

	tip := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)
	refused := f.build(t, cell, tip, 3)
	refuse(t, f, e, cell, refused, aRefusal)

	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.halted[cell] {
		t.Error("a first refusal halted the cell instead of scheduling a rebuild")
	}
	if !e.needsRepair[cell] {
		t.Error("no rebuild was scheduled")
	}
	if got := e.retries[cell]; got.generation != refused.Generation || got.attempts != 1 {
		t.Errorf("retry state = %+v, want generation %d attempt 1", got, refused.Generation)
	}
}

// TestRepairRebuildsFromTheDerivedTip is the bug 9a guard, and it is the whole
// reason repairCell re-derives instead of reasoning from what it has in memory.
//
// A rejection leaves e.tips pointing at the REFUSED transaction's output — an
// output the network never produced. Rebuilding from there is what destroyed
// cells on the live deployment: the cell spent a phantom for ever, once per
// generation, each rejection looking like a fresh independent failure.
func TestRepairRebuildsFromTheDerivedTip(t *testing.T) {
	f := newFixture(t)
	e := engineOn(t, f)
	const cell = 6

	tip := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)
	refused := f.build(t, cell, tip, 3)

	// Exactly what recordCell does at broadcast: the tip moves to the new
	// transaction's output before there is any verdict on it.
	e.mu.Lock()
	e.tips[cell] = refused
	e.mu.Unlock()

	refuse(t, f, e, cell, refused, aRefusal)
	e.repairCell(t.Context(), cell)

	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.halted[cell] {
		t.Fatalf("cell stayed halted after the repair: %s", e.haltReason[cell])
	}
	if e.tips[cell].TxID == refused.TxID {
		t.Fatal("the repair left the tip on the REFUSED transaction's output — this is bug 9a")
	}
	if e.tips[cell].TxID != tip.TxID || e.tips[cell].Generation != tip.Generation {
		t.Errorf("tip = %s at generation %d, want the last transaction the network accepted, %s at %d",
			e.tips[cell].TxID, e.tips[cell].Generation, tip.TxID, tip.Generation)
	}
	if e.needsRepair[cell] {
		t.Error("the repair did not clear its own flag")
	}
}

// TestRepairClearsTheWholeDoomedRun covers what actually happened to cell 106 on
// the live deployment: generation 250 was already broadcast when 249's rejection
// landed, so two rows had to go.
//
// Both must clear in ONE repair. Each pass of the reviewed repair retracts a
// single row deliberately, so that each row is judged on its own evidence — but
// derivation stops offering a cell to recovery at all once maxWreckage rejections
// are stacked above its tip, so a repair that cleared one row per rejection would
// walk the cell into the cascade it is trying to prevent.
func TestRepairClearsTheWholeDoomedRun(t *testing.T) {
	f := newFixture(t)
	e := engineOn(t, f)
	const cell = 6

	tip := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)
	refused := f.build(t, cell, tip, 3)    // generation tip+1, spends the tip
	doomed := f.build(t, cell, refused, 4) // generation tip+2, spends the refused output

	for _, r := range []chain.CellChain{refused, doomed} {
		if err := f.store.RecordTx(t.Context(), history.CellTx{
			Generation: r.Generation, Cell: cell, TxID: r.TxID,
			Status: history.StatusFailed, Err: aRefusal,
		}); err != nil {
			t.Fatalf("record the refusal: %v", err)
		}
	}
	e.mu.Lock()
	e.tips[cell] = doomed
	e.noteRefusalLocked(cell, refused.Generation, refused.TxID, string(arcade.StatusRejected), aRefusal)
	e.mu.Unlock()

	e.repairCell(t.Context(), cell)

	e.mu.RLock()
	halted, haltReason, got := e.halted[cell], e.haltReason[cell], e.tips[cell]
	e.mu.RUnlock()
	if halted {
		t.Fatalf("cell stayed halted after the repair: %s", haltReason)
	}
	if got.TxID != tip.TxID {
		t.Errorf("tip = %s, want the last accepted transaction %s", got.TxID, tip.TxID)
	}

	// And the store agrees, which is what the next startup will read.
	again := f.derive(t)
	if again[cell].Halted {
		t.Fatalf("re-derivation still halts the cell: %s", again[cell].HaltReason)
	}
	if again[cell].Tip.TxID != tip.TxID {
		t.Errorf("re-derived tip = %s, want %s", again[cell].Tip.TxID, tip.TxID)
	}
}

// TestRepairStopsAfterMaxRetries bounds the rebuild. A transition that cannot be
// made to work must halt for a human rather than rebuild for ever.
func TestRepairStopsAfterMaxRetries(t *testing.T) {
	f := newFixture(t)
	e := engineOn(t, f)
	const cell = 6

	tip := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)
	refused := f.build(t, cell, tip, 3)

	for i := 1; i <= maxRetries; i++ {
		refuse(t, f, e, cell, refused, aRefusal)
		e.mu.RLock()
		halted := e.halted[cell]
		e.mu.RUnlock()
		if halted {
			t.Fatalf("cell halted on attempt %d of %d", i, maxRetries)
		}
	}

	// Attempts alone are no longer enough, and that is the point: rebuilds are
	// immediate, so three of them fit inside one bad second and say nothing
	// about the transition. The refusals must also have SPANNED minHaltWindow.
	// Backdate them, as a genuine outage would.
	refuse(t, f, e, cell, refused, aRefusal)
	e.mu.RLock()
	early := e.halted[cell]
	e.mu.RUnlock()
	if early {
		t.Fatal("cell halted on refusals that all landed in the same instant")
	}

	e.mu.Lock()
	st := e.retries[cell]
	st.first = st.first.Add(-2 * minHaltWindow)
	e.retries[cell] = st
	e.mu.Unlock()

	// Now a refusal of the SAME generation, past the window, is the one that halts.
	refuse(t, f, e, cell, refused, aRefusal)

	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.halted[cell] {
		t.Fatalf("cell rebuilt past %d consecutive refusals of generation %d",
			maxRetries, refused.Generation)
	}
	if e.needsRepair[cell] {
		t.Error("a halted cell still has a rebuild scheduled")
	}
	// The reason has to survive: this path used to set halted and leave
	// haltReason empty, so the UI reported a halted cell with nothing to say.
	if !strings.Contains(e.haltReason[cell], "REJECTED") {
		t.Errorf("halt reason = %q, want arcade's own words", e.haltReason[cell])
	}
}

// TestRetriesAreConsecutiveNotCumulative — a cell that meets one intermittent
// refusal, recovers, runs on and meets another much later has not failed twice at
// the same thing, and must not be halted for the sum of two unrelated faults.
func TestRetriesAreConsecutiveNotCumulative(t *testing.T) {
	f := newFixture(t)
	e := engineOn(t, f)
	const cell = 6

	e.mu.Lock()
	for i := 0; i < maxRetries; i++ {
		e.noteRefusalLocked(cell, 100, "tx-100", string(arcade.StatusRejected), aRefusal)
	}
	if e.halted[cell] {
		e.mu.Unlock()
		t.Fatal("halted before the bound was reached")
	}
	// The cell advances past the generation the count was about.
	e.clearRetriesLocked(cell, 101)
	if _, still := e.retries[cell]; still {
		e.mu.Unlock()
		t.Fatal("advancing past the refused generation left the count in place")
	}

	// A later, unrelated refusal starts from one again.
	e.noteRefusalLocked(cell, 200, "tx-200", string(arcade.StatusRejected), aRefusal)
	halted, st := e.halted[cell], e.retries[cell]
	e.mu.Unlock()

	if halted {
		t.Error("a later unrelated refusal halted the cell on its first attempt")
	}
	if st.generation != 200 || st.attempts != 1 {
		t.Errorf("retry state = %+v, want generation 200 attempt 1", st)
	}
}

// TestRepairDoesNotBlindlyRetryASpentTip. A UTXO_SPENT refusal is not an
// unexplained one: arcade has named the transaction that spent the tip. That
// routes to RecoverSpentTip, which verifies the named candidate from its own
// bytes — rebuilding the generation instead would re-spend an output somebody
// else already spent.
func TestRepairDoesNotBlindlyRetryASpentTip(t *testing.T) {
	f := newFixture(t)
	e := engineOn(t, f)
	const cell = 6

	tip := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)
	refused := f.build(t, cell, tip, 3)

	// A spend the ledger cannot produce bytes for, so RecoverSpentTip can verify
	// nothing and must halt rather than guess.
	spentErr := "arcade: REJECTED: UTXO_SPENT (70): " + tip.TxID +
		":0 utxo already spent by tx " + strings.Repeat("ab", 32) + "[0]"
	refuse(t, f, e, cell, refused, spentErr)

	e.repairCell(t.Context(), cell)

	e.mu.RLock()
	defer e.mu.RUnlock()
	if !e.halted[cell] {
		t.Fatal("a UTXO_SPENT refusal was rebuilt blindly; the tip may already be spent")
	}
	if e.tips[cell].TxID == refused.TxID {
		t.Error("the halt left the tip on the refused transaction's output")
	}
}
