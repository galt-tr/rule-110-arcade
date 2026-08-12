package chain

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdk "github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
)

// ErrNotBroadcast marks a failure that happened before anything could reach the
// network, so the cell's UTXO is certainly still unspent and the attempt can be
// retried — or retracted — without risking a double spend.
var ErrNotBroadcast = errors.New("chain: not broadcast")

// StepResult describes one completed cell transition.
type StepResult struct {
	Cell       int
	Generation uint64
	TxID       string
	RowHex     string

	// RawTxHex is this transaction alone, which is all the successor needs to
	// spend it. Deliberately not the atomic BEEF — see CellChain.RawTxHex.
	RawTxHex  string
	SizeBytes int
}

// AdvanceCell moves one cell forward a generation.
//
// This is the two-step BRC-100 flow, and it has to be two-step: the covenant
// binds the WHOLE spending transaction through its sighash preimage, so the
// unlocking script cannot be built until the wallet has chosen the funding
// inputs and change. CreateAction returns the assembled-but-unsigned
// transaction, we build the unlocking script against it, and SignAction
// completes it.
// It takes the cell's tip by value rather than the whole State: cells advance
// concurrently and independently, so reaching into shared state here would be a
// race waiting to happen. The caller reads the tip under its own lock.
func (c *Chain) AdvanceCell(
	ctx context.Context,
	compiled *cellscript.Compiled,
	tip CellChain,
	cells int,
	rule ca.Rule,
) (res *StepResult, err error) {
	// Signing is what broadcasts, so a caller cannot otherwise tell whether a
	// failure left anything on the network. Everything that fails before that
	// point is reported as ErrNotBroadcast, which lets the caller retract its
	// write-ahead record instead of treating the cell's tip as unknown.
	broadcast := false
	defer func() {
		if err != nil && !broadcast {
			err = fmt.Errorf("%w: %w", ErrNotBroadcast, err)
		}
	}()

	cell := tip.Cell
	if cell < 0 || cell >= cells {
		return nil, fmt.Errorf("chain: cell %d out of range", cell)
	}

	// Use the row THIS cell's UTXO carries, not the global row. They diverge
	// whenever cells sit at different generations, and the mismatch surfaces
	// only as OP_CHECKSIGVERIFY failing on a script that looks correct.
	currentRow, err := tip.Row(cells)
	if err != nil {
		return nil, fmt.Errorf("chain: cell %d: %w", cell, err)
	}

	// Derive the successor from this cell's own row so a skewed cell still
	// advances correctly rather than being dragged to another generation's row.
	nextRow := rule.Step(currentRow)

	nextLock, err := compiled.LockingScript(cell, nextRow)
	if err != nil {
		return nil, err
	}
	inputBEEF, err := tip.tipBEEF()
	if err != nil {
		return nil, err
	}
	txid, err := chainhash.NewHashFromHex(tip.TxID)
	if err != nil {
		return nil, fmt.Errorf("chain: parse tip txid for cell %d: %w", cell, err)
	}

	nextGen := tip.Generation + 1
	instructions, err := json.Marshal(map[string]any{"cell": cell, "generation": nextGen})
	if err != nil {
		return nil, fmt.Errorf("chain: encode custom instructions: %w", err)
	}

	no := false
	// Leaving the unlocking script unset is what selects the signable path: any
	// input without one turns CreateAction into a two-step action.
	args := sdk.CreateActionArgs{
		Description: fmt.Sprintf("rule110 cell %d generation %d", cell, nextGen),
		// The source transaction is NOT recovered automatically for a
		// caller-provided input; without this the assembler fails with
		// "every signable transaction input must have a source transaction".
		InputBEEF: inputBEEF,
		Inputs: []sdk.CreateActionInput{{
			Outpoint:              transaction.Outpoint{Txid: *txid, Index: tip.Vout},
			InputDescription:      fmt.Sprintf("rule110 cell %d generation %d", cell, tip.Generation),
			UnlockingScriptLength: c.unlockingScriptEstimate(compiled, cell, currentRow),
		}},
		Outputs: []sdk.CreateActionOutput{{
			LockingScript:      nextLock,
			Satoshis:           tip.Satoshis,
			OutputDescription:  fmt.Sprintf("rule110 cell %d generation %d", cell, nextGen),
			Basket:             CellBasket,
			CustomInstructions: string(instructions),
			Tags:               []string{"rule110", "step"},
		}},
		// The per-cell label is what makes a lost transition findable again.
		//
		// If this process dies between signing (which broadcasts) and recording,
		// the transaction exists on chain and nowhere in our record — and the
		// only public way back to it is ListActions, which filters by label and
		// nothing else. Labelling by cell turns "what did cell 34 actually do?"
		// into two paginated calls; without it, a lost tip is unrecoverable
		// through any supported API and the cell is gone for good.
		//
		// "step" was replaced rather than added to: it distinguished nothing, so
		// this costs no extra label rows.
		Labels: []string{"rule110", CellLabel(cell)},
		Options: &sdk.CreateActionOptions{
			// The continuation MUST be vout 0: the covenant rebuilds it there.
			RandomizeOutputs:       &no,
			SignAndProcess:         &no,
			AcceptDelayedBroadcast: &no,
		},
	}

	// Deliberately NOT wrapped in retryUntilFunded. A cell transition is one
	// step of a continuous pipeline, so a funding shortfall has to surface
	// immediately and let the caller decide: retry after a few milliseconds if
	// it is the fuel pool briefly draining, or stop the whole automaton if the
	// deployment is genuinely out of coin. Blocking here for minutes instead
	// wedges the cell — and with it the automaton's frontier — while reporting
	// nothing at all.
	created, err := c.Wallet.CreateAction(ctx, args, c.Config.Originator)
	if err != nil {
		return nil, fmt.Errorf("chain: create step action for cell %d: %w", cell, err)
	}
	if created.SignableTransaction == nil {
		return nil, fmt.Errorf("chain: cell %d: expected a signable transaction, got none "+
			"(did the input acquire an unlocking script?)", cell)
	}

	tx, err := transaction.NewTransactionFromBEEF(created.SignableTransaction.Tx)
	if err != nil {
		return nil, fmt.Errorf("chain: parse signable transaction for cell %d: %w", cell, err)
	}

	changePKH, changeAmount, err := singleChangeOutput(tx)
	if err != nil {
		return nil, fmt.Errorf("chain: cell %d: %w", cell, err)
	}

	unlock, err := compiled.UnlockingScript(cellscript.Spend{
		CellIndex:    cell,
		CurrentRow:   currentRow,
		NextRow:      nextRow,
		Tx:           tx,
		InputIndex:   0,
		PrevSatoshis: tip.Satoshis,
		ChangePKH:    changePKH,
		ChangeAmount: changeAmount,
		NewAmount:    tip.Satoshis,
	})
	if err != nil {
		return nil, err
	}

	// Verify locally before broadcasting, so a covenant failure is reported
	// against our own script rather than as an opaque arcade rejection.
	tx.Inputs[0].UnlockingScript = script.NewFromBytes(unlock)
	if err := cellscript.VerifyInput(tx, 0); err != nil {
		return nil, fmt.Errorf("chain: cell %d covenant rejected locally: %w", cell, err)
	}

	// Past this line the transaction may exist on the network whatever happens.
	broadcast = true
	signed, err := c.Wallet.SignAction(ctx, sdk.SignActionArgs{
		Reference: created.SignableTransaction.Reference,
		Spends:    map[uint32]sdk.SignActionSpend{0: {UnlockingScript: unlock}},
	}, c.Config.Originator)
	if err != nil {
		return nil, fmt.Errorf("chain: sign step action for cell %d: %w", cell, err)
	}

	finalTx, err := transaction.NewTransactionFromBEEF(signed.Tx)
	if err != nil {
		return nil, fmt.Errorf("chain: parse signed transaction for cell %d: %w", cell, err)
	}

	return &StepResult{
		Cell:       cell,
		Generation: nextGen,
		RowHex:     nextRow.Hex(),
		TxID:       signed.Txid.String(),
		RawTxHex:   hex.EncodeToString(finalTx.Bytes()),
		SizeBytes:  len(finalTx.Bytes()),
	}, nil
}

// singleChangeOutput finds the P2PKH change the covenant expects alongside its
// continuation output at vout 0.
//
// The compiled covenant reconstructs exactly two outputs — continuation and one
// P2PKH change — so anything else cannot satisfy it, and saying so here is far
// clearer than letting OP_CHECKSIGVERIFY fail later.
func singleChangeOutput(tx *transaction.Transaction) ([]byte, uint64, error) {
	if len(tx.Outputs) != 2 {
		return nil, 0, fmt.Errorf(
			"expected exactly 2 outputs (continuation + change), got %d: "+
				"the covenant reconstructs both and cannot match another shape", len(tx.Outputs))
	}
	out := tx.Outputs[1]
	pkh, err := p2pkhHash(out.LockingScript.Bytes())
	if err != nil {
		return nil, 0, fmt.Errorf("output 1 is not P2PKH change: %w", err)
	}
	return pkh, out.Satoshis, nil
}

// p2pkhHash extracts the 20-byte hash from OP_DUP OP_HASH160 <20> OP_EQUALVERIFY OP_CHECKSIG.
func p2pkhHash(s []byte) ([]byte, error) {
	const p2pkhLen = 25
	if len(s) != p2pkhLen || s[0] != script.OpDUP || s[1] != script.OpHASH160 ||
		s[2] != 20 || s[23] != script.OpEQUALVERIFY || s[24] != script.OpCHECKSIG {
		return nil, fmt.Errorf("script is not a standard P2PKH (%d bytes)", len(s))
	}
	return s[3:23], nil
}

// unlockingScriptEstimate sizes the unlocking script for fee calculation.
//
// It is only an estimate: the wallet uses it to compute the fee, and the real
// script is supplied later by SignAction. Deriving it from the actual code part
// and preimage geometry keeps the fee honest instead of padding a guess.
func (c *Chain) unlockingScriptEstimate(compiled *cellscript.Compiled, cell int, row ca.Row) uint32 {
	codePart, err := compiled.CodePart(cell, row)
	if err != nil {
		return 4096 // a safe over-estimate; only the fee is affected
	}
	lock, err := compiled.LockingScript(cell, row)
	if err != nil {
		return 4096
	}
	// BIP-143 preimage: 156 fixed bytes + the scriptCode (locking script after
	// the 2-byte OP_DUP OP_CODESEPARATOR prefix) and its length prefix.
	preimage := 156 + (len(lock) - 2) + 5
	// pushes: codePart, nextRow, changePKH, changeAmount, newAmount, preimage
	total := (len(codePart) + 5) + (len(row) + 2) + 22 + 6 + 6 + (preimage + 5)
	return uint32(total)
}

// CellLabel is the wallet label carrying a transition's cell.
//
// Zero-padded so the labels sort in cell order, which matters only for a human
// reading them, and fixed-width so a prefix match cannot confuse cell 3 with
// cell 30.
func CellLabel(cell int) string { return fmt.Sprintf("rule110-cell-%03d", cell) }
