// Command spike is a throwaway probe for the Rúnar -> Bitcoin Script -> toolbox
// integration. It hand-builds a transaction shaped exactly like the one the
// arcade toolbox will produce (contract input + fuel input, continuation output
// + P2PKH change) and runs the real lock/unlock pair through the Script VM.
//
// If this passes, the covenant accepts the toolbox's transaction shape and the
// remaining work is plumbing. Delete once internal/contract exists.
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/icellan/runar/compilers/go/compiler"
	runar "github.com/icellan/runar/packages/runar-go"
)

const (
	ringSize  = 8   // cells
	ruleNum   = 110 // Wolfram rule
	cellSats  = 1000
	fuelSats  = 5000
	changeAmt = 3500
)

func main() {
	art := mustCompile()

	// Rule 110 on a ring of 8 with only cell 0 alive (row 0x01) gives 0x03:
	// cell 0 sees (l=0,c=1,r=0)->1 and cell 1 sees (l=0,c=0,r=1)->1.
	const rowNow, rowNext = "01", "03"

	// Prove the expectation independently rather than trusting the constant.
	if got := stepRing(0x01); got != 0x03 {
		log.Fatalf("reference step wrong: got %#02x want 0x03", got)
	}

	const cellIndex = 1
	now := bindCell(art, cellIndex, rowNow)
	next := bindCell(art, cellIndex, rowNext)

	lockNow := now.GetLockingScript()
	lockNext := next.GetLockingScript()

	// A stateful locking script decomposes as: codeScript | OP_RETURN | state.
	// _codePart is the codeScript ONLY — the OP_RETURN separator is not part
	// of it (see RunarContract.GetLockingScript).
	stateHex := runar.SerializeState(art.StateFields, map[string]interface{}{"row": rowNow})
	const opReturn = "6a"
	suffix := opReturn + stateHex
	if !strings.HasSuffix(lockNow, suffix) {
		log.Fatalf("locking script does not end with OP_RETURN|state\n  want suffix=%s\n  got tail   =%s",
			suffix, tail(lockNow, len(suffix)+20))
	}
	codePart := lockNow[:len(lockNow)-len(suffix)]

	// Locate OP_CODESEPARATOR and sanity-check the artifact's reported offset.
	codeSepIdx := codeSepOffset(art, lockNow)

	fmt.Printf("lock(now)  %d bytes\nlock(next) %d bytes\ncodePart   %d bytes\ncodeSep@   %d\n\n",
		len(lockNow)/2, len(lockNext)/2, len(codePart)/2, codeSepIdx)

	// Build a transaction shaped the way the toolbox will build it.
	changePKH := strings.Repeat("ab", 20)
	tx := buildTx(lockNext, changePKH)

	_, preimage, err := runar.ComputeOpPushTxWithCodeSep(tx.Hex(), 0, lockNow, cellSats, codeSepIdx)
	if err != nil {
		log.Fatalf("compute preimage: %v", err)
	}

	// Unlocking script layout for a stateful method, mirroring the SDK:
	//   _codePart | args | _changePKH _changeAmount | _newAmount | txPreimage
	// (no method selector: Step is the only public method; no opSig push:
	// the script derives the k=1 signature from the preimage itself).
	unlock := runar.EncodePushData(codePart) +
		runar.EncodePushData(rowNext) +
		runar.EncodePushData(changePKH) +
		runar.EncodeScriptInt(changeAmt) +
		runar.EncodeScriptInt(cellSats) +
		runar.EncodePushData(hex.EncodeToString(preimage))

	fmt.Printf("unlock     %d bytes\n\n", len(unlock)/2)

	run("CORRECT next row", tx, 0, unlock, lockNow, true)

	// Negative control: claim the wrong bit for this cell. Cell 1 must turn ON
	// (0x03 has bit 1 set); 0x01 leaves it off and must be rejected.
	badUnlock := runar.EncodePushData(codePart) +
		runar.EncodePushData("01") +
		runar.EncodePushData(changePKH) +
		runar.EncodeScriptInt(changeAmt) +
		runar.EncodeScriptInt(cellSats) +
		runar.EncodePushData(hex.EncodeToString(preimage))
	run("WRONG next row (must fail)", tx, 0, badUnlock, lockNow, false)

	if failures > 0 {
		log.Fatalf("%d case(s) behaved unexpectedly", failures)
	}
	fmt.Println("SPIKE PASSED: covenant accepts the toolbox transaction shape.")
}

// run verifies one input of tx with the go-sdk interpreter, supplying full
// transaction context. Rúnar's own ScriptVM cannot be used here: its VMOptions
// carries no tx/prevout, so any script containing OP_CHECKSIG — which every
// checkPreimage covenant does — fails with "tx and previous output must be
// supplied for checksig". This is the same call the toolbox makes in
// pkg/storage/verifiers.go before broadcasting.
func run(label string, tx *transaction.Transaction, inputIdx int, unlockHex, lockHex string, wantOK bool) {
	u, err := script.NewFromHex(unlockHex)
	if err != nil {
		log.Fatalf("%s: decode unlock: %v", label, err)
	}
	l, err := script.NewFromHex(lockHex)
	if err != nil {
		log.Fatalf("%s: decode lock: %v", label, err)
	}

	tx.Inputs[inputIdx].UnlockingScript = u
	src := &transaction.TransactionOutput{Satoshis: cellSats, LockingScript: l}
	tx.Inputs[inputIdx].SetSourceTxOutput(src)

	err = interpreter.NewEngine().Execute(
		interpreter.WithTx(tx, inputIdx, src),
		interpreter.WithForkID(),
		// Rúnar's OP_PUSH_TX preamble emits OP_2MUL, which BSV only re-enabled
		// in the Chronicle upgrade. WithAfterGenesis() alone is not enough.
		interpreter.WithAfterChronicle(),
	)

	got := err == nil
	status := "OK"
	if got != wantOK {
		status = "*** UNEXPECTED ***"
	}
	fmt.Printf("--- %s ---\n    accepted=%v (want %v) %s\n", label, got, wantOK, status)
	if err != nil {
		fmt.Printf("    err: %v\n", err)
	}
	fmt.Println()
	if got != wantOK {
		failures++
	}
}

var failures int

// buildTx assembles [contract input, fuel input] -> [continuation, change].
func buildTx(lockNextHex, changePKH string) *transaction.Transaction {
	tx := transaction.NewTransaction()

	addInput(tx, strings.Repeat("11", 32), 0)
	addInput(tx, strings.Repeat("22", 32), 1)

	contOut, err := script.NewFromHex(lockNextHex)
	if err != nil {
		log.Fatalf("parse continuation script: %v", err)
	}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: cellSats, LockingScript: contOut})

	chg, err := script.NewFromHex("76a914" + changePKH + "88ac")
	if err != nil {
		log.Fatalf("parse change script: %v", err)
	}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: changeAmt, LockingScript: chg})

	return tx
}

func addInput(tx *transaction.Transaction, txidHex string, vout uint32) {
	txid, err := chainHash(txidHex)
	if err != nil {
		log.Fatalf("txid: %v", err)
	}
	tx.AddInput(&transaction.TransactionInput{
		SourceTXID:       txid,
		SourceTxOutIndex: vout,
		SequenceNumber:   0xffffffff,
	})
}

func bindCell(art *runar.RunarArtifact, i int, rowHex string) *runar.RunarContract {
	n := ringSize
	li := (i + 1) % n
	ri := ((i-1)%n + n) % n
	return runar.NewRunarContract(art, []interface{}{
		rowHex,
		int64(li / 8), int64(1) << (li % 8),
		int64(i / 8), int64(1) << (i % 8),
		int64(ri / 8), int64(1) << (ri % 8),
		int64(n / 8),
		int64(ruleNum),
	})
}

func codeSepOffset(art *runar.RunarArtifact, lockHex string) int {
	raw, err := hex.DecodeString(lockHex)
	if err != nil {
		log.Fatalf("decode lock: %v", err)
	}
	idx := -1
	if len(art.CodeSeparatorIndices) > 0 {
		idx = art.CodeSeparatorIndices[0]
	} else if art.CodeSeparatorIndex != nil {
		idx = *art.CodeSeparatorIndex
	}
	if idx < 0 || idx >= len(raw) || raw[idx] != 0xab {
		log.Fatalf("artifact codesep offset %d does not point at OP_CODESEPARATOR (byte=%#x)",
			idx, raw[min(idx, len(raw)-1)])
	}
	return idx
}

// stepRing is the pure-Go reference for one Rule 110 generation on a ring.
func stepRing(row uint8) uint8 {
	var out uint8
	for i := range ringSize {
		l := (row >> ((i + 1) % ringSize)) & 1
		c := (row >> i) & 1
		r := (row >> (((i-1)%ringSize + ringSize) % ringSize)) & 1
		if (ruleNum>>(l*4+c*2+r))&1 == 1 {
			out |= 1 << i
		}
	}
	return out
}

func mustCompile() *runar.RunarArtifact {
	a, err := compiler.CompileFromSource("contracts/Cell.runar.go")
	if err != nil {
		log.Fatalf("compile: %v", err)
	}
	raw, err := compiler.ArtifactToJSON(a)
	if err != nil {
		log.Fatalf("artifact json: %v", err)
	}
	var out runar.RunarArtifact
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Fatalf("unmarshal artifact: %v", err)
	}
	return &out
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func chainHash(h string) (*chainhash.Hash, error) { return chainhash.NewHashFromHex(h) }
