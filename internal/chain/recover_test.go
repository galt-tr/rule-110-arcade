package chain

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
)

// fakeLedger is a wallet made of maps.
//
// It exists because every branch of RecoverCell decides whether to re-spend a
// live UTXO, including the branches reached only when the wallet or the network
// FAILS — and those are the ones that matter most. A real wallet cannot be made
// to fail on demand; this can.
type fakeLedger struct {
	raw     map[string][]byte
	actions map[int][]CellAction
	total   map[int]int

	rawErr    error
	actionErr error
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{
		raw:     map[string][]byte{},
		actions: map[int][]CellAction{},
		total:   map[int]int{},
	}
}

func (f *fakeLedger) RawTx(_ context.Context, txid string) ([]byte, error) {
	if f.rawErr != nil {
		return nil, f.rawErr
	}
	raw, ok := f.raw[txid]
	if !ok {
		return nil, errors.New("fake: no such transaction")
	}
	return raw, nil
}

func (f *fakeLedger) CellActions(_ context.Context, cell, _ int) ([]CellAction, int, error) {
	if f.actionErr != nil {
		return nil, 0, f.actionErr
	}
	return f.actions[cell], f.total[cell], nil
}

// addAction registers a wallet action for a cell, and the transaction's bytes
// when it was signed.
func (f *fakeLedger) addAction(t *testing.T, compiled *cellscript.Compiled, cell int,
	gen uint64, row ca.Row, status string, tx *transaction.Transaction) {
	t.Helper()

	a := CellAction{Status: status, Generation: gen, HasGeneration: true}
	if tx != nil {
		a.TxID = tx.TxID().String()
		a.Satoshis = tx.Outputs[0].Satoshis
		lock, err := compiled.LockingScript(cell, row)
		if err != nil {
			t.Fatalf("locking script: %v", err)
		}
		a.LockingScript = lock
		f.raw[a.TxID] = tx.Bytes()
	}
	f.actions[cell] = append(f.actions[cell], a)
	f.total[cell] = len(f.actions[cell])
}

// aGenesisTip builds cell 0's genesis tip and the ledger that knows about it.
func aGenesisTip(t *testing.T) (*cellscript.Compiled, *fakeLedger, CellChain, ca.Row) {
	t.Helper()
	compiled := compileForTest(t)
	seed := seedRow(t)
	gen := genesisTx(t, compiled, seed, 1)

	l := newFakeLedger()
	l.raw[gen.TxID().String()] = gen.Bytes()

	tip, err := DeriveTip(compiled, 0, 0, seed, gen.TxID().String(), gen.Bytes())
	if err != nil {
		t.Fatalf("derive tip: %v", err)
	}
	return compiled, l, tip, seed
}

// TestLedgerErrorNeverResumes is the double-spend property, as a test.
//
// The two ways to be wrong are not equally bad. Adopting a successor that never
// reached the network costs nothing — we do not re-spend the tip, and the
// wallet's own resend path rebroadcasts it. Concluding "nothing was broadcast"
// when something was destroys the cell: we build on a phantom output and the
// rejection is indistinguishable from an ordinary one. That is what killed cells
// 34 and 51.
//
// So "nothing was broadcast" may only ever be concluded from POSITIVE evidence —
// an action that exists and carries no txid, or a newest generation below the
// one attempted. A lookup that fails, times out, 404s or returns an empty page
// is not evidence of anything, and must leave the cell halted.
func TestLedgerErrorNeverResumes(t *testing.T) {
	compiled, _, tip, _ := aGenesisTip(t)
	ctx := context.Background()

	t.Run("the action lookup fails", func(t *testing.T) {
		l := newFakeLedger()
		l.actionErr = errors.New("connection reset")

		rec, err := RecoverCell(ctx, l, compiled, tip, 1, testCells, ca.Rule110, 8)
		if err != nil {
			t.Fatalf("RecoverCell: %v", err)
		}
		if rec.Verdict != VerdictHalt {
			t.Fatalf("verdict = %s on a failed lookup, want halt: %s", rec.Verdict, rec.Reason)
		}
	})

	t.Run("the raw transaction cannot be read", func(t *testing.T) {
		compiled, l, tip, seed := aGenesisTip(t)
		next := ca.Rule110.Step(seed)
		tx := stepTx(t, compiled, 0, next, tip.Satoshis, outpointOf(tip))
		l.addAction(t, compiled, 0, 1, next, "unproven", tx)
		// The signed transaction exists, but its bytes are unreadable, so it
		// cannot be verified — and an unverifiable signed transaction must never
		// be treated as one that was never signed.
		l.rawErr = errors.New("timeout")

		rec, err := RecoverCell(ctx, l, compiled, tip, 1, testCells, ca.Rule110, 8)
		if err != nil {
			t.Fatalf("RecoverCell: %v", err)
		}
		if rec.Verdict != VerdictHalt {
			t.Fatalf("verdict = %s when the bytes could not be read, want halt: %s",
				rec.Verdict, rec.Reason)
		}
	})

	t.Run("the wallet reports nothing for a cell that has advanced", func(t *testing.T) {
		// An empty wallet and a cell at generation 5 cannot both be true of the
		// wallet that ran this cell. Reading it as "nothing was ever signed" is
		// how a restored snapshot re-spends 128 live tips.
		l := newFakeLedger()
		ahead := tip
		ahead.Generation = 5

		rec, err := RecoverCell(ctx, l, compiled, ahead, 6, testCells, ca.Rule110, 8)
		if err != nil {
			t.Fatalf("RecoverCell: %v", err)
		}
		if rec.Verdict != VerdictHalt {
			t.Fatalf("verdict = %s for an empty wallet at generation 5, want halt: %s",
				rec.Verdict, rec.Reason)
		}
		if !strings.Contains(rec.Reason, "not the wallet") {
			t.Errorf("the reason should name the wallet mismatch, got: %s", rec.Reason)
		}
	})

	t.Run("the actions carry no readable generation", func(t *testing.T) {
		l := newFakeLedger()
		l.actions[0] = []CellAction{{TxID: strings.Repeat("aa", 32), Status: "unproven"}}
		l.total[0] = 1

		rec, err := RecoverCell(ctx, l, compiled, tip, 1, testCells, ca.Rule110, 8)
		if err != nil {
			t.Fatalf("RecoverCell: %v", err)
		}
		if rec.Verdict != VerdictHalt {
			t.Fatalf("verdict = %s with no readable generation, want halt: %s", rec.Verdict, rec.Reason)
		}
	})
}

// No action at all, on a cell that has never advanced, is positive evidence:
// CreateAction never completed, so the genesis output is certainly unspent.
func TestNoActionResumes(t *testing.T) {
	compiled, l, tip, _ := aGenesisTip(t)

	rec, err := RecoverCell(context.Background(), l, compiled, tip, 1, testCells, ca.Rule110, 8)
	if err != nil {
		t.Fatalf("RecoverCell: %v", err)
	}
	if rec.Verdict != VerdictResume {
		t.Fatalf("verdict = %s, want resume: %s", rec.Verdict, rec.Reason)
	}
	if rec.Tip.TxID != tip.TxID || rec.Tip.Generation != tip.Generation {
		t.Errorf("resume moved the tip to %+v; it must be left exactly where it was", rec.Tip)
	}
}

// An action that exists but carries no txid was never signed: storage inserts
// the row before the transaction exists. Nothing reached the network.
func TestUnsignedAttemptResumes(t *testing.T) {
	compiled, l, tip, seed := aGenesisTip(t)
	next := ca.Rule110.Step(seed)
	l.addAction(t, compiled, 0, 1, next, "unsigned", nil)

	rec, err := RecoverCell(context.Background(), l, compiled, tip, 1, testCells, ca.Rule110, 8)
	if err != nil {
		t.Fatalf("RecoverCell: %v", err)
	}
	if rec.Verdict != VerdictResume {
		t.Fatalf("verdict = %s, want resume: %s", rec.Verdict, rec.Reason)
	}
}

// The newest action being older than the generation attempted means the attempt
// never produced an action at all.
func TestOlderNewestActionResumes(t *testing.T) {
	compiled, l, tip, seed := aGenesisTip(t)
	l.addAction(t, compiled, 0, 0, seed, "completed", nil)

	rec, err := RecoverCell(context.Background(), l, compiled, tip, 1, testCells, ca.Rule110, 8)
	if err != nil {
		t.Fatalf("RecoverCell: %v", err)
	}
	if rec.Verdict != VerdictResume {
		t.Fatalf("verdict = %s, want resume: %s", rec.Verdict, rec.Reason)
	}
}

// A signed transaction exists, so it is adopted rather than re-spent — and the
// tip moves to it without anything being broadcast.
func TestSignedAttemptIsAdopted(t *testing.T) {
	compiled, l, tip, seed := aGenesisTip(t)
	next := ca.Rule110.Step(seed)
	tx := stepTx(t, compiled, 0, next, tip.Satoshis, outpointOf(tip))
	l.addAction(t, compiled, 0, 1, next, "unproven", tx)

	rec, err := RecoverCell(context.Background(), l, compiled, tip, 1, testCells, ca.Rule110, 8)
	if err != nil {
		t.Fatalf("RecoverCell: %v", err)
	}
	if rec.Verdict != VerdictAdopt {
		t.Fatalf("verdict = %s, want adopt: %s", rec.Verdict, rec.Reason)
	}
	if rec.Tip.TxID != tx.TxID().String() || rec.Tip.Generation != 1 {
		t.Errorf("adopted tip = %+v, want generation 1 on %s", rec.Tip, tx.TxID())
	}
	if len(rec.Steps) != 1 {
		t.Errorf("steps = %d, want the single adopted generation", len(rec.Steps))
	}
}

// TestRejectedActionIsNotRecovered: a rejection is not an unknown tip.
//
// The rejected transaction's output does not exist, so there is nothing to
// adopt; and the output it tried to spend is gone, so there is nothing to resume
// from either. The cell's chain has ended and a human decides what to do.
func TestRejectedActionIsNotRecovered(t *testing.T) {
	compiled, l, tip, seed := aGenesisTip(t)
	next := ca.Rule110.Step(seed)
	tx := stepTx(t, compiled, 0, next, tip.Satoshis, outpointOf(tip))
	l.addAction(t, compiled, 0, 1, next, "failed", tx)

	rec, err := RecoverCell(context.Background(), l, compiled, tip, 1, testCells, ca.Rule110, 8)
	if err != nil {
		t.Fatalf("RecoverCell: %v", err)
	}
	if rec.Verdict != VerdictHalt {
		t.Fatalf("verdict = %s for a rejected transaction, want halt: %s", rec.Verdict, rec.Reason)
	}
	if !strings.Contains(rec.Reason, "failed") {
		t.Errorf("the reason should say the wallet marked it failed, got: %s", rec.Reason)
	}
}

// TestWalkForwardIsChainedAndBounded: several generations may have been signed
// before the crash, and each must be shown to spend the previous one — the same
// outpoint check that makes a single adoption safe, applied link by link.
func TestWalkForwardIsChainedAndBounded(t *testing.T) {
	compiled, l, tip, seed := aGenesisTip(t)

	row := seed
	current := tip
	for gen := uint64(1); gen <= 3; gen++ {
		row = ca.Rule110.Step(row)
		tx := stepTx(t, compiled, 0, row, tip.Satoshis, outpointOf(current))
		l.addAction(t, compiled, 0, gen, row, "unproven", tx)
		derived, err := VerifySuccessor(compiled, current, testCells, ca.Rule110, tx.TxID().String(), tx.Bytes())
		if err != nil {
			t.Fatalf("building generation %d: %v", gen, err)
		}
		current = derived
	}

	rec, err := RecoverCell(context.Background(), l, compiled, tip, 1, testCells, ca.Rule110, 8)
	if err != nil {
		t.Fatalf("RecoverCell: %v", err)
	}
	if rec.Verdict != VerdictAdopt {
		t.Fatalf("verdict = %s, want adopt: %s", rec.Verdict, rec.Reason)
	}
	if len(rec.Steps) != 3 || rec.Tip.Generation != 3 {
		t.Fatalf("adopted %d step(s) ending at generation %d, want 3 ending at 3",
			len(rec.Steps), rec.Tip.Generation)
	}

	// The same walk with a lower bound must stop and say so rather than follow a
	// chain longer than this program can produce.
	rec, err = RecoverCell(context.Background(), l, compiled, tip, 1, testCells, ca.Rule110, 2)
	if err != nil {
		t.Fatalf("RecoverCell: %v", err)
	}
	if rec.Verdict != VerdictHalt {
		t.Fatalf("verdict = %s past the walk bound, want halt: %s", rec.Verdict, rec.Reason)
	}
}

// A link that does not spend the previous one breaks the walk. This is the
// repeated-row ambiguity again, one generation further in.
func TestWalkForwardRejectsABrokenLink(t *testing.T) {
	compiled, l, tip, seed := aGenesisTip(t)

	one := ca.Rule110.Step(seed)
	first := stepTx(t, compiled, 0, one, tip.Satoshis, outpointOf(tip))
	l.addAction(t, compiled, 0, 1, one, "unproven", first)

	// Generation 2 is well formed but spends something else entirely.
	two := ca.Rule110.Step(one)
	orphan := stepTx(t, compiled, 0, two, tip.Satoshis,
		transaction.Outpoint{Txid: *hashOf(t, strings.Repeat("ee", 32)), Index: 0})
	l.addAction(t, compiled, 0, 2, two, "unproven", orphan)

	rec, err := RecoverCell(context.Background(), l, compiled, tip, 1, testCells, ca.Rule110, 8)
	if err != nil {
		t.Fatalf("RecoverCell: %v", err)
	}
	if rec.Verdict != VerdictHalt {
		t.Fatalf("verdict = %s for a broken chain, want halt: %s", rec.Verdict, rec.Reason)
	}
	// What was verified before the break is still reported, so an operator can
	// see exactly how far the chain held.
	if len(rec.Steps) != 1 {
		t.Errorf("steps = %d, want the one link that did verify", len(rec.Steps))
	}
}

// An attempt that is not adjacent to the tip means the records themselves
// disagree, and nothing sensible can be concluded from them.
func TestNonAdjacentAttemptHalts(t *testing.T) {
	compiled, l, tip, _ := aGenesisTip(t)

	rec, err := RecoverCell(context.Background(), l, compiled, tip, 9, testCells, ca.Rule110, 8)
	if err != nil {
		t.Fatalf("RecoverCell: %v", err)
	}
	if rec.Verdict != VerdictHalt {
		t.Fatalf("verdict = %s for a non-adjacent attempt, want halt: %s", rec.Verdict, rec.Reason)
	}
}

// generationOf is what reads a cell and generation out of an output's custom
// instructions. It must refuse anything that does not name THIS cell: a
// transition's other outputs belong to the same action, and matching on the
// generation field alone would let one of them speak for the cell.
func TestGenerationOfRequiresTheCell(t *testing.T) {
	own, err := json.Marshal(map[string]any{"cell": 3, "generation": 42})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if gen, ok := generationOf(3, string(own)); !ok || gen != 42 {
		t.Errorf("generationOf(3, own) = (%d, %v), want (42, true)", gen, ok)
	}
	if _, ok := generationOf(4, string(own)); ok {
		t.Error("cell 4 read a generation out of cell 3's instructions")
	}
	for _, bad := range []string{"", "{}", `{"generation":1}`, `{"cell":3}`, "not json"} {
		if _, ok := generationOf(3, bad); ok {
			t.Errorf("generationOf accepted %q", bad)
		}
	}
}
