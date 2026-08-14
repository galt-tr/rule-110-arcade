package engine

import (
	"context"
	"fmt"
	"slices"
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

	out, decisions, err := Recover(t.Context(), l, noArcade, f.compiled, f.facts, f.store,
		positions, RecoverOptions{Apply: true})
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
	out, decisions, err := Recover(t.Context(), l, noArcade, f.compiled, f.facts, f.store,
		positions, RecoverOptions{Apply: true})
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
	_, decisions, err := Recover(t.Context(), l, noArcade, f.compiled, f.facts, f.store,
		positions, RecoverOptions{})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("decisions = %+v, want one", decisions)
	}

	again := f.derive(t)
	if !again[4].Unknown {
		t.Error("a dry run resolved the cell; it must only report what it would do")
	}
}

// TestRecoverWillNotResumeAGenuineRejection is the direction that must stay
// broken.
//
// The rejected transaction here really does spend the cell's tip: the network was
// asked to spend that exact output and said no. Our `failed` row is a belief
// written from an arcade status event rather than proof the transaction never
// reached anyone, so rebuilding that generation against the same tip could put a
// second transaction on a live output. The cell stays halted, the rejection stays
// recorded, and a human decides.
func TestRecoverWillNotResumeAGenuineRejection(t *testing.T) {
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
	out, decisions, err := Recover(t.Context(), f.ledger, noArcade, f.compiled, f.facts, f.store,
		positions, RecoverOptions{Apply: true})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// EXAMINED, and halted with a reason. An operator who came to look at this
	// cell must find it in the report; what they must not find is it moving.
	if len(decisions) != 1 || decisions[0].Verdict != chain.VerdictHalt {
		t.Fatalf("decisions = %+v, want a single halt", decisions)
	}
	if !strings.Contains(decisions[0].Reason, "spends this cell's tip") {
		t.Errorf("the reason should say the rejected transaction spent the tip, got: %s",
			decisions[0].Reason)
	}
	if !out[6].Halted || out[6].Tip.TxID != good.TxID {
		t.Errorf("cell 6 = %+v, want left exactly where it was", out[6])
	}

	// And the rejection is still recorded, so the halt survives the restart.
	again := f.derive(t)
	if !again[6].Halted || again[6].Tip.TxID != good.TxID {
		t.Errorf("cell 6 = %+v after a restart, want still halted on %s", again[6], good.TxID)
	}
}

// aSupersededRejection puts one cell into the state RecoverSpentTip leaves behind
// and cannot itself clear — the state roughly 67 cells of the live deployment sat
// in, halted for a reason that had already been repaired.
//
// Cell 2 of the live deployment, exactly: the record's tip was generation 1508;
// 1509 held 03a1cf74, a re-spend that arcade refused; 1509 is really held by
// 3b3d0b5a, which was mined and which recovery adopted; and 1510 holds 8db21dcb,
// marked failed — built on 03a1cf74's output, which has never existed.
//
// It returns the repaired tip and the doomed transaction sitting above it.
func aSupersededRejection(t *testing.T, f *fixture, cell int) (chain.CellChain, chain.CellChain) {
	t.Helper()
	base := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)
	tip := f.advance(t, cell, base, history.StatusMined)

	// The phantom is a sibling of the tip: both spend `base`, only one of them
	// ever existed as far as the network is concerned. It is deliberately never
	// recorded — recovery has already replaced its row with the tip's.
	phantom := f.build(t, cell, base, 7)
	doomed := f.build(t, cell, phantom, 0)
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Generation: doomed.Generation, Cell: cell, TxID: doomed.TxID,
		Status: history.StatusFailed,
		Err:    "arcade: REJECTED: MISSING_INPUTS (13): unable to find inputs",
	}); err != nil {
		t.Fatalf("record the rejection: %v", err)
	}
	if doomed.Generation != tip.Generation+1 {
		t.Fatalf("the rejection is at generation %d over a tip at %d; the shape under test is a "+
			"rejection DIRECTLY above the tip", doomed.Generation, tip.Generation)
	}
	return tip, doomed
}

// TestRecoverResumesOverASupersededRejection is the repair, end to end and
// through a restart.
//
// Nothing about the tip changes: the cell simply stops being held down by a
// verdict about a parent it no longer has, and re-creates the generation itself.
func TestRecoverResumesOverASupersededRejection(t *testing.T) {
	f := newFixture(t)
	tip, doomed := aSupersededRejection(t, f, 3)

	positions := f.derive(t)
	p := positions[3]
	if !p.Halted || p.Unknown {
		t.Fatalf("cell 3 = %+v, want halted by a rejection rather than flagged unknown", p)
	}
	if !p.Rejected || p.RejectionTxID != doomed.TxID {
		t.Fatalf("cell 3 was not offered with the rejected transaction to examine: %+v", p)
	}

	out, decisions, err := Recover(t.Context(), f.ledger, noArcade, f.compiled, f.facts, f.store,
		positions, RecoverOptions{Apply: true})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Verdict != chain.VerdictResume {
		t.Fatalf("decisions = %+v, want a single resume", decisions)
	}
	if out[3].Halted || out[3].Tip.TxID != tip.TxID || out[3].Tip.Generation != tip.Generation {
		t.Errorf("cell 3 = %+v, want released on its unchanged tip %s at generation %d",
			out[3], tip.TxID, tip.Generation)
	}

	// What the operator reads: the same verdict vocabulary as every other path,
	// naming the transaction whose refusal was set aside and why.
	line := FormatRecovery(decisions[0])
	for _, want := range []string{"resume", doomed.TxID, "does not spend this cell's tip"} {
		if !strings.Contains(line, want) {
			t.Errorf("the report does not mention %q:\n%s", want, line)
		}
	}

	// Surviving the restart is the only thing that matters. Derivation halts a
	// cell from the same rows the tip comes from, so a rejection left in the store
	// would halt this cell again at every startup for ever.
	again := f.derive(t)
	if again[3].Halted {
		t.Fatalf("cell 3 came back halted after recovery: %s", again[3].HaltReason)
	}
	if again[3].Tip.TxID != tip.TxID || again[3].Tip.Generation != tip.Generation {
		t.Errorf("re-derived tip = %s at generation %d, want the unchanged %s at %d",
			again[3].Tip.TxID, again[3].Tip.Generation, tip.TxID, tip.Generation)
	}
}

// A dry run over this path must change nothing either. It is the flag an operator
// uses to read a decision about 128 live UTXO chains before allowing it to act.
func TestRecoverStaleRejectionDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	aSupersededRejection(t, f, 3)

	positions := f.derive(t)
	_, decisions, err := Recover(t.Context(), f.ledger, noArcade, f.compiled, f.facts, f.store,
		positions, RecoverOptions{})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Verdict != chain.VerdictResume {
		t.Fatalf("decisions = %+v, want a single resume to be REPORTED", decisions)
	}

	if again := f.derive(t); !again[3].Halted {
		t.Error("a dry run released the cell; it must only report what it would do")
	}
}

// TestRecoverResumesWhenTheSpentUTXOWasNotTheTip covers the one case where both
// rejection repairs bear on the same cell.
//
// The rejection is a UTXO_SPENT, so the tip fix goes first — and it halts,
// because the transaction the message names is not one anybody can produce the
// bytes of. That halt is not the end of it: the transaction that PRODUCED the
// message does not spend this cell's tip at all, so arcade was complaining about
// one of its OTHER inputs (the fuel coin, or the phantom it was built on). The
// message was therefore never evidence about this tip, and the cell resumes.
func TestRecoverResumesWhenTheSpentUTXOWasNotTheTip(t *testing.T) {
	f := newFixture(t)
	const cell = 4
	base := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)
	tip := f.advance(t, cell, base, history.StatusMined)

	phantom := f.build(t, cell, base, 7)
	doomed := f.build(t, cell, phantom, 0)
	// Arcade blames a transaction nobody can produce: not on our record, and no
	// bytes anywhere. RecoverSpentTip can establish nothing about it.
	foreign := strings.Repeat("ee", 32)
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Generation: doomed.Generation, Cell: cell, TxID: doomed.TxID,
		Status: history.StatusFailed,
		Err: "arcade: REJECTED: UTXO_SPENT (70): UTXO_SPENT (70): " + phantom.TxID +
			":1 utxo already spent by tx " + foreign,
	}); err != nil {
		t.Fatalf("record the rejection: %v", err)
	}

	positions := f.derive(t)
	if !isSpentTip(positions[cell]) {
		t.Fatalf("cell %d = %+v, want it selected by the tip fix first", cell, positions[cell])
	}

	out, decisions, err := Recover(t.Context(), f.ledger, mined{foreign: true}, f.compiled,
		f.facts, f.store, positions, RecoverOptions{Apply: true})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Verdict != chain.VerdictResume {
		t.Fatalf("decisions = %+v, want a single resume: the rejected transaction does not spend "+
			"the tip, so the UTXO_SPENT was about one of its other inputs", decisions)
	}
	if out[cell].Halted || out[cell].Tip.TxID != tip.TxID {
		t.Errorf("cell %d = %+v, want released on its unchanged tip %s", cell, out[cell], tip.TxID)
	}
	if again := f.derive(t); again[cell].Halted {
		t.Fatalf("cell %d came back halted: %s", cell, again[cell].HaltReason)
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
		f.facts, f.store, positions, RecoverOptions{Apply: true})
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
		f.facts, f.store, positions, RecoverOptions{})
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
		f.facts, f.store, positions, RecoverOptions{Apply: true})
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
		f.facts, f.store, positions, RecoverOptions{Apply: true})
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

// aLocalFailure is the record 21 cells of the live deployment were halted by: a
// transition that died inside CreateAction when the wallet's database ran out of
// connections, wrapped in the sentinel that is set before SignAction and
// recorded with no txid, because there was never a transaction to name.
const aLocalFailure = "chain: not broadcast: chain: create step action for cell 75: create action " +
	"failed: failed to create action: storage: fund: failed to connect to `user=postgres " +
	"database=rule110`: server error: FATAL: sorry, too many clients already (SQLSTATE 53300)"

// aNeverBroadcastFailure halts one cell exactly as the old worker did: a `failed`
// row directly above a healthy tip, for a transition that never reached the
// network. It returns the tip the cell is left on.
func aNeverBroadcastFailure(t *testing.T, f *fixture, cell int, txid string) chain.CellChain {
	t.Helper()
	tip := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Generation: tip.Generation + 1, Cell: cell, TxID: txid,
		Status: history.StatusFailed, Err: aLocalFailure,
	}); err != nil {
		t.Fatalf("record the failure: %v", err)
	}
	return tip
}

// TestRecoverResumesANeverBroadcastFailure is the repair for the cells the live
// bug already halted, end to end and through a restart.
//
// Nothing is adopted and the tip does not move. The cell simply stops being held
// down by a record of something that never happened, and re-creates the
// generation itself.
func TestRecoverResumesANeverBroadcastFailure(t *testing.T) {
	f := newFixture(t)
	const cell = 3
	tip := aNeverBroadcastFailure(t, f, cell, "")

	positions := f.derive(t)
	p := positions[cell]
	if !p.Halted || p.Unknown {
		t.Fatalf("cell %d = %+v, want halted by a failure rather than flagged unknown", cell, p)
	}
	if !p.Rejected || p.RejectionTxID != "" {
		t.Fatalf("cell %d was not offered as a never-broadcast candidate: %+v", cell, p)
	}

	out, decisions, err := Recover(t.Context(), f.ledger, noArcade, f.compiled, f.facts, f.store,
		positions, RecoverOptions{Apply: true})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Verdict != chain.VerdictResume {
		t.Fatalf("decisions = %+v, want a single resume", decisions)
	}
	if out[cell].Halted || out[cell].Tip.TxID != tip.TxID || out[cell].Tip.Generation != tip.Generation {
		t.Errorf("cell %d = %+v, want released on its unchanged tip %s at generation %d",
			cell, out[cell], tip.TxID, tip.Generation)
	}

	// The same verdict vocabulary as every other path, and the failure it set
	// aside quoted in full: once the row is retracted this line is the only place
	// the reason survives.
	line := FormatRecovery(decisions[0])
	for _, want := range []string{"resume", "too many clients already", "nothing reached the network"} {
		if !strings.Contains(line, want) {
			t.Errorf("the report does not mention %q:\n%s", want, line)
		}
	}

	// Surviving the restart is the only thing that matters: derivation halts a
	// cell from the same rows the tip comes from, so a failure left in the store
	// would halt this cell again at every startup, for ever.
	again := f.derive(t)
	if again[cell].Halted {
		t.Fatalf("cell %d came back halted after recovery: %s", cell, again[cell].HaltReason)
	}
	if again[cell].Tip.TxID != tip.TxID || again[cell].Tip.Generation != tip.Generation {
		t.Errorf("re-derived tip = %s at generation %d, want the unchanged %s at %d",
			again[cell].Tip.TxID, again[cell].Tip.Generation, tip.TxID, tip.Generation)
	}
}

// A dry run over this path must change nothing either. It is the flag an operator
// uses to read a decision about 128 live UTXO chains before allowing it to act.
func TestRecoverNotBroadcastDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	const cell = 3
	aNeverBroadcastFailure(t, f, cell, "")

	positions := f.derive(t)
	_, decisions, err := Recover(t.Context(), f.ledger, noArcade, f.compiled, f.facts, f.store,
		positions, RecoverOptions{})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Verdict != chain.VerdictResume {
		t.Fatalf("decisions = %+v, want a single resume to be REPORTED", decisions)
	}
	if again := f.derive(t); !again[cell].Halted {
		t.Error("a dry run released the cell; it must only report what it would do")
	}
}

// TestRecoverWillNotResumeANotBroadcastRecordThatNamesATransaction is the
// contradiction, and it must stay halted.
//
// The sentinel says nothing was signed. The txid beside it says something was.
// Both cannot be true, and resolving that in favour of the actionable half would
// rebuild a generation over a transaction that may be on the network.
func TestRecoverWillNotResumeANotBroadcastRecordThatNamesATransaction(t *testing.T) {
	f := newFixture(t)
	const cell = 3
	tip := aNeverBroadcastFailure(t, f, cell, strings.Repeat("cc", 32))

	positions := f.derive(t)
	out, decisions, err := Recover(t.Context(), f.ledger, noArcade, f.compiled, f.facts, f.store,
		positions, RecoverOptions{Apply: true, RetryRefused: true})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	// EXAMINED — an operator must find the cell they came to look at — and halted,
	// with a reason that names the contradiction rather than the generic halt.
	if len(decisions) != 1 || decisions[0].Verdict != chain.VerdictHalt {
		t.Fatalf("decisions = %+v, want a single halt", decisions)
	}
	if !strings.Contains(decisions[0].Reason, "cannot both be true") {
		t.Errorf("the reason should name the contradiction, got: %s", decisions[0].Reason)
	}
	if !out[cell].Halted || out[cell].Tip.TxID != tip.TxID {
		t.Errorf("cell %d = %+v, want left exactly where it was", cell, out[cell])
	}
	// And the row is still there, so the halt survives the restart.
	if again := f.derive(t); !again[cell].Halted {
		t.Error("the record was retracted on a halt")
	}
}

// TestUnsignedRejectionDeleteIsExact pins the scope of the one delete that
// cannot key on a txid.
//
// Retracting a row is how a cell is released, so a delete that matched more
// broadly would release cells nothing has decided about — silently, and for the
// whole ring at once. The clauses are (generation, cell, status=failed, no
// txid), and every one of them has to be doing work.
func TestUnsignedRejectionDeleteIsExact(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	rows := []history.CellTx{
		{Generation: 5, Cell: 1, Status: history.StatusFailed, Err: aLocalFailure},         // the target
		{Generation: 5, Cell: 2, Status: history.StatusFailed, Err: aLocalFailure},         // another cell
		{Generation: 6, Cell: 1, Status: history.StatusFailed, Err: aLocalFailure},         // another generation
		{Generation: 7, Cell: 1, TxID: "abc", Status: history.StatusFailed, Err: aRefusal}, // names a transaction
		{Generation: 8, Cell: 1, TxID: "def", Status: history.StatusBroadcast},             // a real transaction
	}
	for _, r := range rows {
		if err := f.store.RecordTx(ctx, r); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	if err := f.store.DeleteUnsignedRejection(ctx, 5, 1); err != nil {
		t.Fatalf("DeleteUnsignedRejection: %v", err)
	}

	left, err := f.store.CellTips(ctx, f.facts.Cells, tipWindow(f.budget()))
	if err != nil {
		t.Fatalf("cell tips: %v", err)
	}
	got := map[string]bool{}
	for cell, tips := range left {
		for _, tip := range tips {
			got[fmt.Sprintf("%d/%d", cell, tip.Generation)] = true
		}
	}
	if got["1/5"] {
		t.Error("the row it was asked about survived")
	}
	for _, want := range []string{"2/5", "1/6", "1/7", "1/8"} {
		if !got[want] {
			t.Errorf("cell/generation %s was deleted too; the delete must match one row", want)
		}
	}
}

// TestRejectionDeleteIsExact pins the other retraction, the one keyed on a txid.
//
// It matters more now than it did. The generation it is called with used to be
// computed (tip+1) and is now read off the record that was examined, so the
// clauses are what guarantee that "the row we verified" and "the row we delete"
// are the same row — including when a pass has already moved the pile.
func TestRejectionDeleteIsExact(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	rows := []history.CellTx{
		{Generation: 5, Cell: 1, TxID: "aaa", Status: history.StatusFailed, Err: aRefusal}, // the target
		{Generation: 5, Cell: 2, TxID: "aaa", Status: history.StatusFailed, Err: aRefusal}, // another cell
		{Generation: 6, Cell: 1, TxID: "bbb", Status: history.StatusFailed, Err: aRefusal}, // another generation
		{Generation: 7, Cell: 1, Status: history.StatusFailed, Err: aLocalFailure},         // names no transaction
		{Generation: 8, Cell: 1, TxID: "ddd", Status: history.StatusBroadcast},             // a real transaction
	}
	for _, r := range rows {
		if err := f.store.RecordTx(ctx, r); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	// A generation nothing was verified at — what a computed tip+1 became once a
	// pass had retracted the row that used to be there — must delete nothing.
	if err := f.store.DeleteRejection(ctx, 4, 1, "aaa"); err != nil {
		t.Fatalf("DeleteRejection: %v", err)
	}
	// So must the right generation with the wrong transaction: a row replaced
	// since the decision was made is a row nobody decided about.
	if err := f.store.DeleteRejection(ctx, 5, 1, "zzz"); err != nil {
		t.Fatalf("DeleteRejection: %v", err)
	}
	// And a txid-keyed delete cannot reach the row that names no transaction.
	if err := f.store.DeleteRejection(ctx, 7, 1, ""); err == nil {
		t.Error("deleting a rejection with no transaction id was allowed; there is no row that can " +
			"be shown to be about")
	}

	if err := f.store.DeleteRejection(ctx, 5, 1, "aaa"); err != nil {
		t.Fatalf("DeleteRejection: %v", err)
	}

	left, err := f.store.CellTips(ctx, f.facts.Cells, tipWindow(f.budget()))
	if err != nil {
		t.Fatalf("cell tips: %v", err)
	}
	got := map[string]bool{}
	for cell, tips := range left {
		for _, tip := range tips {
			got[fmt.Sprintf("%d/%d", cell, tip.Generation)] = true
		}
	}
	if got["1/5"] {
		t.Error("the row it was asked about survived")
	}
	for _, want := range []string{"2/5", "1/6", "1/7", "1/8"} {
		if !got[want] {
			t.Errorf("cell/generation %s was deleted too; the delete must match one row", want)
		}
	}
}

// aRefusal is arcade refusing a transaction for a reason we have never explained.
// It names the transaction and nothing else: not which input, not which rule.
// 13 cells of the live deployment are halted by exactly this.
const aRefusal = "arcade: REJECTED: PROCESSING (4): " +
	"[ProcessTransaction][8b1f0c] failed to validate transaction"

// TestRefusedGenerationIsOnlyRetriedWhenAskedFor is the opt-in repair, and the
// gate is as much the point as the repair.
//
// The refused transaction really did spend this cell's tip, so nothing in the
// record explains anything: the stale-rejection check reads its bytes, finds the
// tip in its inputs and halts, correctly. What is left is the one fact that
// holds regardless — a refused transaction spends nothing, so the tip is still
// unspent and rebuilding cannot double spend. That makes the retry SAFE, not
// warranted, which is why it happens only when an operator asks.
func TestRefusedGenerationIsOnlyRetriedWhenAskedFor(t *testing.T) {
	f := newFixture(t)
	const cell = 6
	tip := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)

	refused := f.build(t, cell, tip, 3)
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Generation: refused.Generation, Cell: cell, TxID: refused.TxID,
		Status: history.StatusFailed, Err: aRefusal,
	}); err != nil {
		t.Fatalf("record the refusal: %v", err)
	}

	positions := f.derive(t)
	if !positions[cell].Rejected || positions[cell].RejectionTxID != refused.TxID {
		t.Fatalf("cell %d was not offered with the refused transaction to examine: %+v",
			cell, positions[cell])
	}

	// Off by default, including under -apply: this is the shape TestRecover-
	// WillNotResumeAGenuineRejection halts, and it must keep halting unless asked.
	out, decisions, err := Recover(t.Context(), f.ledger, noArcade, f.compiled, f.facts, f.store,
		positions, RecoverOptions{Apply: true})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Verdict != chain.VerdictHalt {
		t.Fatalf("decisions = %+v, want a single halt without the flag", decisions)
	}
	if !out[cell].Halted {
		t.Error("a refusal of a transaction that spent the tip released the cell unasked")
	}
	if again := f.derive(t); !again[cell].Halted {
		t.Fatal("the refusal was retracted without the flag")
	}

	// Asked for, it resumes: the tip is unchanged and the cell re-creates the
	// generation that was refused.
	out, decisions, err = Recover(t.Context(), f.ledger, noArcade, f.compiled, f.facts, f.store,
		positions, RecoverOptions{Apply: true, RetryRefused: true})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 1 || decisions[0].Verdict != chain.VerdictResume {
		t.Fatalf("decisions = %+v, want a single resume with -retry-refused", decisions)
	}
	if out[cell].Halted || out[cell].Tip.TxID != tip.TxID || out[cell].Tip.Generation != tip.Generation {
		t.Errorf("cell %d = %+v, want released on its unchanged tip %s at generation %d",
			cell, out[cell], tip.TxID, tip.Generation)
	}

	// What the operator reads must not read as a diagnosis.
	line := FormatRecovery(decisions[0])
	for _, want := range []string{"resume", refused.TxID, "spends nothing", "NOT established"} {
		if !strings.Contains(line, want) {
			t.Errorf("the report does not say %q:\n%s", want, line)
		}
	}

	again := f.derive(t)
	if again[cell].Halted {
		t.Fatalf("cell %d came back halted after the retry was applied: %s", cell, again[cell].HaltReason)
	}
	if again[cell].Tip.TxID != tip.TxID {
		t.Errorf("re-derived tip = %s, want the unchanged %s", again[cell].Tip.TxID, tip.TxID)
	}
}

// TestRetryNeverReachesACascade keeps the newest repair away from the four cells
// that must be untangled by hand.
//
// Cells 34, 51, 64 and 91 carry about 170 stacked refusals each over tips near
// generation 300. Nothing here may touch them — resuming would achieve nothing
// anyway, since the rejections underneath the newest would halt the cell again
// at the next startup — and the guard is derivation refusing to flag a rejection
// that is not directly above the tip. The flag must not be a way around it.
func TestRetryNeverReachesACascade(t *testing.T) {
	f := newFixture(t)
	const cell = 5
	tip := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)

	// The first refusal spent the tip; everything after it spent the refusal.
	parent := f.build(t, cell, tip, 9)
	newest := parent
	for range tipWindow(f.budget()) + 4 {
		if err := f.store.RecordTx(t.Context(), history.CellTx{
			Generation: newest.Generation, Cell: cell, TxID: newest.TxID,
			Status: history.StatusFailed, Err: aRefusal,
		}); err != nil {
			t.Fatalf("record refusal at %d: %v", newest.Generation, err)
		}
		parent = newest
		newest = f.build(t, cell, parent, 0)
	}

	positions := f.derive(t)
	if positions[cell].Rejected {
		t.Fatalf("a cascade was offered for examination: %+v", positions[cell])
	}
	_, decisions, err := Recover(t.Context(), f.ledger, noArcade, f.compiled, f.facts, f.store,
		positions, RecoverOptions{Apply: true, RetryRefused: true})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 0 {
		t.Fatalf("recovery considered a cascaded cell with -retry-refused: %+v", decisions)
	}
	if again := f.derive(t); !again[cell].Halted || again[cell].Tip.TxID != tip.TxID {
		t.Errorf("cell %d = %+v, want left exactly where it was", cell, again[cell])
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

	for gen := good.Generation + 1; gen <= good.Generation+uint64(tipWindow(f.budget()))+2; gen++ {
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
		positions, RecoverOptions{Apply: true})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 0 {
		t.Fatalf("recovery considered a cascaded cell: %+v", decisions)
	}
}

// TestCascadeIsNeverOfferedToTheStaleRejectionCheck is the cascade guard where it
// now matters most, built out of real transactions rather than placeholder ids.
//
// In a cascade every attempt after the first spends the PREVIOUS rejected
// transaction's output, because the build that produced this deployment's older
// history advanced a cell's tip onto the output of a transaction the network had
// refused. So every rejection in the pile spends a phantom rather than the tip —
// which is exactly the condition chain.RecoverStaleRejection resumes on. The only
// thing standing between cells 34, 51, 64 and 91 and being resumed automatically
// is that derivation counts the pile above the tip and refuses to offer one this
// deep — see WreckageBudget — and this pins it.
//
// Resuming them would achieve nothing anyway: the 169 rejections underneath the
// newest one would halt the cell again at the next startup. Untangling that is
// task 34's job and a human's.
func TestCascadeIsNeverOfferedToTheStaleRejectionCheck(t *testing.T) {
	f := newFixture(t)
	const cell = 5
	tip := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)

	// The first refusal spent the tip; everything after it spent the refusal.
	parent := f.build(t, cell, tip, 9)
	newest := parent
	for range tipWindow(f.budget()) + 4 {
		if err := f.store.RecordTx(t.Context(), history.CellTx{
			Generation: newest.Generation, Cell: cell, TxID: newest.TxID,
			Status: history.StatusFailed, Err: "arcade: REJECTED: MISSING_INPUTS (13)",
		}); err != nil {
			t.Fatalf("record rejection at %d: %v", newest.Generation, err)
		}
		parent = newest
		newest = f.build(t, cell, parent, 0)
	}

	positions := f.derive(t)
	p := positions[cell]
	if !p.Halted {
		t.Fatal("a cell under a cascade of rejections must not advance")
	}
	if p.Rejected || p.RejectionTxID != "" {
		t.Errorf("a cascade was offered for examination: %+v", p)
	}
	if p.Tip.TxID != tip.TxID {
		t.Fatalf("derived tip = %s, want the newest accepted record %s", p.Tip.TxID, tip.TxID)
	}

	_, decisions, err := Recover(t.Context(), f.ledger, noArcade, f.compiled, f.facts, f.store,
		positions, RecoverOptions{Apply: true})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 0 {
		t.Fatalf("recovery considered a cascaded cell: %+v", decisions)
	}
	if again := f.derive(t); !again[cell].Halted || again[cell].Tip.TxID != tip.TxID {
		t.Errorf("cell %d = %+v, want left exactly where it was", cell, again[cell])
	}

	// And the guard really is the only thing stopping it: handed one of these
	// rejections directly, the decision resumes — at the generation the cascade
	// actually holds it at, which since the adjacency test was dropped is no
	// longer a barrier either.
	rec, err := chain.RecoverStaleRejection(t.Context(), f.ledger, p.Tip, parent.Generation,
		parent.TxID, "arcade: REJECTED: MISSING_INPUTS (13)")
	if err != nil {
		t.Fatalf("RecoverStaleRejection: %v", err)
	}
	if rec.Verdict != chain.VerdictResume {
		t.Fatalf("verdict = %s; this test only means something if a cascade's rejections would "+
			"otherwise resume: %s", rec.Verdict, rec.Reason)
	}
}

// aRetractedRow puts one cell into the state a repair pass leaves behind, and
// that every pass after it then reported as needing nothing.
//
// Cell 12 of the live deployment, exactly. Its record was generation 991 mined,
// then 992, 993 and 994 all `failed` — one break and the two transitions the old
// build stacked on its phantom output. A pass retracted 992. From then on
// derivation halted the cell on 993 ("generation 993 was rejected, so this cell's
// chain ends at generation 991") while recovery went and looked at 992, which no
// longer existed, so it decided there was nothing to do and the tool reported
// success. Repeated `recover -apply` runs converged on a no-op with 23 cells
// still halted.
//
// The generations here are small because the fixture walks the automaton from the
// seed, but the shape is exact: a tip, a HOLE directly above it, and two
// rejections above that, each built on the output of the one below.
//
// It returns the tip and the two doomed transactions, oldest first.
func aRetractedRow(t *testing.T, f *fixture, cell int) (chain.CellChain, chain.CellChain, chain.CellChain) {
	t.Helper()
	tip := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)

	// The retracted generation. Its transaction was refused and its row is gone —
	// which is the whole reason nothing sits at tip+1 any more — but the two
	// transactions built on its output are still recorded.
	phantom := f.build(t, cell, tip, 7)
	first := f.build(t, cell, phantom, 0)
	second := f.build(t, cell, first, 0)

	for _, doomed := range []chain.CellChain{first, second} {
		if err := f.store.RecordTx(t.Context(), history.CellTx{
			Generation: doomed.Generation, Cell: cell, TxID: doomed.TxID,
			Status: history.StatusFailed,
			Err:    "arcade: REJECTED: MISSING_INPUTS (13): unable to find inputs",
		}); err != nil {
			t.Fatalf("record the rejection at generation %d: %v", doomed.Generation, err)
		}
	}
	if first.Generation != tip.Generation+2 || second.Generation != tip.Generation+3 {
		t.Fatalf("the shape under test is a tip at %d with a hole above it and rejections at %d and "+
			"%d; got %d and %d", tip.Generation, tip.Generation+2, tip.Generation+3,
			first.Generation, second.Generation)
	}
	return tip, first, second
}

// failedRows reports which generations the store still holds a rejection at for
// one cell. Retraction is how a cell is released, so which rows survive a pass is
// the property, not just what the decision said.
func failedRows(t *testing.T, f *fixture, cell int) []uint64 {
	t.Helper()
	rows, err := f.store.CellTips(t.Context(), f.facts.Cells, tipWindow(f.budget()))
	if err != nil {
		t.Fatalf("cell tips: %v", err)
	}
	var out []uint64
	for _, r := range rows[cell] {
		if r.Status == history.StatusFailed {
			out = append(out, r.Generation)
		}
	}
	return out
}

// TestRecoveryExaminesTheRecordDerivationHaltedOn is the repair for the cells
// that repeated passes could not finish, end to end.
//
// The halting record is at tip+2, so everything that computed tip+1 examined a
// generation nothing was recorded at. Recovery now takes the generation off the
// record derivation stopped on, which is also the record the halt message names —
// the two come out of the same place, so they cannot disagree.
func TestRecoveryExaminesTheRecordDerivationHaltedOn(t *testing.T) {
	f := newFixture(t)
	const cell = 3
	tip, first, second := aRetractedRow(t, f, cell)

	p := f.derive(t)[cell]
	if !p.Halted || p.Unknown {
		t.Fatalf("cell %d = %+v, want halted by a rejection rather than flagged unknown", cell, p)
	}
	if p.Tip.TxID != tip.TxID || p.Tip.Generation != tip.Generation {
		t.Fatalf("derived tip = %+v, want the last accepted record %s at generation %d",
			p.Tip, tip.TxID, tip.Generation)
	}
	if p.RejectedAt == p.Tip.Generation+1 {
		t.Fatalf("the halting record is at tip+1, so this test cannot show anything: %+v", p)
	}
	if !p.Rejected || p.RejectedAt != first.Generation || p.RejectionTxID != first.TxID {
		t.Fatalf("cell %d = %+v, want the OLDEST rejection above the tip (generation %d, %s) offered "+
			"for examination", cell, p, first.Generation, first.TxID)
	}
	// The message an operator reads and the row recovery acts on are the same row.
	if !strings.Contains(p.HaltReason, fmt.Sprintf("generation %d was rejected", first.Generation)) {
		t.Errorf("the halt names a different record from the one recovery will examine: %q", p.HaltReason)
	}

	// A bystander: another cell with a `failed` row at the SAME generation, sitting
	// below its own tip so it is not a candidate. Retraction has to leave it alone.
	const other = 6
	otherTip := f.genesisTip(other)
	for range 5 {
		otherTip = f.advance(t, other, otherTip, history.StatusMined)
	}
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Generation: first.Generation, Cell: other, TxID: strings.Repeat("ab", 32),
		Status: history.StatusFailed, Err: "arcade: REJECTED: MISSING_INPUTS (13)",
	}); err != nil {
		t.Fatalf("record the bystander: %v", err)
	}

	// One pass per rejection, oldest first, exactly as `rule110 recover -apply`
	// runs it. Each pass must examine the row it halted on and retract that row
	// and no other.
	for pass, want := range []chain.CellChain{first, second} {
		positions := f.derive(t)
		if got := positions[cell]; !got.Rejected || got.RejectedAt != want.Generation ||
			got.RejectionTxID != want.TxID {
			t.Fatalf("pass %d offered %+v, want the rejection at generation %d (%s)",
				pass+1, got, want.Generation, want.TxID)
		}
		_, decisions, err := Recover(t.Context(), f.ledger, noArcade, f.compiled, f.facts, f.store,
			positions, RecoverOptions{Apply: true})
		if err != nil {
			t.Fatalf("pass %d: Recover: %v", pass+1, err)
		}
		if len(decisions) != 1 || decisions[0].Cell != cell ||
			decisions[0].Verdict != chain.VerdictResume {
			t.Fatalf("pass %d decisions = %+v, want a single resume for cell %d", pass+1, decisions, cell)
		}
		// The decision has to be about the record that was examined, generation and
		// transaction together. Naming the transaction while carrying tip+1 as its
		// generation is the disagreement this whole change exists to remove: the
		// operator reads one row, the delete is aimed at another.
		if want := fmt.Sprintf("generation %d (%s)", want.Generation, want.TxID); !strings.Contains(
			decisions[0].Reason, want) {
			t.Errorf("pass %d did not decide about %q: %s", pass+1, want, decisions[0].Reason)
		}
		if got := failedRows(t, f, cell); slices.Contains(got, want.Generation) {
			t.Errorf("pass %d left the row it verified in place: rejections at %v", pass+1, got)
		}
	}

	if got := failedRows(t, f, cell); len(got) != 0 {
		t.Errorf("rejections at %v survived; the cell halts again at the next startup", got)
	}
	if got := failedRows(t, f, other); !slices.Contains(got, first.Generation) {
		t.Errorf("the bystander's row at generation %d was retracted too; the delete must be keyed "+
			"on the cell as well as the generation (cell %d now has %v)",
			first.Generation, other, got)
	}

	// The only thing that matters: the cell comes back able to advance, from the
	// tip it always had, with nothing re-spent.
	again := f.derive(t)[cell]
	if again.Halted || again.Rejected || again.Unknown {
		t.Fatalf("cell %d came back %+v after two passes: %s", cell, again, again.HaltReason)
	}
	if again.Tip.TxID != tip.TxID || again.Tip.Generation != tip.Generation {
		t.Errorf("re-derived tip = %s at generation %d, want the unchanged %s at %d",
			again.Tip.TxID, again.Tip.Generation, tip.TxID, tip.Generation)
	}
}

// TestRecoverRefusesAPileTooDeepToBeOneBreak is the cascade guard at the exact
// point it now turns, built out of real transactions.
//
// Cells 34, 51, 64 and 91 are refused by the derivation window never reaching
// their tips at all. This is the other side of that: a pile small enough for
// derivation to see whole, and still too big to be one break with a tail. Every
// rejection in it spends a phantom, so every one of them WOULD resume if it were
// offered — which is why the count has to be checked before anything is offered.
func TestRecoverRefusesAPileTooDeepToBeOneBreak(t *testing.T) {
	f := newFixture(t)
	const cell = 5
	tip := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)

	// The first refusal spent the tip; every one after it spent the refusal below.
	doomed := []chain.CellChain{f.build(t, cell, tip, 9)}
	for range f.budget() {
		doomed = append(doomed, f.build(t, cell, doomed[len(doomed)-1], 0))
	}
	if len(doomed) <= f.budget() || len(doomed) >= tipWindow(f.budget()) {
		t.Fatalf("this test needs a pile of %d that derivation can still see whole, and the window is %d",
			len(doomed), tipWindow(f.budget()))
	}
	for _, d := range doomed {
		if err := f.store.RecordTx(t.Context(), history.CellTx{
			Generation: d.Generation, Cell: cell, TxID: d.TxID,
			Status: history.StatusFailed, Err: "arcade: REJECTED: MISSING_INPUTS (13)",
		}); err != nil {
			t.Fatalf("record rejection at %d: %v", d.Generation, err)
		}
	}

	positions := f.derive(t)
	p := positions[cell]
	if !p.Halted {
		t.Fatal("a cell under a cascade of rejections must not advance")
	}
	if p.Rejected || p.RejectionTxID != "" {
		t.Fatalf("a cascade was offered for examination: %+v", p)
	}
	// The operator still gets the cell, with the count that decided it.
	for _, want := range []string{"cascade", fmt.Sprintf("%d rejections", len(doomed))} {
		if !strings.Contains(p.HaltReason, want) {
			t.Errorf("the halt does not say %q: %q", want, p.HaltReason)
		}
	}
	_, decisions, err := Recover(t.Context(), f.ledger, noArcade, f.compiled, f.facts, f.store,
		positions, RecoverOptions{Apply: true, RetryRefused: true})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(decisions) != 0 {
		t.Fatalf("recovery considered a cascaded cell, with -retry-refused: %+v", decisions)
	}
	if again := f.derive(t); !again[cell].Halted || again[cell].Tip.TxID != tip.TxID {
		t.Errorf("cell %d = %+v, want left exactly where it was", cell, again[cell])
	}

	// And the count is what decided it, not the shape or the messages: take one row
	// away and the very same wreckage is offered, oldest first.
	if err := f.store.DeleteRejection(t.Context(), doomed[0].Generation, cell,
		doomed[0].TxID); err != nil {
		t.Fatalf("delete the bottom row: %v", err)
	}
	shorter := f.derive(t)[cell]
	if !shorter.Rejected || shorter.RejectedAt != doomed[1].Generation {
		t.Fatalf("a pile of %d was refused and a pile of %d was not offered either (%+v); the guard "+
			"must turn on the count, or it is refusing everything",
			len(doomed), len(doomed)-1, shorter)
	}
}

// TestAnAttemptOnTopOfACascadeIsNotAWayRound closes the hole the count guard
// would otherwise leave open.
//
// Recover dispatches on Unknown BEFORE Rejected, and chain.RecoverCell has no
// count guard at all: it walks the WALLET's actions forward. In a cascade those
// link one to the next — every attempt spent the previous refused transaction's
// output, so each verifies as the successor of the one below — while the status
// the wallet holds is its own lifecycle rather than arcade's verdict. So an
// `attempting` row left on top of a pile would have carried the cell straight
// past the guard and into adopting refused transactions as its tip, which is the
// direction that destroys a cell rather than stalling it.
//
// Both derivation paths are covered: a pile the window can see whole, and one
// deeper than the window.
func TestAnAttemptOnTopOfACascadeIsNotAWayRound(t *testing.T) {
	for name, depth := range map[string]int{
		"the window sees the whole pile":     defaultBudget() + 1,
		"the pile is deeper than the window": tipWindow(defaultBudget()) + 2,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			const cell = 5
			tip := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)

			doomed := f.build(t, cell, tip, 9)
			for range depth {
				if err := f.store.RecordTx(t.Context(), history.CellTx{
					Generation: doomed.Generation, Cell: cell, TxID: doomed.TxID,
					Status: history.StatusFailed, Err: "arcade: REJECTED: MISSING_INPUTS (13)",
				}); err != nil {
					t.Fatalf("record rejection at %d: %v", doomed.Generation, err)
				}
				doomed = f.build(t, cell, doomed, 0)
			}
			// And one unresolved write-ahead record on top of all of it.
			if err := f.store.RecordTx(t.Context(), history.CellTx{
				Generation: doomed.Generation, Cell: cell, Status: history.StatusAttempting,
			}); err != nil {
				t.Fatalf("record the attempt: %v", err)
			}

			p := f.derive(t)[cell]
			if !p.Halted {
				t.Fatal("a cell under a cascade must not advance")
			}
			if p.Rejected || p.Unknown {
				t.Fatalf("a cascade with an attempt on top was offered to recovery: %+v", p)
			}
			if p.Tip.TxID != tip.TxID {
				t.Fatalf("derived tip = %s, want the newest accepted record %s", p.Tip.TxID, tip.TxID)
			}

			// A wallet that would happily walk the whole cascade forward if it were
			// ever asked. It must not be asked.
			l := &recoveringLedger{actions: map[int][]chain.CellAction{}}
			l.raw = f.ledger.raw
			_, decisions, err := Recover(t.Context(), l, noArcade, f.compiled, f.facts, f.store,
				f.derive(t), RecoverOptions{Apply: true, RetryRefused: true})
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if len(decisions) != 0 {
				t.Fatalf("recovery considered a cascaded cell through its attempt row: %+v", decisions)
			}
			if again := f.derive(t); !again[cell].Halted || again[cell].Tip.TxID != tip.TxID {
				t.Errorf("cell %d = %+v, want left exactly where it was", cell, again[cell])
			}
		})
	}
}

// TestABuriedSeenTipIsStillATip covers the disagreement that made a repairable
// cell read as lost.
//
// Derivation reads a shallow window and digs deeper only when nothing in it
// settled. The two answers have to use the same definition of "settled": the
// window accepts broadcast, seen or mined, and the dig used to accept only
// broadcast or mined. `seen` is what the status stream records between the two,
// so a cell whose newest accepted transaction had not been mined yet — and which
// then collected enough wreckage to push that row out of the window — was
// reported as having no derivable tip at all. That reads as a lost cell, and it
// is the one shape where the operator is told to give up on a cell whose tip is
// perfectly well defined.
func TestABuriedSeenTipIsStillATip(t *testing.T) {
	f := newFixture(t)
	const cell = 2
	tip := f.advance(t, cell, f.genesisTip(cell), history.StatusSeen)

	doomed := f.build(t, cell, tip, 9)
	for range tipWindow(f.budget()) + 2 {
		if err := f.store.RecordTx(t.Context(), history.CellTx{
			Generation: doomed.Generation, Cell: cell, TxID: doomed.TxID,
			Status: history.StatusFailed, Err: "arcade: REJECTED: MISSING_INPUTS (13)",
		}); err != nil {
			t.Fatalf("record rejection at %d: %v", doomed.Generation, err)
		}
		doomed = f.build(t, cell, doomed, 0)
	}

	p := f.derive(t)[cell]
	if !p.Halted {
		t.Fatal("a cell under a cascade must not advance")
	}
	if p.Tip.TxID != tip.TxID || p.Tip.Generation != tip.Generation {
		t.Fatalf("derived tip = %s at generation %d, want the buried `seen` record %s at %d",
			p.Tip.TxID, p.Tip.Generation, tip.TxID, tip.Generation)
	}
	if strings.Contains(p.HaltReason, "cannot be derived") {
		t.Errorf("the halt reports the cell as underivable, which reads as lost: %q", p.HaltReason)
	}
	// It is still a cascade, so still nobody's to repair automatically.
	if p.Rejected {
		t.Errorf("a cascade over a `seen` tip was offered for examination: %+v", p)
	}
}
