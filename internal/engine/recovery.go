package engine

import (
	"context"
	"fmt"

	"github.com/dymurray/rule-110-arcade/internal/cellscript"
	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// CellRecovery is one cell's recovery decision, with the cell it belongs to.
type CellRecovery struct {
	Cell int
	chain.Recovery
}

// Recover decides what to do with every cell whose tip is unknown, and — when
// apply is set — writes the decision to the history store.
//
// The decision and the act of applying it are the same code path either way, so
// `rule110 recover` (dry run) shows exactly what `run -auto-recover` would do.
// Splitting them into two implementations would mean the reviewed one and the
// executed one could differ, which for something that decides whether to re-spend
// a live UTXO is not a risk worth taking for tidiness.
//
// Two kinds of cell are considered, and they are not the same problem.
//
// A cell marked Unknown has an unresolved write-ahead record: something may have
// been broadcast without being recorded, and the wallet's own actions say what.
// chain.RecoverCell decides those.
//
// A cell halted by a UTXO_SPENT rejection directly above its tip is the case
// where not even the write-ahead record survived, so there is nothing in our
// database to reason from at all — but arcade's refusal NAMES the transaction
// that spent the tip, and that name is enough to verify one specific candidate
// from its own bytes. chain.RecoverSpentTip decides those.
//
// Every OTHER rejection is still never recovered, exactly as before: the
// rejected transaction's output does not exist, so there is nothing to adopt,
// and the output it tried to spend is gone, so there is nothing to resume from
// either. That cell's chain has ended and a human decides.
//
// The caller must hold the writer lease. Recovery reads the wallet's actions and
// writes tips; another writer doing the same thing concurrently would have both
// of them adopt against a moving target.
func Recover(ctx context.Context, l chain.Ledger, oracle chain.TxStatus, compiled *cellscript.Compiled,
	d *chain.Deployment, store *history.Store, positions []CellPosition, apply bool,
) ([]CellPosition, []CellRecovery, error) {

	out := make([]CellPosition, len(positions))
	copy(out, positions)

	var decisions []CellRecovery
	for cell, p := range positions {
		var rec chain.Recovery
		var err error
		switch {
		case p.Unknown:
			rec, err = chain.RecoverCell(ctx, l, compiled, p.Tip, p.Attempted, d.Cells, d.Rule, tipDepth)
		case isSpentTip(p):
			rec, err = recoverSpentTip(ctx, l, oracle, compiled, d, store, p)
		default:
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		decisions = append(decisions, CellRecovery{Cell: cell, Recovery: rec})
		if !apply {
			continue
		}
		applied, err := applyRecovery(ctx, store, cell, p, rec)
		if err != nil {
			return nil, nil, err
		}
		out[cell] = applied
	}
	return out, decisions, nil
}

// isSpentTip selects the cells whose halt might be a lost transition rather
// than a dead chain.
//
// A cell qualifies on the SHAPE of its halt alone — a rejection one generation
// above the tip whose reason is arcade's already-spent verdict. Nothing about
// the candidate is judged here; chain.RecoverSpentTip does all of that, so a
// cell that qualifies and then fails verification is REPORTED as a halt with a
// reason rather than quietly dropped. An operator who came to look at one
// specific cell must find it in the output either way.
func isSpentTip(p CellPosition) bool {
	return p.Rejected && chain.IsUTXOSpent(p.RejectionErr)
}

// recoverSpentTip gathers what chain.RecoverSpentTip cannot reach from where it
// lives, and asks it to decide.
//
// The row is stepped from the TIP's row rather than walked afresh from the seed.
// They are the same row: DeriveTips recomputes every tip's row from the seed and
// then proves it against the covenant script the transaction actually carries,
// so p.Tip.RowHex is a verified position on the seed's own sequence, and one
// Step from a verified row is the same evidence for a fraction of the work.
func recoverSpentTip(ctx context.Context, l chain.Ledger, oracle chain.TxStatus,
	compiled *cellscript.Compiled, d *chain.Deployment, store *history.Store, p CellPosition,
) (chain.Recovery, error) {

	row, err := p.Tip.Row(d.Cells)
	if err != nil {
		// Halt this cell rather than failing the whole run: one cell whose
		// recorded row is unreadable must not stop the other 127 from being
		// examined, and `rule110 recover` is the tool an operator reaches for
		// precisely when something is already wrong.
		return chain.Recovery{Verdict: chain.VerdictHalt, Reason: fmt.Sprintf(
			"cell %d: the tip's recorded row cannot be read, so the row its successor must carry "+
				"cannot be computed: %v", p.Tip.Cell, err)}, nil
	}
	return chain.RecoverSpentTip(ctx, l, oracle, compiled, p.Tip, p.RejectionErr, d.Rule.Step(row),
		func(ctx context.Context, txid string) (bool, error) { return store.HasTxID(ctx, txid) })
}

// applyRecovery writes one decision to the store and returns the cell's new
// position.
//
// Order matters. The adopted transactions are recorded BEFORE the cell is
// released, so a crash midway leaves the store ahead of where the cell was
// rather than behind it — and derivation reads the store, so being ahead is
// harmless where being behind re-spends.
func applyRecovery(ctx context.Context, store *history.Store, cell int,
	p CellPosition, rec chain.Recovery) (CellPosition, error) {

	switch rec.Verdict {
	case chain.VerdictAdopt:
		for _, step := range rec.Steps {
			if err := store.RecordGeneration(ctx, step.Generation, step.RowHex); err != nil {
				return p, err
			}
			// Recorded as broadcast, not seen or mined: this says the transaction
			// exists and was handed to the network, which is all we know. The
			// status stream and the reconciler settle the rest, and a transaction
			// that never actually reached the network is picked up by the wallet's
			// own resend path.
			//
			// For a UTXO_SPENT adoption this REPLACES the rejection at that
			// generation — cell_txs is keyed by (generation, cell), so the refused
			// re-spend and the transaction that actually holds the generation cannot
			// both be recorded. Replacing is the correct direction and the only
			// possible one: the row is supposed to say which transaction holds this
			// cell at this generation, and after verification we know that it is
			// this one and not the phantom. Leaving the rejection in place would
			// halt the cell again at the next startup, for a transaction proved not
			// to be its tip.
			if err := store.RecordTx(ctx, history.CellTx{
				Generation: step.Generation, Cell: cell,
				TxID: step.TxID, Status: history.StatusBroadcast,
			}); err != nil {
				return p, err
			}
		}
		return CellPosition{Tip: rec.Tip}, nil

	case chain.VerdictResume:
		// Nothing was ever signed, so the write-ahead record is a false alarm and
		// has to go — leaving it would halt the cell again at the next startup.
		if err := store.DeleteAttempt(ctx, p.Attempted, cell); err != nil {
			return p, err
		}
		return CellPosition{Tip: rec.Tip}, nil

	default:
		// Still halted, but now with a reason that says which kind of uncertainty
		// this is rather than the generic "tip is unknown".
		p.HaltReason = rec.Reason
		return p, nil
	}
}

// recoverUnknown is the engine's own use of Recover, under the lease it already
// holds. Failures leave the cells halted and are reported, never retried blindly.
//
// The candidate count must match Recover's own selection exactly. It used to
// count only Unknown cells; if it still did, a deployment whose ONLY damage is a
// UTXO_SPENT halt would skip recovery entirely and `run -auto-recover` would
// silently do less than the dry run said it would.
func (e *Engine) recoverUnknown(ctx context.Context, positions []CellPosition) []CellPosition {
	candidates := 0
	for _, p := range positions {
		if p.Unknown || isSpentTip(p) {
			candidates++
		}
	}
	if candidates == 0 {
		return positions
	}

	e.logger.WarnContext(ctx, "recovering cells whose tip is unknown or spent out from under them",
		"cells", candidates)
	out, decisions, err := Recover(ctx, e.chain, e.chain.Oracle, e.compiled, e.deployment, e.store,
		positions, true)
	if err != nil {
		e.logger.ErrorContext(ctx, "recovery failed; the affected cells stay halted", "err", err)
		return positions
	}
	for _, d := range decisions {
		switch d.Verdict {
		case chain.VerdictHalt:
			e.logger.WarnContext(ctx, "recovery halted a cell", "cell", d.Cell, "reason", d.Reason)
		default:
			e.logger.InfoContext(ctx, "recovered a cell",
				"cell", d.Cell, "verdict", d.Verdict.String(), "reason", d.Reason)
		}
	}
	return out
}

// FormatRecovery renders one decision for an operator.
func FormatRecovery(d CellRecovery) string {
	s := fmt.Sprintf("cell %3d  %-6s  %s", d.Cell, d.Verdict, d.Reason)
	for _, step := range d.Steps {
		s += fmt.Sprintf("\n            adopt generation %d = %s:%d (%d sat)",
			step.Generation, step.TxID, step.Vout, step.Satoshis)
	}
	return s
}
