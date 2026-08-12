package chain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"
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

// fakeOracle is arcade made of a map, so the "did the network take it?" branch
// can be driven to every answer including the ones a live arcade will not
// produce on demand: rejected, never heard of it, and unreachable.
type fakeOracle struct {
	status map[string]arcade.Status
	err    error
}

func (f fakeOracle) GetTx(_ context.Context, txid string) (*arcade.TxRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	st, ok := f.status[txid]
	if !ok {
		return nil, arcade.ErrTxNotFound
	}
	return &arcade.TxRecord{TxID: txid, Status: st}, nil
}

func minedOracle(txid string) fakeOracle {
	return fakeOracle{status: map[string]arcade.Status{txid: arcade.StatusMined}}
}

// liveRejection reproduces the message the live deployment actually recorded,
// doubled prefix and trailing input index included:
//
//	arcade: REJECTED: UTXO_SPENT (70): UTXO_SPENT (70): <outpoint> utxo already spent by tx <txid>[0]
func liveRejection(spent CellChain, by string) string {
	return fmt.Sprintf(
		"arcade: REJECTED: UTXO_SPENT (70): UTXO_SPENT (70): %s:%d utxo already spent by tx %s[0]",
		spent.TxID, spent.Vout, by)
}

// notOnRecord is the answer for a transaction this deployment has never
// recorded, which is what a LOST transition looks like.
func notOnRecord(context.Context, string) (bool, error) { return false, nil }

// aLostTransition sets up the damage class: cell 0's tip is generation 0, a
// generation-1 transition really was broadcast and mined, and nothing anywhere
// in our own record mentions it.
func aLostTransition(t *testing.T) (*cellscript.Compiled, *fakeLedger, CellChain, ca.Row, *transaction.Transaction) {
	t.Helper()
	compiled, l, tip, seed := aGenesisTip(t)
	next := ca.Rule110.Step(seed)
	lost := stepTx(t, compiled, 0, next, tip.Satoshis, outpointOf(tip))
	l.raw[lost.TxID().String()] = lost.Bytes()
	return compiled, l, tip, next, lost
}

// TestSpentTipAdoptsTheNamedTransition is the recovery this whole path exists
// for.
//
// The cell's record says generation 0. A transition to generation 1 was signed,
// broadcast and mined, and BOTH its write-ahead row and its broadcast row failed
// to persist, so our database has no trace of it at all. On restart the cell
// re-spends generation 0's output and arcade refuses — naming, in the refusal,
// the transaction that already spent it. That name is the only surviving pointer
// to where the cell really is, and following it re-spends nothing.
func TestSpentTipAdoptsTheNamedTransition(t *testing.T) {
	compiled, l, tip, next, lost := aLostTransition(t)
	txid := lost.TxID().String()

	rec, err := RecoverSpentTip(context.Background(), l, minedOracle(txid), compiled, tip,
		liveRejection(tip, txid), next, notOnRecord)
	if err != nil {
		t.Fatalf("RecoverSpentTip: %v", err)
	}
	if rec.Verdict != VerdictAdopt {
		t.Fatalf("verdict = %s, want adopt: %s", rec.Verdict, rec.Reason)
	}
	if rec.Tip.TxID != txid || rec.Tip.Generation != 1 || rec.Tip.Vout != 0 {
		t.Errorf("adopted tip = %+v, want generation 1 at output 0 of %s", rec.Tip, txid)
	}
	if rec.Tip.RowHex != next.Hex() {
		t.Errorf("adopted row = %s, want the recomputed %s", rec.Tip.RowHex, next.Hex())
	}
	if rec.Tip.Satoshis != tip.Satoshis {
		t.Errorf("adopted %d satoshis, want the tip's %d", rec.Tip.Satoshis, tip.Satoshis)
	}
	// Steps is what the applier writes and what the dry run prints, so an adopt
	// that carries no step would report a move it then fails to record.
	if len(rec.Steps) != 1 || rec.Steps[0].TxID != txid {
		t.Errorf("steps = %+v, want the single adopted generation", rec.Steps)
	}
}

// TestSpentTipHaltsOnEveryFailedCondition walks each of the six conditions and
// breaks exactly one of them.
//
// The asymmetry is the point. Staying halted costs a stalled cell and an
// operator's attention; adopting on incomplete evidence points a live chain at
// an outpoint nobody verified, which is unrecoverable. So every one of these
// must halt, and the reason must say WHICH condition failed — an operator
// reading "halt" with no discriminating reason cannot tell a foreign double
// spend from a transaction we simply could not fetch.
func TestSpentTipHaltsOnEveryFailedCondition(t *testing.T) {
	ctx := context.Background()

	// 1. The message does not parse as a UTXO_SPENT naming one transaction.
	t.Run("the rejection does not parse", func(t *testing.T) {
		compiled, l, tip, next, lost := aLostTransition(t)
		txid := lost.TxID().String()

		for name, rejection := range map[string]string{
			"another rejection entirely": "arcade: REJECTED: PROCESSING (4)",
			"no reason recorded":         "",
			"names nothing":              "arcade: REJECTED: UTXO_SPENT (70): utxo already spent",
			"a truncated txid":           "arcade: REJECTED: UTXO_SPENT (70): utxo already spent by tx " + txid[:60],
			"two different transactions": "arcade: REJECTED: UTXO_SPENT (70): utxo already spent by tx " + txid +
				" utxo already spent by tx " + strings.Repeat("ab", 32),
		} {
			t.Run(name, func(t *testing.T) {
				rec, err := RecoverSpentTip(ctx, l, minedOracle(txid), compiled, tip, rejection, next, notOnRecord)
				if err != nil {
					t.Fatalf("RecoverSpentTip: %v", err)
				}
				if rec.Verdict != VerdictHalt {
					t.Fatalf("verdict = %s for %q, want halt: %s", rec.Verdict, rejection, rec.Reason)
				}
				if !strings.Contains(rec.Reason, "does not parse") {
					t.Errorf("the reason should say the message did not parse, got: %s", rec.Reason)
				}
			})
		}
	})

	// 2. The named transaction is one we already hold.
	t.Run("the named transaction is already on our record", func(t *testing.T) {
		compiled, l, tip, next, lost := aLostTransition(t)
		txid := lost.TxID().String()
		onRecord := func(context.Context, string) (bool, error) { return true, nil }

		rec, err := RecoverSpentTip(ctx, l, minedOracle(txid), compiled, tip,
			liveRejection(tip, txid), next, onRecord)
		if err != nil {
			t.Fatalf("RecoverSpentTip: %v", err)
		}
		if rec.Verdict != VerdictHalt {
			t.Fatalf("verdict = %s for a transaction we already hold, want halt: %s", rec.Verdict, rec.Reason)
		}
		if !strings.Contains(rec.Reason, "IS on our") {
			t.Errorf("the reason should say we already hold it, got: %s", rec.Reason)
		}

		// A lookup that FAILS is not an answer of "no". Concluding "not ours" from
		// a database error would adopt on the strength of an outage.
		broken := func(context.Context, string) (bool, error) { return false, errors.New("connection reset") }
		rec, err = RecoverSpentTip(ctx, l, minedOracle(txid), compiled, tip,
			liveRejection(tip, txid), next, broken)
		if err != nil {
			t.Fatalf("RecoverSpentTip: %v", err)
		}
		if rec.Verdict != VerdictHalt {
			t.Fatalf("verdict = %s when the record could not be searched, want halt: %s",
				rec.Verdict, rec.Reason)
		}
	})

	// 3. The bytes do not hash to the name arcade gave us.
	t.Run("the bytes do not hash to the named transaction", func(t *testing.T) {
		compiled, l, tip, next, lost := aLostTransition(t)
		txid := lost.TxID().String()
		// Something else entirely, served under the name we asked for. Neither the
		// wallet nor arcade is authenticated, so this is the check that makes
		// fetched bytes evidence at all.
		other := stepTx(t, compiled, 0, next, tip.Satoshis+1, outpointOf(tip))
		l.raw[txid] = other.Bytes()

		rec, err := RecoverSpentTip(ctx, l, minedOracle(txid), compiled, tip,
			liveRejection(tip, txid), next, notOnRecord)
		if err != nil {
			t.Fatalf("RecoverSpentTip: %v", err)
		}
		if rec.Verdict != VerdictHalt {
			t.Fatalf("verdict = %s for bytes that hash elsewhere, want halt: %s", rec.Verdict, rec.Reason)
		}
		if !strings.Contains(rec.Reason, "hash to") {
			t.Errorf("the reason should name the hash mismatch, got: %s", rec.Reason)
		}

		// And bytes that cannot be fetched at all are not evidence of anything.
		l.rawErr = errors.New("timeout")
		rec, err = RecoverSpentTip(ctx, l, minedOracle(txid), compiled, tip,
			liveRejection(tip, txid), next, notOnRecord)
		if err != nil {
			t.Fatalf("RecoverSpentTip: %v", err)
		}
		if rec.Verdict != VerdictHalt {
			t.Fatalf("verdict = %s when the bytes could not be read, want halt: %s", rec.Verdict, rec.Reason)
		}
	})

	// 4. It does not spend THIS cell's tip.
	//
	// Rule 110 on a finite ring cycles, so a row recurs and the script carrying it
	// recurs with it: a transition from an earlier pass around the cycle passes
	// every script check exactly while spending an output consumed long ago. The
	// outpoint is the only thing that tells them apart.
	t.Run("it does not spend this cell's tip", func(t *testing.T) {
		compiled, l, tip, next, _ := aLostTransition(t)
		impostor := stepTx(t, compiled, 0, next, tip.Satoshis,
			transaction.Outpoint{Txid: *hashOf(t, strings.Repeat("cd", 32)), Index: 0})
		txid := impostor.TxID().String()
		l.raw[txid] = impostor.Bytes()

		rec, err := RecoverSpentTip(ctx, l, minedOracle(txid), compiled, tip,
			liveRejection(tip, txid), next, notOnRecord)
		if err != nil {
			t.Fatalf("RecoverSpentTip: %v", err)
		}
		if rec.Verdict != VerdictHalt {
			t.Fatalf("verdict = %s for a transaction that spends something else, want halt: %s",
				rec.Verdict, rec.Reason)
		}
		if !strings.Contains(rec.Reason, "inputs do not include") {
			t.Errorf("the reason should name the outpoint it fails to spend, got: %s", rec.Reason)
		}
	})

	// 5. Output 0 is not this cell's covenant for the next row.
	t.Run("output 0 carries the wrong covenant", func(t *testing.T) {
		compiled, l, tip, next, _ := aLostTransition(t)
		twoAhead := ca.Rule110.Step(next)

		for name, tx := range map[string]*transaction.Transaction{
			// Cell 5's covenant, spending cell 0's tip. A foreign spend of our
			// output that does not continue OUR chain is the end of this cell, not a
			// tip to adopt.
			"another cell": stepTx(t, compiled, 5, next, tip.Satoshis, outpointOf(tip)),
			// Our cell, our tip, but a row two generations on. Adopting it would
			// record a generation the automaton never passed through.
			"another row": stepTx(t, compiled, 0, twoAhead, tip.Satoshis, outpointOf(tip)),
		} {
			t.Run(name, func(t *testing.T) {
				txid := tx.TxID().String()
				l.raw[txid] = tx.Bytes()

				rec, err := RecoverSpentTip(ctx, l, minedOracle(txid), compiled, tip,
					liveRejection(tip, txid), next, notOnRecord)
				if err != nil {
					t.Fatalf("RecoverSpentTip: %v", err)
				}
				if rec.Verdict != VerdictHalt {
					t.Fatalf("verdict = %s, want halt: %s", rec.Verdict, rec.Reason)
				}
				if !strings.Contains(rec.Reason, "covenant") {
					t.Errorf("the reason should name the covenant mismatch, got: %s", rec.Reason)
				}
			})
		}
	})

	// 6. Arcade does not say the network took it.
	t.Run("arcade does not report it accepted", func(t *testing.T) {
		compiled, l, tip, next, lost := aLostTransition(t)
		txid := lost.TxID().String()

		oracles := map[string]TxStatus{
			"rejected":      fakeOracle{status: map[string]arcade.Status{txid: arcade.StatusRejected}},
			"double spend":  fakeOracle{status: map[string]arcade.Status{txid: arcade.StatusDoubleSpendAttempted}},
			"unknown":       fakeOracle{status: map[string]arcade.Status{txid: arcade.StatusUnknown}},
			"still in hand": fakeOracle{status: map[string]arcade.Status{txid: arcade.StatusReceived}},
			// A status this build has never heard of must halt, not adopt: the
			// accepted set is a whitelist precisely so that arcade adding a status
			// tomorrow cannot widen it.
			"a status we do not know": fakeOracle{status: map[string]arcade.Status{txid: "SOMETHING_NEW"}},
			"never heard of it":       fakeOracle{},
			"unreachable":             fakeOracle{err: errors.New("connection refused")},
			"no client at all":        nil,
		}
		for name, oracle := range oracles {
			t.Run(name, func(t *testing.T) {
				rec, err := RecoverSpentTip(ctx, l, oracle, compiled, tip,
					liveRejection(tip, txid), next, notOnRecord)
				if err != nil {
					t.Fatalf("RecoverSpentTip: %v", err)
				}
				if rec.Verdict != VerdictHalt {
					t.Fatalf("verdict = %s, want halt: %s", rec.Verdict, rec.Reason)
				}
			})
		}

		// The other accepted statuses must still adopt, or the check above could be
		// satisfied by a function that refuses everything.
		for _, st := range []arcade.Status{arcade.StatusAcceptedByNetwork, arcade.StatusSeenOnNetwork,
			arcade.StatusSeenMultipleNodes, arcade.StatusMined, arcade.StatusImmutable} {
			oracle := fakeOracle{status: map[string]arcade.Status{txid: st}}
			rec, err := RecoverSpentTip(ctx, l, oracle, compiled, tip,
				liveRejection(tip, txid), next, notOnRecord)
			if err != nil {
				t.Fatalf("RecoverSpentTip: %v", err)
			}
			if rec.Verdict != VerdictAdopt {
				t.Errorf("verdict = %s for arcade status %s, want adopt: %s", rec.Verdict, st, rec.Reason)
			}
		}
	})
}

// TestSpentByReadsTheDoubledPrefix covers the parser on its own, because the
// message it reads is the only surviving pointer to a lost tip and getting it
// wrong in either direction is expensive: a missed parse leaves a recoverable
// cell dead, and a loose one hands an unverified txid to the checks that follow.
//
// The live deployment doubles the prefix — arcade wraps a reason that already
// carries one — so nothing here anchors on the prefix at all.
func TestSpentByReadsTheDoubledPrefix(t *testing.T) {
	txid := strings.Repeat("b9", 32)

	good := map[string]string{
		"the live doubled prefix": "arcade: REJECTED: UTXO_SPENT (70): UTXO_SPENT (70): " +
			"8d3f0e:0 utxo already spent by tx " + txid,
		"a single prefix": "UTXO_SPENT (70): 8d3f0e:0 utxo already spent by tx " + txid,
		"with the input index appended": "UTXO_SPENT (70): UTXO_SPENT (70): 8d3f0e:0 " +
			"utxo already spent by tx " + txid + "[0]",
		"tripled, for good measure": "UTXO_SPENT (70): UTXO_SPENT (70): UTXO_SPENT (70): " +
			"utxo already spent by tx " + txid,
		"no numeric code": "UTXO_SPENT: utxo already spent by tx " + txid,
		"shouted":         strings.ToUpper("utxo_spent: utxo already spent by tx " + txid),
		// A wholly doubled message, not merely a doubled prefix: it still names
		// one transaction, so it is still unambiguous.
		"the same tx named twice": "UTXO_SPENT: utxo already spent by tx " + txid +
			"; UTXO_SPENT: utxo already spent by tx " + txid,
	}
	for name, msg := range good {
		t.Run(name, func(t *testing.T) {
			got, ok := SpentBy(msg)
			if !ok || got != txid {
				t.Errorf("SpentBy(%q) = (%q, %v), want (%q, true)", msg, got, ok, txid)
			}
			if !IsUTXOSpent(msg) {
				t.Error("the selection predicate missed a message that parses")
			}
		})
	}

	bad := map[string]string{
		"empty":                  "",
		"a different rejection":  "arcade: REJECTED: PROCESSING (4)",
		"the marker but no txid": "UTXO_SPENT (70): 8d3f0e:0 utxo already spent",
		"a truncated txid":       "UTXO_SPENT (70): utxo already spent by tx " + txid[:62],
		"an over-long txid":      "UTXO_SPENT (70): utxo already spent by tx " + txid + "ab",
		"not hex":                "UTXO_SPENT (70): utxo already spent by tx " + strings.Repeat("zz", 32),
		"the null hash":          "UTXO_SPENT (70): utxo already spent by tx " + zeroTxID,
		"two different transactions": "UTXO_SPENT: utxo already spent by tx " + txid +
			"; UTXO_SPENT: utxo already spent by tx " + strings.Repeat("ab", 32),
		// The phrase without the marker is some other message that happens to
		// mention a spend. Condition 1 is a UTXO_SPENT rejection specifically.
		"the phrase without the marker": "arcade: REJECTED: utxo already spent by tx " + txid,
	}
	for name, msg := range bad {
		t.Run(name, func(t *testing.T) {
			if got, ok := SpentBy(msg); ok {
				t.Errorf("SpentBy(%q) = (%q, true), want no parse — a message we do not understand "+
					"must halt the cell, not yield a guess", msg, got)
			}
		})
	}

	// The selection predicate is deliberately weaker than the parser: a message
	// that says UTXO_SPENT and names nothing readable must still be EXAMINED, so
	// the cell appears in `rule110 recover` as a halt with a reason instead of
	// vanishing from the report.
	unreadable := "UTXO_SPENT (70): 8d3f0e:0 utxo already spent"
	if !IsUTXOSpent(unreadable) {
		t.Error("a UTXO_SPENT whose txid cannot be read must still be offered for examination")
	}
	if IsUTXOSpent("arcade: REJECTED: PROCESSING (4)") {
		t.Error("an ordinary rejection must stay outside recovery entirely")
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
