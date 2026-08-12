package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// mined is an arcade that reports everything it is asked about as mined, and
// everything it is not asked about as unknown.
type mined map[string]bool

func (m mined) GetTx(_ context.Context, txid string) (*arcade.TxRecord, error) {
	if !m[txid] {
		return nil, arcade.ErrTxNotFound
	}
	return &arcade.TxRecord{TxID: txid, Status: arcade.StatusMined}, nil
}

// noArcade is the oracle for tests where no candidate should ever be looked up.
var noArcade = mined{}

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

	out, decisions, err := Recover(t.Context(), l, noArcade, f.compiled, f.facts, f.store, positions, true)
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
	out, decisions, err := Recover(t.Context(), l, noArcade, f.compiled, f.facts, f.store, positions, true)
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
	if _, decisions, err := Recover(t.Context(), l, noArcade, f.compiled, f.facts, f.store, positions, false); err != nil {
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

	_, decisions, err := Recover(t.Context(), l, noArcade, f.compiled, f.facts, f.store, positions, true)
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

// spentTip puts one cell into the damage class the UTXO_SPENT path repairs, as
// the live deployment recorded it.
//
// Cell 1 advances once and that is the last thing our database knows. A second
// transition was signed, broadcast and MINED, but both of its records failed to
// persist, so nothing of ours mentions it. After the restart the cell re-derived
// the older tip, re-spent it, and arcade refused — and the refusal, which is the
// only record of the episode we have, names the transaction that got there
// first.
//
// It returns the tip the record believes in, the transition that really holds
// the cell, and the rejection message as arcade wrote it.
func spentTip(t *testing.T, f *fixture, cell int) (chain.CellChain, chain.CellChain, string) {
	t.Helper()
	good := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)
	lost := f.advance(t, cell, good, history.StatusMined)

	// The record of the lost transition is replaced by the record its re-spend
	// left behind: cell_txs is keyed by (generation, cell), so the rejection is
	// literally what erased it.
	rejection := fmt.Sprintf(
		"arcade: REJECTED: UTXO_SPENT (70): UTXO_SPENT (70): %s:%d utxo already spent by tx %s",
		good.TxID, good.Vout, lost.TxID)
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Generation: lost.Generation, Cell: cell, TxID: strings.Repeat("cc", 32),
		Status: history.StatusFailed, Err: rejection,
	}); err != nil {
		t.Fatalf("record the rejection: %v", err)
	}
	return good, lost, rejection
}

// TestRecoverAdoptsATipSpentByOurOwnTransaction is the whole point of the
// UTXO_SPENT path, end to end and through a restart.
//
// Before it existed this cell was dead for good: `rule110 recover` only looked
// at cells with an unresolved write-ahead record, and this one has no record of
// the lost transition at all — the bug that produced it (fixed in 081bd2a) is
// precisely that both records failed to persist. Every startup re-derived the
// stale tip, re-spent it, and collected another rejection.
func TestRecoverAdoptsATipSpentByOurOwnTransaction(t *testing.T) {
	f := newFixture(t)
	good, lost, _ := spentTip(t, f, 1)

	positions := f.derive(t)
	p := positions[1]
	if !p.Halted || p.Unknown {
		t.Fatalf("cell 1 = %+v, want halted by a rejection rather than flagged unknown", p)
	}
	if p.Tip.TxID != good.TxID {
		t.Fatalf("derived tip = %s, want the last record we have (%s)", p.Tip.TxID, good.TxID)
	}
	if !p.Rejected || !chain.IsUTXOSpent(p.RejectionErr) {
		t.Fatalf("cell 1 was not offered as a UTXO_SPENT candidate: %+v", p)
	}

	out, decisions, err := Recover(t.Context(), f.ledger, mined{lost.TxID: true}, f.compiled,
		f.facts, f.store, positions, true)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Verdict != chain.VerdictAdopt {
		t.Fatalf("decisions = %+v, want a single adopt", decisions)
	}
	if out[1].Halted || out[1].Tip.TxID != lost.TxID {
		t.Errorf("cell 1 = %+v, want released onto %s", out[1], lost.TxID)
	}

	// What the operator actually reads. The verdict vocabulary is the same one
	// the unresolved-attempt path uses, and the adopted step is spelled out with
	// its outpoint so a dry run can be checked against the chain by hand before
	// anyone reaches for -apply.
	line := FormatRecovery(decisions[0])
	for _, want := range []string{"adopt", lost.TxID, fmt.Sprintf("generation %d", lost.Generation)} {
		if !strings.Contains(line, want) {
			t.Errorf("the report does not mention %q:\n%s", want, line)
		}
	}

	// Surviving the restart is the only thing that matters: the store is what the
	// next startup derives from, and the rejection that halted this cell has to be
	// gone from it — replaced by the transaction that really holds the generation.
	again := f.derive(t)
	if again[1].Halted {
		t.Fatalf("cell 1 came back halted after recovery: %s", again[1].HaltReason)
	}
	if again[1].Tip.TxID != lost.TxID || again[1].Tip.Generation != lost.Generation {
		t.Errorf("re-derived tip = %s at generation %d, want the adopted %s at %d",
			again[1].Tip.TxID, again[1].Tip.Generation, lost.TxID, lost.Generation)
	}
}

// A dry run over the UTXO_SPENT path must change nothing, exactly like the dry
// run over the unresolved-attempt path. This is the flag an operator uses to
// read a decision about 128 live UTXO chains before allowing it to act.
func TestRecoverSpentTipDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	_, lost, _ := spentTip(t, f, 1)

	positions := f.derive(t)
	_, decisions, err := Recover(t.Context(), f.ledger, mined{lost.TxID: true}, f.compiled,
		f.facts, f.store, positions, false)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Verdict != chain.VerdictAdopt {
		t.Fatalf("decisions = %+v, want a single adopt to be REPORTED", decisions)
	}

	again := f.derive(t)
	if !again[1].Halted || again[1].Tip.TxID == lost.TxID {
		t.Error("a dry run moved the tip; it must only report what it would do")
	}
}

// TestRecoverWillNotAdoptAForeignSpend is the case that must stay broken.
//
// A UTXO_SPENT naming a transaction that is not ours means somebody else spent
// this cell's output — a genuine double spend, and the end of that chain.
// Resuming from it would point the cell at an outpoint we cannot spend, and
// would do it unattended for however many cells were affected. Halting costs an
// operator's attention; the other direction costs the cell.
func TestRecoverWillNotAdoptAForeignSpend(t *testing.T) {
	f := newFixture(t)
	good := f.advance(t, 1, f.genesisTip(1), history.StatusMined)

	foreign := strings.Repeat("ee", 32)
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Generation: good.Generation + 1, Cell: 1, TxID: strings.Repeat("cc", 32),
		Status: history.StatusFailed,
		Err: fmt.Sprintf("arcade: REJECTED: UTXO_SPENT (70): UTXO_SPENT (70): %s:%d "+
			"utxo already spent by tx %s", good.TxID, good.Vout, foreign),
	}); err != nil {
		t.Fatalf("record the rejection: %v", err)
	}

	positions := f.derive(t)
	out, decisions, err := Recover(t.Context(), f.ledger, mined{foreign: true}, f.compiled,
		f.facts, f.store, positions, true)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// It is EXAMINED — an operator must find the cell they came to look at in the
	// report — and the verdict is halt: nobody can produce the bytes of this
	// transaction, so nothing about it has been established.
	if len(decisions) != 1 || decisions[0].Verdict != chain.VerdictHalt {
		t.Fatalf("decisions = %+v, want a single halt", decisions)
	}
	if !out[1].Halted || out[1].Tip.TxID != good.TxID {
		t.Errorf("cell 1 = %+v, want left exactly where it was", out[1])
	}
	if again := f.derive(t); again[1].Tip.TxID != good.TxID {
		t.Errorf("the tip moved to %s on a halt", again[1].Tip.TxID)
	}
}

// TestRecoverWillNotAdoptATransactionWeAlreadyHold exercises condition 2
// through the real store rather than a stub.
//
// A UTXO_SPENT that names a transaction already in cell_txs is not a lost
// transition. It is our own bookkeeping contradicting itself — here, arcade
// blaming the cell's own tip for spending itself — and there is no version of
// that which is fixed by moving a live tip. The rejection is otherwise perfectly
// well formed, so nothing but the record lookup distinguishes it.
func TestRecoverWillNotAdoptATransactionWeAlreadyHold(t *testing.T) {
	f := newFixture(t)
	good := f.advance(t, 1, f.genesisTip(1), history.StatusMined)

	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Generation: good.Generation + 1, Cell: 1, TxID: strings.Repeat("cc", 32),
		Status: history.StatusFailed,
		Err: fmt.Sprintf("arcade: REJECTED: UTXO_SPENT (70): UTXO_SPENT (70): %s:%d "+
			"utxo already spent by tx %s", good.TxID, good.Vout, good.TxID),
	}); err != nil {
		t.Fatalf("record the rejection: %v", err)
	}

	positions := f.derive(t)
	out, decisions, err := Recover(t.Context(), f.ledger, mined{good.TxID: true}, f.compiled,
		f.facts, f.store, positions, true)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Verdict != chain.VerdictHalt {
		t.Fatalf("decisions = %+v, want a single halt", decisions)
	}
	if !strings.Contains(decisions[0].Reason, "IS on our record") {
		t.Errorf("the reason should say we already hold the named transaction, got: %s",
			decisions[0].Reason)
	}
	if !out[1].Halted || out[1].Tip.TxID != good.TxID {
		t.Errorf("cell 1 = %+v, want left exactly where it was", out[1])
	}
}

// A rejection higher up than the tip's own successor is a CASCADE, not a lost
// transition: the cell built on a phantom output and every attempt after it was
// refused in turn, so the newest reason describes wreckage. Cells 34, 51 and 91
// of the live deployment are in exactly this state, under about 170 consecutive
// failures each, and none of those messages points at a recoverable tip.
func TestRecoverIgnoresACascadeRejection(t *testing.T) {
	f := newFixture(t)
	good := f.advance(t, 1, f.genesisTip(1), history.StatusMined)

	for gen := good.Generation + 1; gen <= good.Generation+tipDepth+2; gen++ {
		if err := f.store.RecordTx(t.Context(), history.CellTx{
			Generation: gen, Cell: 1, TxID: fmt.Sprintf("%058x%06x", 0, gen),
			Status: history.StatusFailed,
			Err: "arcade: REJECTED: UTXO_SPENT (70): UTXO_SPENT (70): utxo already spent by tx " +
				strings.Repeat("ee", 32),
		}); err != nil {
			t.Fatalf("record rejection at %d: %v", gen, err)
		}
	}

	positions := f.derive(t)
	if positions[1].Rejected {
		t.Errorf("a cascade was offered to recovery as a lost transition: %q", positions[1].RejectionErr)
	}
	_, decisions, err := Recover(t.Context(), f.ledger, noArcade, f.compiled, f.facts, f.store,
		positions, true)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 0 {
		t.Fatalf("recovery considered a cascaded cell: %+v", decisions)
	}
}
