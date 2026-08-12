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
// Three kinds of cell are considered, and they are not the same problem.
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
// A cell halted by ANY rejection directly above its tip may be held down by a
// verdict about a parent it no longer has: the rejected transaction was built on
// an output that never existed, so its refusal was never about this tip. Reading
// the rejected transaction's own bytes settles it. chain.RecoverStaleRejection
// decides those, and only ever on positive proof that it spent something else.
//
// A rejection further up than the tip's own successor is still never recovered,
// exactly as before: that is a cascade, its newest message describes wreckage
// rather than the break, and a human decides. Derivation is where that line is
// drawn — CellPosition.Rejected is set only for a rejection at tip+1 — so it
// holds for both rejection paths at once. See noteRejection.
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
		case p.Rejected:
			rec, err = recoverRejected(ctx, l, oracle, compiled, d, store, p)
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

// recoverRejected decides one cell whose halt is a rejection sitting directly
// above its tip.
//
// Two repairs can apply to the same row and they are not interchangeable, so the
// order is argued rather than convenient.
//
// # The tip fix speaks first
//
// A UTXO_SPENT names the transaction that spent this cell's tip, and if that
// transaction verifies as ours the cell's TIP MOVES. Every later question is
// asked about the tip, so asking "was this rejection built against a parent the
// cell no longer has?" before the tip has been corrected is asking the right
// question about the wrong output.
//
// # Why a halt from it may still be overturned
//
// The two examine DIFFERENT transactions. The tip fix reads the transaction the
// message NAMES; the stale-rejection check reads the transaction that PRODUCED
// the message — the one the record marks failed at this generation. Arcade's
// "utxo already spent" is about one of that transaction's own inputs, so if none
// of its inputs is this cell's tip, the message was never evidence about the tip
// at all: it is about the fuel coin, or about the superseded parent it was built
// on. The tip fix halting on a candidate it could not verify says nothing against
// that, and RecoverSpentTip's own condition-4 halt says so in as many words.
//
// So a resume may override the tip fix's halt, and nothing else may: two halts
// report the tip fix's reason, which names the more specific uncertainty.
//
// # One decision per cell per pass
//
// A cell whose tip the fix ADOPTS is not then examined for a stale rejection,
// even though the live deployment's cell 2 needs both. The rejection that would
// matter then is the one above the NEW tip, and that is not the row derivation
// looked at: deciding about it here would mean deciding from a tip this pass has
// not written yet, against a row nothing has verified, with the adjacency test
// applied to a generation number rather than to what the store holds.
//
// So recovery CONVERGES instead. The next derivation reads the corrected tip out
// of the store and offers the next row up, once, with the same evidence and the
// same checks as every other cell. `rule110 recover -apply` run twice lands in
// the same place — and so does the engine on its own, from the other direction:
// an adopted cell is released, and the write-ahead record its worker writes for
// the next generation replaces the stale rejection row (cell_txs is keyed by
// generation and cell) before anything is broadcast.
func recoverRejected(ctx context.Context, l chain.Ledger, oracle chain.TxStatus,
	compiled *cellscript.Compiled, d *chain.Deployment, store *history.Store, p CellPosition,
) (chain.Recovery, error) {

	// The rejection is at tip+1 by construction: derivation flags a rejection only
	// when it sits directly above the tip (see noteRejection), and
	// chain.RecoverStaleRejection refuses to decide anything else — which is what
	// keeps this away from the cascaded cells.
	stale := func() (chain.Recovery, error) {
		return chain.RecoverStaleRejection(ctx, l, p.Tip, p.Tip.Generation+1,
			p.RejectionTxID, p.RejectionErr)
	}
	if !isSpentTip(p) {
		return stale()
	}

	rec, err := recoverSpentTip(ctx, l, oracle, compiled, d, store, p)
	if err != nil || rec.Verdict != chain.VerdictHalt {
		return rec, err
	}
	alt, err := stale()
	if err != nil || alt.Verdict != chain.VerdictResume {
		return rec, err
	}
	return alt, nil
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
		// Whatever halted this cell has been shown not to be about its tip, and the
		// record of it has to go: derivation reads these same rows, so leaving it
		// would halt the cell again at the next startup, and at every startup after
		// that.
		//
		// Which record depends on which question was answered, and the order
		// mirrors Recover's own switch exactly — an unresolved attempt outranks a
		// rejection — so the row that is retracted is always the one the decision
		// was made about.
		switch {
		case p.Unknown:
			// Nothing was ever signed, so the write-ahead record is a false alarm.
			if err := store.DeleteAttempt(ctx, p.Attempted, cell); err != nil {
				return p, err
			}
		case p.Rejected:
			// The rejected transaction was built on a parent this cell no longer
			// has, so its refusal was a verdict about that parent. tip+1 is where
			// derivation found it; see noteRejection.
			if err := store.DeleteRejection(ctx, p.Tip.Generation+1, cell, p.RejectionTxID); err != nil {
				return p, err
			}
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
		if p.Unknown || p.Rejected {
			candidates++
		}
	}
	if candidates == 0 {
		return positions
	}

	e.logger.WarnContext(ctx,
		"recovering cells whose tip is unknown, spent out from under them, or held down by a rejection",
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
