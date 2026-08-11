// Command sighashdbg compares the sighash implied by the preimage we build
// against the sighash the transaction actually commits to, under several
// candidate scriptCode choices. Whichever candidate matches tells us what the
// on-chain OP_PUSH_TX check is really binding.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	"github.com/icellan/runar/compilers/go/compiler"
	runar "github.com/icellan/runar/packages/runar-go"
)

const (
	cellSats  = 1000
	changeAmt = 3500
)

func main() {
	art := mustArtifact()
	c := runar.NewRunarContract(art, []interface{}{
		"01", int64(0), int64(4), int64(0), int64(2), int64(0), int64(1), int64(1), int64(110),
	})
	next := runar.NewRunarContract(art, []interface{}{
		"03", int64(0), int64(4), int64(0), int64(2), int64(0), int64(1), int64(1), int64(110),
	})
	lockNow := c.GetLockingScript()
	lockNext := next.GetLockingScript()

	full, _ := script.NewFromHex(lockNow)
	prevOut := &transaction.TransactionOutput{Satoshis: cellSats, LockingScript: full}

	tx := transaction.NewTransaction()
	txid, _ := chainhash.NewHashFromHex(strings.Repeat("11", 32))
	tx.AddInputWithOutput(&transaction.TransactionInput{
		SourceTXID:       txid,
		SourceTxOutIndex: 0,
		SequenceNumber:   transaction.DefaultSequenceNumber,
	}, prevOut)

	contScript, _ := script.NewFromHex(lockNext)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: cellSats, LockingScript: contScript})
	chg, _ := script.NewFromHex("76a914" + strings.Repeat("ab", 20) + "88ac")
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: changeAmt, LockingScript: chg})

	// Our preimage, via the SDK helper.
	_, pre, err := runar.ComputeOpPushTxWithCodeSep(tx.Hex(), 0, lockNow, cellSats, 1)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("preimage len      : %d\n", len(pre))
	fmt.Printf("hash256(preimage) : %s\n\n", hex.EncodeToString(hash256(pre)))

	// Candidate scriptCodes the transaction might actually be committing to.
	candidates := map[string]string{
		"full locking script": lockNow,
		"after codesep (1B)":  lockNow[4:],
		"after codesep (0B)":  lockNow[2:],
	}
	for name, sc := range candidates {
		s, err := script.NewFromHex(sc)
		if err != nil {
			fmt.Printf("%-20s: parse error %v\n", name, err)
			continue
		}
		tx.Inputs[0].SetSourceTxOutput(&transaction.TransactionOutput{
			Satoshis: cellSats, LockingScript: s,
		})
		h, err := tx.CalcInputSignatureHash(0, sighash.AllForkID)
		if err != nil {
			fmt.Printf("%-20s: %v\n", name, err)
			continue
		}
		fmt.Printf("%-20s: %s\n", name, hex.EncodeToString(h))
	}
}

func hash256(b []byte) []byte {
	a := sha256.Sum256(b)
	d := sha256.Sum256(a[:])
	return d[:]
}

func mustArtifact() *runar.RunarArtifact {
	a, err := compiler.CompileFromSource("contracts/Cell.runar.go")
	if err != nil {
		log.Fatal(err)
	}
	raw, err := compiler.ArtifactToJSON(a)
	if err != nil {
		log.Fatal(err)
	}
	var out runar.RunarArtifact
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Fatal(err)
	}
	return &out
}
