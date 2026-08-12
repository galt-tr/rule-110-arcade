package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// recoveringLedger is countingLedger with wallet actions, so the whole path from
// "the store says this tip is unknown" to "the store now says something else"
// can run offline.
type recoveringLedger struct {
	countingLedger
	actions map[int][]chain.CellAction
}

func (l *recoveringLedger) CellActions(_ context.Context, cell, _ int) ([]chain.CellAction, int, error) {
	return l.actions[cell], len(l.actions[cell]), nil
}

// unknownTip leaves cell 4 with an unresolved write-ahead record on top of one
// real transition, which is the shape a crash between signing and recording
// leaves behind.
func unknownTip(t *testing.T, f *fixture) (*recoveringLedger, chain.CellChain) {
	t.Helper()
	good := f.advance(t, 4, f.genesisTip(4), history.StatusBroadcast)
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Generation: good.Generation + 1, Cell: 4, Status: history.StatusAttempting,
	}); err != nil {
		t.Fatalf("record attempt: %v", err)
	}
	l := &recoveringLedger{actions: map[int][]chain.CellAction{}}
	l.raw = f.ledger.raw
	f.ledger = &l.countingLedger
	return l, good
}

// TestRecoverAdoptsIntoTheStore: the decision is only useful if it lands
// somewhere durable. An adopted transition must appear in the store as a real
// broadcast record, because the store is what the next startup derives from —
// and it must be written BEFORE the cell is released, so a crash mid-recovery
// leaves the store ahead of the cell rather than behind it.
func TestRecoverAdoptsIntoTheStore(t *testing.T) {
	f := newFixture(t)
	l, good := unknownTip(t, f)

	// The lost transition really was signed: build it and give the wallet an
	// action for it.
	row, err := good.Row(testCells)
	if err != nil {
		t.Fatalf("row: %v", err)
	}
	next := ca.Rule110.Step(row)
	lost := f.advance(t, 4, good, history.StatusBroadcast)
	// advance() also recorded it; undo that so the store is genuinely ignorant,
	// which is the state recovery has to fix.
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Generation: lost.Generation, Cell: 4, Status: history.StatusAttempting,
	}); err != nil {
		t.Fatalf("reset record: %v", err)
	}
	lock, err := f.compiled.LockingScript(4, next)
	if err != nil {
		t.Fatalf("locking script: %v", err)
	}
	l.actions[4] = []chain.CellAction{{
		TxID: lost.TxID, Status: "unproven", Generation: lost.Generation,
		HasGeneration: true, LockingScript: lock, Satoshis: good.Satoshis,
	}}

	positions := f.derive(t)
	if !positions[4].Unknown {
		t.Fatalf("cell 4 = %+v, want an unknown tip before recovery", positions[4])
	}

	out, decisions, err := Recover(t.Context(), l, f.compiled, f.facts, f.store, positions, true)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Verdict != chain.VerdictAdopt {
		t.Fatalf("decisions = %+v, want a single adopt", decisions)
	}
	if out[4].Halted || out[4].Tip.TxID != lost.TxID {
		t.Errorf("cell 4 = %+v, want released onto the adopted transaction %s", out[4], lost.TxID)
	}

	// And it survives a restart, which is the only thing that matters.
	again := f.derive(t)
	if again[4].Halted {
		t.Fatalf("cell 4 came back halted after recovery: %s", again[4].HaltReason)
	}
	if again[4].Tip.TxID != lost.TxID {
		t.Errorf("re-derived tip = %s, want the adopted %s", again[4].Tip.TxID, lost.TxID)
	}
}

// A cell whose attempt was never signed resumes, and the write-ahead record is
// retracted — otherwise it would halt the cell again at every startup for ever.
func TestRecoverRetractsAnUnsignedAttempt(t *testing.T) {
	f := newFixture(t)
	l, good := unknownTip(t, f)
	l.actions[4] = []chain.CellAction{{
		Status: "unsigned", Generation: good.Generation + 1, HasGeneration: true,
		Satoshis: good.Satoshis,
	}}

	positions := f.derive(t)
	out, decisions, err := Recover(t.Context(), l, f.compiled, f.facts, f.store, positions, true)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Verdict != chain.VerdictResume {
		t.Fatalf("decisions = %+v, want a single resume", decisions)
	}
	if out[4].Halted || out[4].Tip.TxID != good.TxID {
		t.Errorf("cell 4 = %+v, want resumed on %s", out[4], good.TxID)
	}

	again := f.derive(t)
	if again[4].Halted || again[4].Unknown {
		t.Fatalf("the write-ahead record was not retracted; the cell halts again: %s", again[4].HaltReason)
	}
}

// A dry run must change nothing. It is the whole reason recovery ships as a
// decision rather than an action: an operator reads it against the real wallet
// before anything is allowed to act on it.
func TestRecoverDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	l, good := unknownTip(t, f)
	l.actions[4] = []chain.CellAction{{
		Status: "unsigned", Generation: good.Generation + 1, HasGeneration: true,
		Satoshis: good.Satoshis,
	}}

	positions := f.derive(t)
	if _, decisions, err := Recover(t.Context(), l, f.compiled, f.facts, f.store, positions, false); err != nil {
		t.Fatalf("Recover: %v", err)
	} else if len(decisions) != 1 {
		t.Fatalf("decisions = %+v, want one", decisions)
	}

	again := f.derive(t)
	if !again[4].Unknown {
		t.Error("a dry run resolved the cell; it must only report what it would do")
	}
}

// A cell halted by a REJECTION is never a recovery candidate. The rejected
// transaction's output does not exist, so there is nothing to adopt, and the
// output it tried to spend is gone, so there is nothing to resume from either.
func TestRecoverIgnoresARejectedCell(t *testing.T) {
	f := newFixture(t)
	good := f.advance(t, 6, f.genesisTip(6), history.StatusBroadcast)
	rejected := f.advance(t, 6, good, history.StatusBroadcast)
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Generation: rejected.Generation, Cell: 6, TxID: rejected.TxID,
		Status: history.StatusFailed, Err: "arcade: REJECTED",
	}); err != nil {
		t.Fatalf("record rejection: %v", err)
	}

	positions := f.derive(t)
	l := &recoveringLedger{actions: map[int][]chain.CellAction{}}
	l.raw = f.ledger.raw
	l.rawErr = errors.New("must not be consulted")

	_, decisions, err := Recover(t.Context(), l, f.compiled, f.facts, f.store, positions, true)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 0 {
		t.Fatalf("recovery considered a rejected cell: %+v", decisions)
	}
	if !positions[6].Halted {
		t.Error("the rejected cell must stay halted")
	}
}
