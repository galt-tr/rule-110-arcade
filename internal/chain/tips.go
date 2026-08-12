package chain

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
)

// DeriveTip rebuilds one cell's tip from a recorded position and the bytes of
// the transaction it names.
//
// The split it enforces is the point of the whole design. A cell's position —
// "cell c is at generation G on transaction T" — is mutable and lives in exactly
// one place, the history store. The BYTES of T are immutable and can come from
// anywhere, because a blob keyed by a txid cannot be stale: it hashes to T or it
// does not. Everything else (the row, the output index, the satoshis) is derived
// here and verified, so there is nothing left to record that could drift out of
// agreement with something else.
//
// The verification is not a formality. The covenant's locking script embeds the
// cell index and the whole row as constructor constants, so an output whose
// script equals compiled.LockingScript(cell, row) is PROVABLY this cell at this
// row — a wrong cell, a wrong generation or a wrong automaton all produce a
// different script. That check is what the state file never had: it recorded a
// txid, a vout and a row with nothing tying them together, which is how the live
// deployment came to hold one cell at generation 1279 while the rest sat at
// 806-837 and nothing noticed.
//
// raw must be the serialized transaction, not a BEEF.
func DeriveTip(compiled *cellscript.Compiled, cell int, gen uint64, row ca.Row,
	txid string, raw []byte) (CellChain, error) {

	tx, err := parseTip(cell, txid, raw)
	if err != nil {
		return CellChain{}, err
	}

	want, err := compiled.LockingScript(cell, row)
	if err != nil {
		return CellChain{}, err
	}

	// Exactly one match, or refuse. Two outputs carrying the same script would
	// make the choice of which to spend arbitrary, and an arbitrary choice here
	// spends a live UTXO.
	found := -1
	for i, out := range tx.Outputs {
		if out.LockingScript == nil || !bytes.Equal(out.LockingScript.Bytes(), want) {
			continue
		}
		if found >= 0 {
			return CellChain{}, fmt.Errorf(
				"chain: cell %d: transaction %s carries cell %d's generation-%d script at both output %d and %d",
				cell, txid, cell, gen, found, i)
		}
		found = i
	}
	if found < 0 {
		return CellChain{}, fmt.Errorf(
			"chain: cell %d: transaction %s has no output locking to cell %d at generation %d "+
				"(recorded position does not match the transaction it names)", cell, txid, cell, gen)
	}

	// Where the output sits is fixed by how it was created, so check it rather
	// than accepting whatever was found. Genesis creates one output per cell in
	// cell order; every transition puts its continuation at output 0 because the
	// covenant rebuilds it there.
	wantVout := uint32(0)
	if gen == 0 {
		wantVout = uint32(cell) //nolint:gosec // cell is bounded by the ring size
	}
	if uint32(found) != wantVout { //nolint:gosec // found indexes tx.Outputs
		return CellChain{}, fmt.Errorf(
			"chain: cell %d: generation-%d script is at output %d of %s, expected output %d",
			cell, gen, found, txid, wantVout)
	}

	return CellChain{
		Cell:       cell,
		TxID:       txid,
		Vout:       wantVout,
		Satoshis:   tx.Outputs[found].Satoshis,
		Generation: gen,
		RowHex:     row.Hex(),
		RawTxHex:   hex.EncodeToString(raw),
	}, nil
}

// VerifySuccessor checks that a candidate transaction really is the next step of
// this cell's chain, and returns the tip it would become.
//
// This is the check that decides whether a lost transition is adopted or the
// cell is left halted, so every clause is load-bearing:
//
//   - it hashes to the txid we were told to look at, so the bytes are the ones
//     the network saw;
//   - it SPENDS OUR TIP. Nothing else establishes that. The locking script alone
//     is not enough: Rule 110 on a finite ring must eventually cycle, so a row
//     recurs and so does the script that carries it, and a transaction from an
//     earlier pass around the cycle would satisfy a script-only check exactly.
//     Anchoring on the spent outpoint removes that ambiguity, and it is only
//     checkable from the raw transaction — ListActions does not expand inputs at
//     all. If the raw-tx fetch is ever dropped as an optimisation, this clause is
//     what fails first;
//   - its continuation carries this cell's script for the NEXT row, so it
//     advances the automaton correctly rather than merely spending our coin;
//   - it preserves the cell's value and has the two-output shape the covenant
//     reconstructs, so the chain it hands us is one we can go on spending.
//
// It says nothing about signatures. Adoption never needs to: the transaction
// being adopted is one the network has already judged, and re-verifying a
// covenant we did not build proves nothing about whether it was accepted.
func VerifySuccessor(compiled *cellscript.Compiled, tip CellChain, cells int,
	rule ca.Rule, txid string, raw []byte) (CellChain, error) {

	cell := tip.Cell
	tx, err := parseTip(cell, txid, raw)
	if err != nil {
		return CellChain{}, err
	}

	spendsTip := false
	for _, in := range tx.Inputs {
		if in.SourceTXID != nil && in.SourceTXID.String() == tip.TxID && in.SourceTxOutIndex == tip.Vout {
			spendsTip = true
			break
		}
	}
	if !spendsTip {
		return CellChain{}, fmt.Errorf(
			"chain: cell %d: transaction %s does not spend this cell's tip %s:%d, so it is not our successor "+
				"(a repeated row makes the locking script alone ambiguous)", cell, txid, tip.TxID, tip.Vout)
	}

	// The covenant reconstructs exactly two outputs — its continuation and one
	// P2PKH change — so anything else is not a transition this program built,
	// whatever else about it looks right.
	if len(tx.Outputs) != 2 {
		return CellChain{}, fmt.Errorf(
			"chain: cell %d: transaction %s has %d outputs; a cell transition has exactly 2 "+
				"(continuation + change)", cell, txid, len(tx.Outputs))
	}

	row, err := tip.Row(cells)
	if err != nil {
		return CellChain{}, fmt.Errorf("chain: cell %d: %w", cell, err)
	}
	next := rule.Step(row)
	want, err := compiled.LockingScript(cell, next)
	if err != nil {
		return CellChain{}, err
	}
	cont := tx.Outputs[0]
	if cont.LockingScript == nil || !bytes.Equal(cont.LockingScript.Bytes(), want) {
		return CellChain{}, fmt.Errorf(
			"chain: cell %d: transaction %s output 0 is not cell %d's script for generation %d",
			cell, txid, cell, tip.Generation+1)
	}
	if cont.Satoshis != tip.Satoshis {
		return CellChain{}, fmt.Errorf(
			"chain: cell %d: transaction %s continues with %d satoshis, but the tip carries %d; "+
				"a cell's value is constant across generations",
			cell, txid, cont.Satoshis, tip.Satoshis)
	}

	return CellChain{
		Cell:       cell,
		TxID:       txid,
		Vout:       0,
		Satoshis:   cont.Satoshis,
		Generation: tip.Generation + 1,
		RowHex:     next.Hex(),
		RawTxHex:   hex.EncodeToString(raw),
	}, nil
}

// parseTip decodes raw and asserts it hashes to txid.
//
// Every path into this package that turns bytes into a tip goes through here.
// The bytes arrive from a wallet lookup or from arcade, neither of which is
// authenticated, and the txid is the only thing we actually recorded — so
// checking that they agree is what makes the bytes trustworthy at all.
func parseTip(cell int, txid string, raw []byte) (*transaction.Transaction, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("chain: cell %d: no bytes for transaction %s", cell, txid)
	}
	tx, err := transaction.NewTransactionFromBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("chain: cell %d: parse transaction %s: %w", cell, txid, err)
	}
	if got := tx.TxID().String(); got != txid {
		return nil, fmt.Errorf(
			"chain: cell %d: bytes offered for %s actually hash to %s", cell, txid, got)
	}
	return tx, nil
}
