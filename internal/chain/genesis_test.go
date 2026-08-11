package chain

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// aTransaction returns a small well-formed transaction and its raw bytes.
func aTransaction(t *testing.T) (*transaction.Transaction, []byte) {
	t.Helper()
	// One input spending an arbitrary outpoint, one OP_TRUE output. The content
	// does not matter — only that it round-trips through the BEEF encoding.
	tx := transaction.NewTransaction()
	if err := tx.AddInputFrom(
		strings.Repeat("ab", 32), 0, "76a914"+strings.Repeat("11", 20)+"88ac", 1000, nil,
	); err != nil {
		t.Fatalf("AddInputFrom: %v", err)
	}
	lock, err := script.NewFromHex("76a914" + strings.Repeat("22", 20) + "88ac")
	if err != nil {
		t.Fatalf("locking script: %v", err)
	}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1, LockingScript: lock})
	return tx, tx.Bytes()
}

// TestTipBEEFCarriesOnlyTheTip is the regression guard for the mistake that made
// the state file 175 MB and the wallet database 12 GB: a cell is an unbroken
// self-spending chain, so storing its atomic BEEF stored every generation back
// to genesis and handed the whole thing back to CreateAction each step.
//
// The successor needs the tip transaction and nothing behind it, so the encoded
// BEEF must stay the size of one transaction no matter how deep the chain is.
func TestTipBEEFCarriesOnlyTheTip(t *testing.T) {
	tx, raw := aTransaction(t)
	chain := CellChain{Cell: 3, RawTxHex: hex.EncodeToString(raw)}

	beefBytes, err := chain.tipBEEF()
	if err != nil {
		t.Fatalf("tipBEEF: %v", err)
	}

	beef, err := transaction.NewBeefFromBytes(beefBytes)
	if err != nil {
		t.Fatalf("re-parse tip beef: %v", err)
	}
	if got := len(beef.Transactions); got != 1 {
		t.Errorf("tip beef carries %d transactions, want exactly 1 — ancestry is leaking back in", got)
	}
	if beef.FindTransactionByHash(tx.TxID()) == nil {
		t.Error("tip beef does not contain the tip transaction, so hydrateInputs would not find it")
	}
	// The whole point is that this does not grow with chain depth.
	if len(beefBytes) > len(raw)+64 {
		t.Errorf("tip beef is %d bytes around a %d-byte transaction; expected only framing overhead",
			len(beefBytes), len(raw))
	}
}

func TestTipBEEFRejectsAnEmptyTip(t *testing.T) {
	if _, err := (CellChain{Cell: 7}).tipBEEF(); err == nil {
		t.Fatal("a cell with no stored transaction must not silently produce an empty beef")
	}
}

// TestLoadStateMigratesLegacyBEEF covers automata written before RawTxHex
// existed: the tip transaction is the subject of its own atomic BEEF, so the
// conversion is lossless and the automaton keeps running rather than needing a
// fresh genesis.
func TestLoadStateMigratesLegacyBEEF(t *testing.T) {
	tx, raw := aTransaction(t)
	legacy, err := tx.AtomicBEEF(true)
	if err != nil {
		t.Fatalf("AtomicBEEF: %v", err)
	}

	s := &State{Chains: []CellChain{
		{Cell: 0, LegacyBEEFHex: hex.EncodeToString(legacy)},
		{Cell: 1, RawTxHex: hex.EncodeToString(raw), LegacyBEEFHex: "dead"},
	}}
	if err := s.migrateLegacyBEEF(); err != nil {
		t.Fatalf("migrateLegacyBEEF: %v", err)
	}

	if got := s.Chains[0].RawTxHex; got != hex.EncodeToString(raw) {
		t.Errorf("cell 0 raw tx = %q, want the tip transaction's bytes", got)
	}
	if s.Chains[1].RawTxHex != hex.EncodeToString(raw) {
		t.Error("cell 1 already had a raw tx and must be left alone")
	}
	for _, c := range s.Chains {
		if c.LegacyBEEFHex != "" {
			t.Errorf("cell %d still carries the legacy beef; it must be dropped, not kept alongside", c.Cell)
		}
	}

	// And the migrated field must not be written back out.
	encoded, err := json.Marshal(s.Chains[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), `"beef"`) {
		t.Errorf("migrated state still serializes a beef field: %s", encoded)
	}
}
