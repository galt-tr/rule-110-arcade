package chain

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
)

// testCells is the ring the offline tests use. Small, because compiling the
// contract is the slowest thing here and the ring size changes nothing about
// what is being tested — the covenant binds one cell at a time.
const testCells = 8

func compileForTest(t *testing.T) *cellscript.Compiled {
	t.Helper()
	compiled, err := cellscript.Compile(testCells, ca.Rule110)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return compiled
}

func seedRow(t *testing.T) ca.Row {
	t.Helper()
	row, err := ca.SeedSingle(testCells)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return row
}

// anyP2PKH is a stand-in for the change output the covenant expects beside its
// continuation. Its contents are irrelevant to everything under test here; only
// its presence, and the resulting two-output shape, matter.
func anyP2PKH(t *testing.T) *script.Script {
	t.Helper()
	s, err := script.NewFromHex("76a914" + strings.Repeat("33", 20) + "88ac")
	if err != nil {
		t.Fatalf("p2pkh: %v", err)
	}
	return s
}

// genesisTx builds a transaction shaped like a real genesis: one output per
// cell, in cell order, all carrying the same row.
func genesisTx(t *testing.T, compiled *cellscript.Compiled, row ca.Row, sats uint64) *transaction.Transaction {
	t.Helper()
	tx := transaction.NewTransaction()
	tx.Inputs = append(tx.Inputs, &transaction.TransactionInput{
		SourceTXID: hashOf(t, strings.Repeat("f0", 32)), SourceTxOutIndex: 0,
	})
	for cell := range testCells {
		lock, err := compiled.LockingScript(cell, row)
		if err != nil {
			t.Fatalf("locking script: %v", err)
		}
		tx.AddOutput(&transaction.TransactionOutput{
			Satoshis: sats, LockingScript: script.NewFromBytes(lock),
		})
	}
	return tx
}

// stepTx builds a transaction shaped like a real cell transition: it spends
// `spends`, continues at output 0 with cell's script for `row`, and carries one
// change output.
//
// Signatures are deliberately absent. Nothing in derivation or adoption checks
// them — the transactions being examined are ones the network has already
// judged, and re-verifying a covenant we did not build proves nothing about
// whether it was accepted — so an unsigned transaction is a faithful stand-in.
func stepTx(t *testing.T, compiled *cellscript.Compiled, cell int, row ca.Row,
	sats uint64, spends transaction.Outpoint) *transaction.Transaction {
	t.Helper()
	lock, err := compiled.LockingScript(cell, row)
	if err != nil {
		t.Fatalf("locking script: %v", err)
	}
	tx := transaction.NewTransaction()
	tx.Inputs = append(tx.Inputs, &transaction.TransactionInput{
		SourceTXID: &spends.Txid, SourceTxOutIndex: spends.Index,
	})
	tx.AddOutput(&transaction.TransactionOutput{
		Satoshis: sats, LockingScript: script.NewFromBytes(lock),
	})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 999, LockingScript: anyP2PKH(t)})
	return tx
}

func hashOf(t *testing.T, hexStr string) *chainhash.Hash {
	t.Helper()
	h, err := chainhash.NewHashFromHex(hexStr)
	if err != nil {
		t.Fatalf("hash %q: %v", hexStr, err)
	}
	return h
}

func outpointOf(tip CellChain) transaction.Outpoint {
	h, _ := chainhash.NewHashFromHex(tip.TxID)
	return transaction.Outpoint{Txid: *h, Index: tip.Vout}
}

// TestDeriveTipFromRecordAndBytes is the whole premise: a txid, a generation and
// the transaction's bytes are enough to rebuild a tip, with the vout and the
// satoshis read off the transaction rather than recorded anywhere.
func TestDeriveTipFromRecordAndBytes(t *testing.T) {
	compiled := compileForTest(t)
	seed := seedRow(t)
	gen := genesisTx(t, compiled, seed, 7)

	for cell := range testCells {
		tip, err := DeriveTip(compiled, cell, 0, seed, gen.TxID().String(), gen.Bytes())
		if err != nil {
			t.Fatalf("cell %d: %v", cell, err)
		}
		if tip.Vout != uint32(cell) {
			t.Errorf("cell %d derived vout %d, want %d — genesis creates one output per cell in order",
				cell, tip.Vout, cell)
		}
		if tip.Satoshis != 7 {
			t.Errorf("cell %d derived %d satoshis, want the 7 the output actually carries", cell, tip.Satoshis)
		}
		if tip.RowHex != seed.Hex() {
			t.Errorf("cell %d derived row %s, want %s", cell, tip.RowHex, seed.Hex())
		}
	}
}

// The row is what the covenant binds, so deriving against the wrong generation
// must fail rather than produce a tip that looks fine and cannot be spent.
func TestDeriveTipRejectsTheWrongRow(t *testing.T) {
	compiled := compileForTest(t)
	seed := seedRow(t)
	gen := genesisTx(t, compiled, seed, 1)
	next := ca.Rule110.Step(seed)

	_, err := DeriveTip(compiled, 0, 1, next, gen.TxID().String(), gen.Bytes())
	if err == nil {
		t.Fatal("derived a tip against a row the transaction does not carry")
	}
}

// The covenant binds the cell index too, so cell 3's record must not derive
// against cell 5's output — which is the check the state file never had.
func TestDeriveTipRejectsTheWrongCell(t *testing.T) {
	compiled := compileForTest(t)
	seed := seedRow(t)

	// A transition transaction for cell 5: exactly two outputs, continuation
	// first. Deriving cell 3 from it must find no output of its own.
	tx := stepTx(t, compiled, 5, seed, 1, transaction.Outpoint{Txid: *hashOf(t, strings.Repeat("11", 32))})

	if _, err := DeriveTip(compiled, 3, 0, seed, tx.TxID().String(), tx.Bytes()); err == nil {
		t.Fatal("cell 3 derived a tip from cell 5's transaction")
	}
}

// Bytes that do not hash to the recorded txid are not the transaction we
// recorded, whatever else they contain. This is the property that makes fetching
// them from an unauthenticated source safe at all.
func TestDeriveTipRejectsMismatchedBytes(t *testing.T) {
	compiled := compileForTest(t)
	seed := seedRow(t)
	gen := genesisTx(t, compiled, seed, 1)

	wrongID := strings.Repeat("ab", 32)
	_, err := DeriveTip(compiled, 0, 0, seed, wrongID, gen.Bytes())
	if err == nil {
		t.Fatal("accepted bytes that hash to a different transaction")
	}
	if !strings.Contains(err.Error(), "hash to") {
		t.Errorf("error should name the hash mismatch, got: %v", err)
	}

	if _, err := DeriveTip(compiled, 0, 0, seed, gen.TxID().String(), nil); err == nil {
		t.Fatal("accepted an empty transaction body")
	}
}

// TestSuccessorNotSpendingOurTipIsRejected is one of the two properties this
// whole rework exists to guarantee.
//
// Rule 110 on a finite ring must eventually cycle, so a row recurs and the
// locking script that carries it recurs with it. A transaction from an earlier
// pass around that cycle therefore satisfies a script-only check EXACTLY — same
// cell, same script, same value — while spending a completely different output.
// Adopting it would move the tip to a UTXO that was consumed long ago, and every
// later generation would build on a phantom.
//
// The only thing that distinguishes them is which outpoint the candidate spends,
// and that is readable only from the raw transaction: ListActions does not
// expand inputs at all. If the raw-tx fetch is ever dropped as an optimisation,
// this is the test that fails.
func TestSuccessorNotSpendingOurTipIsRejected(t *testing.T) {
	compiled := compileForTest(t)
	seed := seedRow(t)
	next := ca.Rule110.Step(seed)
	gen := genesisTx(t, compiled, seed, 1)

	tip, err := DeriveTip(compiled, 0, 0, seed, gen.TxID().String(), gen.Bytes())
	if err != nil {
		t.Fatalf("derive tip: %v", err)
	}

	// A perfectly well-formed successor for cell 0 at the right row and the
	// right value — but spending somebody else's outpoint.
	impostor := stepTx(t, compiled, 0, next, tip.Satoshis,
		transaction.Outpoint{Txid: *hashOf(t, strings.Repeat("cd", 32)), Index: 0})

	_, err = VerifySuccessor(compiled, tip, testCells, ca.Rule110, impostor.TxID().String(), impostor.Bytes())
	if err == nil {
		t.Fatal("adopted a transaction that does not spend this cell's tip; " +
			"the locking script alone cannot distinguish a repeated row")
	}
	if !strings.Contains(err.Error(), "does not spend") {
		t.Errorf("error should name the unspent tip, got: %v", err)
	}

	// The genuine successor, spending our tip, must still be adopted — otherwise
	// the check above could be satisfied by refusing everything.
	real := stepTx(t, compiled, 0, next, tip.Satoshis, outpointOf(tip))
	got, err := VerifySuccessor(compiled, tip, testCells, ca.Rule110, real.TxID().String(), real.Bytes())
	if err != nil {
		t.Fatalf("rejected the genuine successor: %v", err)
	}
	if got.Generation != 1 || got.Vout != 0 || got.RowHex != next.Hex() {
		t.Errorf("adopted tip = %+v, want generation 1 at vout 0 carrying the next row", got)
	}
	if got.RawTxHex != hex.EncodeToString(real.Bytes()) {
		t.Error("the adopted tip must carry the transaction's own bytes")
	}
}

// A cell's value is constant across generations — fees come from the fuel pool,
// not from the cell — so a successor that changes it is not ours.
func TestSuccessorWithWrongSatoshisIsRejected(t *testing.T) {
	compiled := compileForTest(t)
	seed := seedRow(t)
	next := ca.Rule110.Step(seed)
	gen := genesisTx(t, compiled, seed, 1)

	tip, err := DeriveTip(compiled, 0, 0, seed, gen.TxID().String(), gen.Bytes())
	if err != nil {
		t.Fatalf("derive tip: %v", err)
	}
	tx := stepTx(t, compiled, 0, next, tip.Satoshis+1, outpointOf(tip))

	if _, err := VerifySuccessor(compiled, tip, testCells, ca.Rule110,
		tx.TxID().String(), tx.Bytes()); err == nil {
		t.Fatal("adopted a successor that changed the cell's value")
	}
}

// The covenant reconstructs exactly two outputs, so a third means this is not a
// transaction this program built — whatever else about it looks right.
func TestSuccessorWithThreeOutputsIsRejected(t *testing.T) {
	compiled := compileForTest(t)
	seed := seedRow(t)
	next := ca.Rule110.Step(seed)
	gen := genesisTx(t, compiled, seed, 1)

	tip, err := DeriveTip(compiled, 0, 0, seed, gen.TxID().String(), gen.Bytes())
	if err != nil {
		t.Fatalf("derive tip: %v", err)
	}
	tx := stepTx(t, compiled, 0, next, tip.Satoshis, outpointOf(tip))
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: anyP2PKH(t)})

	if _, err := VerifySuccessor(compiled, tip, testCells, ca.Rule110,
		tx.TxID().String(), tx.Bytes()); err == nil {
		t.Fatal("adopted a three-output transaction the covenant could never have produced")
	}
}

// A successor that advances the row by the wrong rule, or by two steps, is not
// this cell's next generation.
func TestSuccessorWithWrongNextRowIsRejected(t *testing.T) {
	compiled := compileForTest(t)
	seed := seedRow(t)
	twoAhead := ca.Rule110.Step(ca.Rule110.Step(seed))
	gen := genesisTx(t, compiled, seed, 1)

	tip, err := DeriveTip(compiled, 0, 0, seed, gen.TxID().String(), gen.Bytes())
	if err != nil {
		t.Fatalf("derive tip: %v", err)
	}
	tx := stepTx(t, compiled, 0, twoAhead, tip.Satoshis, outpointOf(tip))

	if _, err := VerifySuccessor(compiled, tip, testCells, ca.Rule110,
		tx.TxID().String(), tx.Bytes()); err == nil {
		t.Fatal("adopted a successor whose continuation is not the next row")
	}
}
