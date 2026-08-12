package cellscript

import (
	"strings"
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/dymurray/rule-110-arcade/internal/ca"
)

// verifierSpend builds a transaction every input of which verifies.
//
// It is not newSpend, and the difference is the funding input. newSpend leaves
// it unsigned, which is faithful to the moment AdvanceCell checks the covenant
// — before SignAction — and therefore useless for exercising a WHOLE-
// transaction verifier, which would always fail at input 1. Here the funding
// input spends an anyone-can-spend output instead, so the only thing that can
// fail is the covenant.
//
// Swapping that output changes nothing the covenant sees: BIP-143 commits to
// inputs by outpoint and sequence, and the scriptCode in the preimage is input
// 0's own.
//
// Deterministic, so calling it twice yields two distinct objects with identical
// bytes and therefore one txid — which is exactly the shape SignAction presents,
// verifying in processNewTx and again in broadcastOne after re-parsing the
// stored transaction.
func verifierSpend(t *testing.T, c *Compiled, cell int, cur, next ca.Row) *transaction.Transaction {
	t.Helper()

	lockNow, err := c.LockingScript(cell, cur)
	if err != nil {
		t.Fatal(err)
	}
	lockNext, err := c.LockingScript(cell, next)
	if err != nil {
		t.Fatal(err)
	}

	tx := transaction.NewTransaction()
	contractTxid, _ := chainhash.NewHashFromHex(strings.Repeat("11", 32))
	tx.AddInputWithOutput(&transaction.TransactionInput{
		SourceTXID: contractTxid, SourceTxOutIndex: 0,
		SequenceNumber: transaction.DefaultSequenceNumber,
	}, &transaction.TransactionOutput{
		Satoshis: testCellSats, LockingScript: script.NewFromBytes(lockNow),
	})

	// OP_TRUE, spendable with an empty unlocking script.
	fuelTxid, _ := chainhash.NewHashFromHex(strings.Repeat("22", 32))
	tx.AddInputWithOutput(&transaction.TransactionInput{
		SourceTXID: fuelTxid, SourceTxOutIndex: 0,
		SequenceNumber:  transaction.DefaultSequenceNumber,
		UnlockingScript: script.NewFromBytes(nil),
	}, &transaction.TransactionOutput{
		Satoshis: 5000, LockingScript: script.NewFromBytes([]byte{script.Op1}),
	})

	tx.AddOutput(&transaction.TransactionOutput{
		Satoshis: testCellSats, LockingScript: script.NewFromBytes(lockNext),
	})
	changeScript, _ := script.NewFromHex("76a914" + strings.Repeat("ab", 20) + "88ac")
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: testChangeAmt, LockingScript: changeScript})

	unlock, err := c.UnlockingScript(Spend{
		CellIndex: cell, CurrentRow: cur, NextRow: next,
		Tx: tx, InputIndex: 0, PrevSatoshis: testCellSats,
		ChangePKH: bytesRepeat(0xab, 20), ChangeAmount: testChangeAmt, NewAmount: testCellSats,
	})
	if err != nil {
		t.Fatal(err)
	}
	tx.Inputs[0].UnlockingScript = script.NewFromBytes(unlock)
	return tx
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func verifierFixture(t *testing.T, cells, cell int) (*Compiled, ca.Row, ca.Row) {
	t.Helper()
	c := mustCompile(t, cells)
	cur, err := ca.SeedSingle(cells)
	if err != nil {
		t.Fatal(err)
	}
	return c, cur, ca.Rule110.Step(cur)
}

// The whole point. SignAction verifies the same finished transaction twice, from
// two separately parsed objects, and the second must come out of the memo rather
// than the interpreter.
func TestVerifierRunsTheInterpreterOncePerTransaction(t *testing.T) {
	c, cur, next := verifierFixture(t, 16, 3)
	first, second := verifierSpend(t, c, 3, cur, next), verifierSpend(t, c, 3, cur, next)
	if *first.TxID() != *second.TxID() {
		t.Fatal("the fixture is not deterministic, so this proves nothing")
	}
	v := NewScriptsVerifier(true)

	okFirst, errFirst := v.VerifyScripts(t.Context(), first)
	okSecond, errSecond := v.VerifyScripts(t.Context(), second)

	if !okFirst || errFirst != nil {
		t.Fatalf("first verification failed: %v", errFirst)
	}
	if !okSecond || errSecond != nil {
		t.Fatalf("remembered verdict differs from the first: %v", errSecond)
	}
	if got := v.Executions(); got != 1 {
		t.Errorf("interpreter ran %d times, want 1 — the memo did not hit", got)
	}
}

// A refusal has to be remembered too. Caching only successes would leave the
// expensive case paying twice, and would make this verifier quietly different
// from the one it replaces.
func TestVerifierRemembersARefusal(t *testing.T) {
	c, cur, next := verifierFixture(t, 16, 3)
	tx := verifierSpend(t, c, 3, cur, next)
	broken := append([]byte(nil), tx.Inputs[0].UnlockingScript.Bytes()...)
	broken[len(broken)-1] ^= 0xff
	tx.Inputs[0].UnlockingScript = script.NewFromBytes(broken)

	v := NewScriptsVerifier(true)
	okFirst, errFirst := v.VerifyScripts(t.Context(), tx)
	okSecond, errSecond := v.VerifyScripts(t.Context(), tx)

	if okFirst || errFirst == nil {
		t.Fatal("a broken covenant verified")
	}
	if okSecond || errSecond == nil {
		t.Error("the remembered verdict for a broken covenant was not a refusal")
	}
	if errFirst.Error() != errSecond.Error() {
		t.Errorf("remembered a different error:\n%v\n%v", errFirst, errSecond)
	}
	if got := v.Executions(); got != 1 {
		t.Errorf("interpreter ran %d times, want 1", got)
	}
}

// Two different transactions must not share a verdict. Keying on anything
// coarser than the txid — the cell, say — would let one cell's refusal condemn
// another cell's perfectly good transition.
func TestVerifierDoesNotConfuseTransactions(t *testing.T) {
	c, cur, next := verifierFixture(t, 16, 3)
	a, b := verifierSpend(t, c, 3, cur, next), verifierSpend(t, c, 7, cur, next)

	v := NewScriptsVerifier(true)
	if _, err := v.VerifyScripts(t.Context(), a); err != nil {
		t.Fatalf("cell 3: %v", err)
	}
	if _, err := v.VerifyScripts(t.Context(), b); err != nil {
		t.Fatalf("cell 7: %v", err)
	}
	if got := v.Executions(); got != 2 {
		t.Errorf("interpreter ran %d times, want 2 — two distinct transactions", got)
	}
}

// The verifier this replaces checks EVERY input, and its callers rely on that:
// the wallet's own funding input is checked here too, not only the covenant. Our
// pre-broadcast VerifyInput deliberately checks input 0 alone, and a verifier
// that did the same would silently stop checking the wallet's spending.
func TestVerifierChecksEveryInputNotJustTheCovenant(t *testing.T) {
	c := mustCompile(t, 16)
	cur, _ := ca.SeedSingle(16)
	// newSpend's funding input is unsigned, which is what an unfinished
	// transaction looks like.
	sp := newSpend(t, c, 3, cur, ca.Rule110.Step(cur))

	if err := VerifyInput(sp.tx, 0); err != nil {
		t.Fatalf("the covenant input should verify on its own: %v", err)
	}
	ok, err := NewScriptsVerifier(true).VerifyScripts(t.Context(), sp.tx)

	if ok || err == nil {
		t.Fatal("a transaction with an unsigned funding input passed whole-transaction verification")
	}
	if !strings.Contains(err.Error(), "input 1") {
		t.Errorf("error does not name the offending input: %v", err)
	}
}

// The era flag is a copy of what storage's own verifier would apply, and
// supplying a custom verifier is exactly what turns that logic off. This is the
// guard that it is plumbed at all: a Rúnar covenant contains OP_2MUL, which
// Genesis-era rules reject, so the two settings must reach different verdicts on
// one transaction. If this stops failing, the era is being ignored.
func TestVerifierAppliesTheConfiguredEra(t *testing.T) {
	c, cur, next := verifierFixture(t, 16, 3)
	tx := verifierSpend(t, c, 3, cur, next)

	if ok, err := NewScriptsVerifier(true).VerifyScripts(t.Context(), tx); !ok || err != nil {
		t.Fatalf("fixture does not verify under Chronicle rules: %v", err)
	}
	if _, err := NewScriptsVerifier(false).VerifyScripts(t.Context(), tx); err == nil {
		t.Error("the covenant verified under Genesis rules; OP_2MUL should have been rejected, " +
			"so the era flag is not reaching the interpreter")
	}
}

// The memo must not grow without bound: an entry whose second read never comes
// would otherwise be held for the life of the process.
func TestVerifierCacheIsBounded(t *testing.T) {
	v := NewScriptsVerifier(true)
	for i := range verdictCacheSize + 50 {
		var txid chainhash.Hash
		txid[0], txid[1] = byte(i), byte(i>>8)
		v.remember(txid, nil)
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.seen) > verdictCacheSize {
		t.Errorf("cache holds %d entries, want at most %d", len(v.seen), verdictCacheSize)
	}
	if len(v.fifo) != len(v.seen) {
		t.Errorf("eviction queue (%d) and cache (%d) disagree, so keys are leaking",
			len(v.fifo), len(v.seen))
	}
}

// One verifier is shared by 128 cell workers and by the toolbox's own
// goroutines, so the memo itself has to be safe under concurrency. Exercised
// directly rather than through the interpreter: go-sdk's transaction type is not
// designed to be verified from several goroutines at once, and it never is here
// — each cell holds its own.
func TestVerifierMemoIsConcurrencySafe(t *testing.T) {
	v := NewScriptsVerifier(true)

	var wg sync.WaitGroup
	for g := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				var txid chainhash.Hash
				txid[0], txid[1] = byte(i), byte(g)
				if _, ok := v.lookup(txid); !ok {
					v.remember(txid, nil)
				}
			}
		}()
	}
	wg.Wait()

	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.fifo) != len(v.seen) {
		t.Errorf("eviction queue (%d) and cache (%d) disagree under concurrency",
			len(v.fifo), len(v.seen))
	}
}
