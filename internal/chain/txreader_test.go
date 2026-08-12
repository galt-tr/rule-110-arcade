package chain

import (
	"encoding/hex"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"
)

// openTestReader points a TxReader at the fixture's wallet, with no arcade to
// fall back on: these tests are about the database, and a fallback that reached
// the network would make them slow, flaky and dependent on someone else's data.
func openTestReader(t *testing.T, w *walletFixture) *TxReader {
	t.Helper()
	cfg := w.cfg
	cfg.ArcadeURL = ""
	r, err := OpenTxReader(t.Context(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("open tx reader: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// storedTxID reads back the display txid of a transaction the fixture inserted.
//
// It hex-encodes the stored bytes deliberately, rather than being handed the
// string: the column holds the txid as BYTES, and going through the same
// conversion the reader has to make is what proves the two agree.
func (w *walletFixture) storedTxID(t *testing.T, txRow int64) string {
	t.Helper()
	var raw []byte
	err := w.db.QueryRowContext(t.Context(), w.q(
		`SELECT k.txid FROM known_txs k JOIN transactions t ON t.txid = k.txid
		  WHERE t.transaction_id = ?`), txRow).Scan(&raw)
	if err != nil {
		t.Fatalf("read txid for row %d: %v", txRow, err)
	}
	return hex.EncodeToString(raw)
}

// TestTxReaderReturnsTheTransactionTheWalletStored is the test that matters for
// the audit: the bytes it gets back must parse and hash to the txid it asked
// for, because everything the audit concludes rests on that.
//
// It runs against the TOOLBOX'S schema — perfprovider plus Migrate, the same
// calls chain.Open makes — for the same reason prune_test.go does: this is raw
// SQL against tables this program does not own, and a test against a schema we
// invented would pass while the real column names and types drifted away.
func TestTxReaderReturnsTheTransactionTheWalletStored(t *testing.T) {
	w := newWalletFixture(t, 8)
	row := w.insertTx(t, txSpec{mined: true, height: testTipHeight, outs: []outSpec{{basket: CellBasket}}})
	txid := w.storedTxID(t, row)

	raw, err := openTestReader(t, w).RawTx(t.Context(), txid)
	if err != nil {
		t.Fatalf("RawTx: %v", err)
	}
	tx, err := transaction.NewTransactionFromBytes(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := tx.TxID().String(); got != txid {
		t.Fatalf("asked for %s and got bytes hashing to %s", txid, got)
	}
}

// TestTxReaderRefusesToInventBytes covers the three ways there can be no answer.
// Each must be an error: a caller that took silence for "no such transaction"
// would go on to audit a chain it never saw.
func TestTxReaderRefusesToInventBytes(t *testing.T) {
	w := newWalletFixture(t, 8)
	row := w.insertTx(t, txSpec{mined: true, height: testTipHeight, outs: []outSpec{{basket: CellBasket}}})
	txid := w.storedTxID(t, row)
	reader := openTestReader(t, w)

	if _, err := reader.RawTx(t.Context(), strings.Repeat("ab", 32)); err == nil {
		t.Error("a transaction the wallet has never heard of returned bytes")
	}
	if _, err := reader.RawTx(t.Context(), "not-a-txid"); err == nil {
		t.Error("a malformed txid was accepted")
	}

	// Pruned: the row is still there, but `rule110 prune` has reclaimed its
	// payload. There is nothing to audit and saying so is the only honest answer.
	w.exec(t, `UPDATE known_txs SET raw_tx = NULL WHERE txid = ?`, mustTxIDBytes(t, txid))
	if _, err := reader.RawTx(t.Context(), txid); err == nil {
		t.Error("a pruned transaction returned bytes")
	}
}

func mustTxIDBytes(t *testing.T, txid string) []byte {
	t.Helper()
	b, err := hex.DecodeString(txid)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
