package chain

import (
	"context"
	"encoding/json"
	"fmt"

	sdk "github.com/bsv-blockchain/go-sdk/wallet"
)

// Ledger is everything tip derivation and recovery need to ask of the wallet.
//
// It is a narrow interface rather than a *Chain because the two callers of it —
// DeriveTip's raw-transaction fetch and RecoverCell's evidence gathering — make
// decisions that spend or refuse to spend live UTXOs, and those decisions must
// be testable without a wallet, a database or a network. A fake backed by maps
// covers every branch, including the error branches, which are the ones that
// matter: see TestLedgerErrorNeverResumes.
//
// *Chain implements it.
type Ledger interface {
	// RawTx returns the bytes of txid. The caller MUST verify they hash to the
	// txid it asked for; this seam does not, because a fake that lies about that
	// is precisely what the tests need to build.
	//
	// A transaction the wallet has never heard of is an error, not an empty
	// result. Nothing downstream may treat "no bytes" as "no transaction".
	RawTx(ctx context.Context, txid string) ([]byte, error)

	// CellActions returns up to n of the newest wallet actions labelled for this
	// cell, oldest first, along with how many actions carry that label in total.
	//
	// The total is what distinguishes "this cell has never done anything" from
	// "this page happens to be empty", and recovery refuses to proceed without
	// it: a wallet restored from an old snapshot answers zero to both, and
	// treating that as "nothing was ever signed" is how a live tip gets
	// re-spent.
	CellActions(ctx context.Context, cell, n int) (actions []CellAction, total int, err error)
}

// CellAction is one wallet action belonging to a cell, reduced to the facts
// recovery reasons about.
type CellAction struct {
	// TxID is empty when the action was created but never signed.
	//
	// An unsigned action has no txid at all: storage inserts the row before the
	// transaction exists, and the wallet reports it as the zero hash. That
	// absence is the positive evidence that nothing reached the network — it is
	// not a lookup that failed, which is the distinction the whole recovery
	// design turns on.
	TxID string

	// Status is the wallet's own lifecycle state for the action: unsigned,
	// unprocessed, sending, unproven, completed, failed, nosend or nonfinal.
	// Recovery reads only "failed" and "unsigned" specifically; the rest are
	// treated alike, because the wallet's optimism about a transaction says
	// nothing about whether the network accepted it.
	Status string

	// Generation and HasGeneration come from the output's customInstructions,
	// which every cell output carries as {"cell":N,"generation":G}.
	//
	// Read from the instructions, NEVER from the action's position in the page:
	// the page is ordered by the wallet's internal row id, and an action that
	// failed to be created leaves a gap. An action that carries no readable
	// generation is unusable, not assumed to be the next one.
	Generation    uint64
	HasGeneration bool

	// LockingScript and Satoshis describe the continuation output — the one
	// whose customInstructions named this cell.
	LockingScript []byte
	Satoshis      uint64
}

// RawTx returns a transaction's bytes, from the wallet if it has them and from
// arcade if it does not.
//
// The wallet is the primary source and is expected to answer: processNewTx
// commits raw_tx in the same database transaction that records the txid, before
// broadcastOne touches the network, and SetProof keeps raw_tx forever
// afterwards. So a transaction this deployment signed has bytes here whether or
// not it was ever broadcast, mined or forgotten by anyone else.
//
// Arcade is a genuine fallback, not a formality — but an unproven one. It is
// not established that a production arcade returns rawTx for an old mined
// transaction, nor how long it retains records at all; the rejection path is
// documented to discard records on arcade's own schedule. Treat a hit here as
// luck and a miss as normal.
//
// If both miss, this fails. It does NOT return empty bytes: a caller that took
// silence for "no such transaction" would go on to rebuild a tip from a guess.
func (c *Chain) RawTx(ctx context.Context, txid string) ([]byte, error) {
	if c.provider != nil {
		raws, err := c.provider.RawTxs(ctx, []string{txid})
		if err != nil {
			return nil, fmt.Errorf("chain: read raw transaction %s from the wallet: %w", txid, err)
		}
		if raw, ok := raws[txid]; ok && len(raw) > 0 {
			return raw, nil
		}
	}

	rec, err := c.Oracle.GetTx(ctx, txid)
	if err != nil {
		return nil, fmt.Errorf("chain: the wallet has no bytes for %s and arcade lookup failed: %w", txid, err)
	}
	if rec == nil || len(rec.RawTx) == 0 {
		return nil, fmt.Errorf(
			"chain: no bytes for transaction %s: the wallet has no record of it and arcade returned none "+
				"(is this the wallet that built it?)", txid)
	}
	return rec.RawTx, nil
}

// cellActionPage is how many actions are read in one ListActions call. It is a
// page size, not a search depth: the caller asks for the newest n and this
// bounds a single request.
const cellActionPage = 64

// CellActions returns the newest n actions labelled for this cell.
//
// Pagination is by offset from the END, because ListActions orders by the
// wallet's ascending internal transaction id and offers no reverse order: with
// TotalActions = M, the newest n are at offset M-n. Asking for the total first
// costs one extra round trip and is what makes the second call bounded — the
// alternative is paging through every generation the cell has ever run.
func (c *Chain) CellActions(ctx context.Context, cell, n int) ([]CellAction, int, error) {
	if n <= 0 || n > cellActionPage {
		n = cellActionPage
	}
	label := CellLabel(cell)

	one := uint32(1)
	head, err := c.Wallet.ListActions(ctx, sdk.ListActionsArgs{
		Labels: []string{label},
		Limit:  &one,
	}, c.Config.Originator)
	if err != nil {
		return nil, 0, fmt.Errorf("chain: count actions for cell %d: %w", cell, err)
	}
	total := int(head.TotalActions)
	if total == 0 {
		return nil, 0, nil
	}

	offset := uint32(max(total-n, 0)) //nolint:gosec // bounded by total, which is a uint32
	limit := uint32(min(n, total))    //nolint:gosec // bounded by n <= cellActionPage
	yes := true
	page, err := c.Wallet.ListActions(ctx, sdk.ListActionsArgs{
		Labels:                      []string{label},
		Limit:                       &limit,
		Offset:                      &offset,
		IncludeOutputs:              &yes,
		IncludeOutputLockingScripts: &yes,
	}, c.Config.Originator)
	if err != nil {
		return nil, 0, fmt.Errorf("chain: list actions for cell %d: %w", cell, err)
	}

	out := make([]CellAction, 0, len(page.Actions))
	for i := range page.Actions {
		out = append(out, cellActionFrom(cell, &page.Actions[i]))
	}
	return out, total, nil
}

// zeroTxID is what the wallet reports for an action that has no txid.
//
// An unsigned action's row carries a NULL txid, and the mapping layer runs it
// through chainhash decoding, which pads an empty string to 32 zero bytes
// rather than failing. So "never signed" arrives as this value, not as an empty
// string — and a caller comparing against "" would read every unsigned action as
// a signed one with an absurd txid.
const zeroTxID = "0000000000000000000000000000000000000000000000000000000000000000"

// cellActionFrom reduces a wallet action to the facts recovery uses, taking the
// generation and the continuation output from the customInstructions we wrote
// when the output was created.
func cellActionFrom(cell int, a *sdk.Action) CellAction {
	out := CellAction{Status: string(a.Status)}
	if txid := a.Txid.String(); txid != zeroTxID {
		out.TxID = txid
	}
	for i := range a.Outputs {
		o := &a.Outputs[i]
		gen, ok := generationOf(cell, o.CustomInstructions)
		if !ok {
			continue
		}
		out.Generation, out.HasGeneration = gen, true
		out.LockingScript = o.LockingScript
		out.Satoshis = o.Satoshis
		break
	}
	return out
}

// cellInstructions is the customInstructions shape written on every cell output.
type cellInstructions struct {
	Cell       *int    `json:"cell"`
	Generation *uint64 `json:"generation"`
}

// generationOf reads an output's generation, and only if the instructions also
// name THIS cell.
//
// The cell check is not redundant with the label. A transition's change output
// belongs to the same action, and a future output shape could carry
// instructions of its own; matching on the generation field alone would let
// another output's number stand in for the cell's. An action self-identifies or
// it is not used.
func generationOf(cell int, instructions string) (uint64, bool) {
	if instructions == "" {
		return 0, false
	}
	var ci cellInstructions
	if err := json.Unmarshal([]byte(instructions), &ci); err != nil {
		return 0, false
	}
	if ci.Cell == nil || ci.Generation == nil || *ci.Cell != cell {
		return 0, false
	}
	return *ci.Generation, true
}
