package cellscript

import (
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/dymurray/rule-110-arcade/internal/ca"
)

// benchCells is the live ring size. The numbers below are per CELL, so a
// generation costs 128 of each — which is the arithmetic that matters when
// asking whether a rate is reachable.
const benchCells = 128

// BenchmarkLockingScriptCached and BenchmarkLockingScriptConstructed are the
// two halves of the code-part cache: what a locking script costs now, against
// what it cost when every call assembled the Rúnar contract.
//
// AdvanceCell provoked five constructions per cell per generation, so the gap
// between these two multiplied by 5 x 128 is what the cache removes from a
// generation.
func BenchmarkLockingScriptCached(b *testing.B) {
	c, row := benchFixture(b)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if _, err := c.LockingScript(i%benchCells, row); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLockingScriptConstructed(b *testing.B) {
	c, row := benchFixture(b)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if _, err := c.constructLockingScriptHex(i%benchCells, row); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkUnlockingScript is the rest of the per-cell script cost: two locking
// scripts and the BIP-143 preimage over the whole transaction.
func BenchmarkUnlockingScript(b *testing.B) {
	c, cur := benchFixture(b)
	next := c.rule.Step(cur)
	tx := benchTx(b, c, 0, cur, next)

	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.UnlockingScript(Spend{
			CellIndex: 0, CurrentRow: cur, NextRow: next,
			Tx: tx, InputIndex: 0, PrevSatoshis: testCellSats,
			ChangePKH: benchPKH(), ChangeAmount: testChangeAmt, NewAmount: testCellSats,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkVerifyInput is one run of the covenant through the script
// interpreter.
//
// It is measured on its own because a transition pays for THREE of these: ours
// before broadcasting, and the toolbox's twice more inside SignAction. Whether
// collapsing those to one is worth doing is a question about this number times
// 3 x 128.
func BenchmarkVerifyInput(b *testing.B) {
	c, cur := benchFixture(b)
	next := c.rule.Step(cur)
	tx := benchSpend(b, c, 0, cur, next)

	b.ReportAllocs()
	for b.Loop() {
		if err := VerifyInput(tx, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func benchFixture(b *testing.B) (*Compiled, ca.Row) {
	b.Helper()
	c, err := Compile(benchCells, ca.Rule110)
	if err != nil {
		b.Fatalf("compile: %v", err)
	}
	row, err := ca.SeedSingle(benchCells)
	if err != nil {
		b.Fatalf("seed: %v", err)
	}
	return c, row
}

func benchPKH() []byte {
	pkh := make([]byte, 20)
	for i := range pkh {
		pkh[i] = 0xab
	}
	return pkh
}

// benchTx builds the transaction shape without an unlocking script; benchSpend
// completes it. Split so the unlocking-script benchmark is not measuring its own
// setup.
func benchTx(b *testing.B, c *Compiled, cell int, cur, next ca.Row) *transaction.Transaction {
	b.Helper()

	lockNow, err := c.LockingScript(cell, cur)
	if err != nil {
		b.Fatal(err)
	}
	lockNext, err := c.LockingScript(cell, next)
	if err != nil {
		b.Fatal(err)
	}

	prevOut := &transaction.TransactionOutput{
		Satoshis: testCellSats, LockingScript: script.NewFromBytes(lockNow),
	}
	tx := transaction.NewTransaction()
	contractTxid, _ := chainhash.NewHashFromHex(strings.Repeat("11", 32))
	tx.AddInputWithOutput(&transaction.TransactionInput{
		SourceTXID: contractTxid, SourceTxOutIndex: 0,
		SequenceNumber: transaction.DefaultSequenceNumber,
	}, prevOut)

	fuelTxid, _ := chainhash.NewHashFromHex(strings.Repeat("22", 32))
	fuelScript, _ := script.NewFromHex("76a914" + strings.Repeat("cd", 20) + "88ac")
	tx.AddInputWithOutput(&transaction.TransactionInput{
		SourceTXID: fuelTxid, SourceTxOutIndex: 0,
		SequenceNumber: transaction.DefaultSequenceNumber,
	}, &transaction.TransactionOutput{Satoshis: 5000, LockingScript: fuelScript})

	tx.AddOutput(&transaction.TransactionOutput{
		Satoshis: testCellSats, LockingScript: script.NewFromBytes(lockNext),
	})
	changeScript, _ := script.NewFromHex("76a914" + strings.Repeat("ab", 20) + "88ac")
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: testChangeAmt, LockingScript: changeScript})
	return tx
}

func benchSpend(b *testing.B, c *Compiled, cell int, cur, next ca.Row) *transaction.Transaction {
	b.Helper()
	tx := benchTx(b, c, cell, cur, next)
	unlock, err := c.UnlockingScript(Spend{
		CellIndex: cell, CurrentRow: cur, NextRow: next,
		Tx: tx, InputIndex: 0, PrevSatoshis: testCellSats,
		ChangePKH: benchPKH(), ChangeAmount: testChangeAmt, NewAmount: testCellSats,
	})
	if err != nil {
		b.Fatal(err)
	}
	tx.Inputs[0].UnlockingScript = script.NewFromBytes(unlock)
	if err := VerifyInput(tx, 0); err != nil {
		b.Fatalf("fixture does not verify, so the benchmark would measure a failure path: %v", err)
	}
	return tx
}
