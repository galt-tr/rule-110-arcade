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
// Only cells marked Unknown are considered. A cell halted by a REJECTION is
// never recovered: the rejected transaction's output does not exist, so there is
// nothing to adopt, and the transaction it tried to spend is gone, so there is
// nothing to resume from either. That cell's chain has ended and a human decides
// what to do about it. See chain.RecoverCell.
//
// The caller must hold the writer lease. Recovery reads the wallet's actions and
// writes tips; another writer doing the same thing concurrently would have both
// of them adopt against a moving target.
func Recover(ctx context.Context, l chain.Ledger, compiled *cellscript.Compiled,
	d *chain.Deployment, store *history.Store, positions []CellPosition, apply bool,
) ([]CellPosition, []CellRecovery, error) {

	out := make([]CellPosition, len(positions))
	copy(out, positions)

	var decisions []CellRecovery
	for cell, p := range positions {
		if !p.Unknown {
			continue
		}
		rec, err := chain.RecoverCell(ctx, l, compiled, p.Tip, p.Attempted, d.Cells, d.Rule, tipDepth)
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
func (e *Engine) recoverUnknown(ctx context.Context, positions []CellPosition) []CellPosition {
	unknown := 0
	for _, p := range positions {
		if p.Unknown {
			unknown++
		}
	}
	if unknown == 0 {
		return positions
	}

	e.logger.WarnContext(ctx, "recovering cells whose tip is unknown", "cells", unknown)
	out, decisions, err := Recover(ctx, e.chain, e.compiled, e.deployment, e.store, positions, true)
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
