package cellscript

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/dymurray/rule-110-arcade/internal/ca"
)

const (
	testCellSats  = 1000
	testChangeAmt = 3500
)

// spendFixture wires one cell transition into a transaction shaped the way the
// arcade toolbox builds them: the contract input first, then a funding input,
// with the continuation output at vout 0 and P2PKH change at vout 1.
type spendFixture struct {
	tx       *transaction.Transaction
	prevOut  *transaction.TransactionOutput
	unlocked []byte
}

func newSpend(t *testing.T, c *Compiled, cell int, cur, next ca.Row) spendFixture {
	t.Helper()

	lockNow, err := c.LockingScript(cell, cur)
	if err != nil {
		t.Fatalf("locking script (current): %v", err)
	}
	lockNext, err := c.LockingScript(cell, next)
	if err != nil {
		t.Fatalf("locking script (next): %v", err)
	}

	prevScript := script.NewFromBytes(lockNow)
	prevOut := &transaction.TransactionOutput{Satoshis: testCellSats, LockingScript: prevScript}

	tx := transaction.NewTransaction()
	contractTxid, _ := chainhash.NewHashFromHex(strings.Repeat("11", 32))
	tx.AddInputWithOutput(&transaction.TransactionInput{
		SourceTXID:       contractTxid,
		SourceTxOutIndex: 0,
		SequenceNumber:   transaction.DefaultSequenceNumber,
	}, prevOut)

	// A funding ("fuel") input, as the toolbox would add to pay the fee.
	fuelTxid, _ := chainhash.NewHashFromHex(strings.Repeat("22", 32))
	fuelScript, _ := script.NewFromHex("76a914" + strings.Repeat("cd", 20) + "88ac")
	tx.AddInputWithOutput(&transaction.TransactionInput{
		SourceTXID:       fuelTxid,
		SourceTxOutIndex: 0,
		SequenceNumber:   transaction.DefaultSequenceNumber,
	}, &transaction.TransactionOutput{Satoshis: 5000, LockingScript: fuelScript})

	contScript := script.NewFromBytes(lockNext)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: testCellSats, LockingScript: contScript})

	changePKH := bytes.Repeat([]byte{0xab}, 20)
	changeScript, _ := script.NewFromHex("76a914" + strings.Repeat("ab", 20) + "88ac")
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: testChangeAmt, LockingScript: changeScript})

	unlock, err := c.UnlockingScript(Spend{
		CellIndex:    cell,
		CurrentRow:   cur,
		NextRow:      next,
		Tx:           tx,
		InputIndex:   0,
		PrevSatoshis: testCellSats,
		ChangePKH:    changePKH,
		ChangeAmount: testChangeAmt,
		NewAmount:    testCellSats,
	})
	if err != nil {
		t.Fatalf("unlocking script: %v", err)
	}

	us := script.NewFromBytes(unlock)
	tx.Inputs[0].UnlockingScript = us

	return spendFixture{tx: tx, prevOut: prevOut, unlocked: unlock}
}

func mustCompile(t *testing.T, cells int) *Compiled {
	t.Helper()
	c, err := Compile(cells, ca.Rule110)
	if err != nil {
		t.Fatalf("compile for %d cells: %v", cells, err)
	}
	return c
}

// TestEveryCellVerifiesItsOwnBit is the core guarantee: for a real generation,
// each of the N cells independently accepts the correct next row on chain.
func TestEveryCellVerifiesItsOwnBit(t *testing.T) {
	const cells = 16
	c := mustCompile(t, cells)

	cur, err := ca.SeedSingle(cells)
	if err != nil {
		t.Fatal(err)
	}
	next := ca.Rule110.Step(cur)

	for cell := range cells {
		fx := newSpend(t, c, cell, cur, next)
		if err := VerifyInput(fx.tx, 0); err != nil {
			t.Errorf("cell %d rejected the correct next row: %v", cell, err)
		}
	}
}

// TestWrongBitIsRejected is the more important half: a cell must refuse a next
// row whose bit at its own index is wrong. Without this the covenant proves
// nothing.
func TestWrongBitIsRejected(t *testing.T) {
	const cells = 16
	c := mustCompile(t, cells)

	cur, _ := ca.SeedSingle(cells)
	correct := ca.Rule110.Step(cur)

	for cell := range cells {
		// Flip only this cell's bit; every other bit stays correct.
		bad := correct.Clone()
		bad.Set(cell, !correct.Get(cell))

		fx := newSpend(t, c, cell, cur, bad)
		if err := VerifyInput(fx.tx, 0); err == nil {
			t.Errorf("cell %d ACCEPTED a wrong bit — the rule is not being enforced", cell)
		}
	}
}

// TestOtherCellsBitsAreNotChecked documents the sharding boundary honestly:
// a cell verifies its own bit and nothing else, so a next row that is wrong
// elsewhere still passes here. It is the conjunction of all N transactions
// that proves a full generation.
func TestOtherCellsBitsAreNotChecked(t *testing.T) {
	const cells = 16
	c := mustCompile(t, cells)

	cur, _ := ca.SeedSingle(cells)
	correct := ca.Rule110.Step(cur)

	const observer = 3
	victim := (observer + 5) % cells

	corrupted := correct.Clone()
	corrupted.Set(victim, !correct.Get(victim))

	fx := newSpend(t, c, observer, cur, corrupted)
	if err := VerifyInput(fx.tx, 0); err != nil {
		t.Fatalf("cell %d should not police cell %d's bit, but rejected: %v", observer, victim, err)
	}
}

// TestRingWrapOnChain checks the boundary cells against the chain, not just the
// Go reference: the wrap is encoded purely in baked constants, so this is the
// test that proves those constants are right.
func TestRingWrapOnChain(t *testing.T) {
	const cells = 16
	c := mustCompile(t, cells)

	// Only cell cells-1 alive. Cell 0 must see it as its right neighbour and
	// turn on: (l=0, c=0, r=1) -> 1.
	cur, _ := ca.NewRow(cells)
	cur.Set(cells-1, true)
	next := ca.Rule110.Step(cur)
	if !next.Get(0) {
		t.Fatalf("reference disagrees: cell 0 should be alive, row=%s", next)
	}

	for _, cell := range []int{0, cells - 1} {
		fx := newSpend(t, c, cell, cur, next)
		if err := VerifyInput(fx.tx, 0); err != nil {
			t.Errorf("boundary cell %d rejected the correct wrapped transition: %v", cell, err)
		}
	}
}

// TestFullSizeRing exercises the production configuration end to end.
func TestFullSizeRing(t *testing.T) {
	const cells = 128
	c := mustCompile(t, cells)

	cur, _ := ca.SeedSingle(cells)
	next := ca.Rule110.Step(cur)

	for _, cell := range []int{0, 1, 63, 64, 127} {
		fx := newSpend(t, c, cell, cur, next)
		if err := VerifyInput(fx.tx, 0); err != nil {
			t.Errorf("cell %d of %d rejected a correct transition: %v", cell, cells, err)
		}

		bad := next.Clone()
		bad.Set(cell, !next.Get(cell))
		fxBad := newSpend(t, c, cell, cur, bad)
		if err := VerifyInput(fxBad.tx, 0); err == nil {
			t.Errorf("cell %d of %d accepted a wrong bit", cell, cells)
		}
	}
}

// TestMultipleGenerations walks a chain of transitions, feeding each
// generation's row into the next, as the engine will.
func TestMultipleGenerations(t *testing.T) {
	const cells = 16
	c := mustCompile(t, cells)

	row, _ := ca.SeedSingle(cells)
	for gen := range 8 {
		next := ca.Rule110.Step(row)
		for cell := range cells {
			fx := newSpend(t, c, cell, row, next)
			if err := VerifyInput(fx.tx, 0); err != nil {
				t.Fatalf("generation %d cell %d rejected: %v", gen, cell, err)
			}
		}
		row = next
	}
}

// TestCodePartIsStableAcrossGenerations matters because the covenant rebuilds
// its continuation output from _codePart: if it drifted with the state, the
// chain would break after one step.
func TestCodePartIsStableAcrossGenerations(t *testing.T) {
	const cells = 16
	c := mustCompile(t, cells)

	row, _ := ca.SeedSingle(cells)
	first, err := c.CodePart(5, row)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		row = ca.Rule110.Step(row)
		got, err := c.CodePart(5, row)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, got) {
			t.Fatal("code part changed between generations")
		}
	}
}

// TestCellsHaveDistinctScripts confirms the per-cell binding actually differs —
// otherwise every cell would be checking the same bit.
func TestCellsHaveDistinctScripts(t *testing.T) {
	const cells = 16
	c := mustCompile(t, cells)
	row, _ := ca.SeedSingle(cells)

	seen := make(map[string]int, cells)
	for cell := range cells {
		cp, err := c.CodePart(cell, row)
		if err != nil {
			t.Fatal(err)
		}
		key := string(cp)
		if prev, dup := seen[key]; dup {
			t.Errorf("cells %d and %d compiled to identical code", prev, cell)
		}
		seen[key] = cell
	}
}

func TestCompileRejectsBadRingSizes(t *testing.T) {
	for _, n := range []int{0, -8, 7, 100} {
		if _, err := Compile(n, ca.Rule110); err == nil {
			t.Errorf("Compile(%d) should have failed", n)
		}
	}
}

func TestUnlockingScriptValidatesInputs(t *testing.T) {
	const cells = 16
	c := mustCompile(t, cells)
	row, _ := ca.SeedSingle(cells)

	if _, err := c.UnlockingScript(Spend{CellIndex: 0, CurrentRow: row, NextRow: row}); err == nil {
		t.Error("expected an error when no transaction is supplied")
	}

	tx := transaction.NewTransaction()
	if _, err := c.UnlockingScript(Spend{
		CellIndex: 0, CurrentRow: row, NextRow: row, Tx: tx, ChangePKH: []byte{1, 2, 3},
	}); err == nil {
		t.Error("expected an error for a malformed change pubkey hash")
	}
}
