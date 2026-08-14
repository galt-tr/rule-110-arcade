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

// RecoverOptions are the choices an operator makes about one recovery pass.
//
// A struct rather than two bool arguments because the two mean very different
// things — one is "write this to the database", the other is "consider a repair
// that is safe but unexplained" — and two adjacent booleans at a call site are
// exactly the shape that gets transposed by an editor and read past by a
// reviewer.
type RecoverOptions struct {
	// Apply writes the decisions to the history store. Without it the pass is a
	// dry run that decides everything and changes nothing.
	Apply bool

	// RetryRefused lets a cell rebuild a generation the network refused for a
	// reason nobody has explained. See chain.RetryRefusal.
	//
	// Off by default, and never set by the engine's own auto-recovery. Every
	// other repair here rests on evidence about what DID happen — a transaction
	// that verifies as ours, a parent the cell no longer has, a sentinel that
	// says nothing was signed. This one rests only on a refused transaction
	// spending nothing, which makes the retry safe without making it warranted,
	// and "safe but unwarranted, 128 times, unattended, right after a crash" is
	// not a decision this program should make for a human.
	RetryRefused bool

	// Wreckage is the cascade budget, from WreckageBudget. Zero means the caller
	// did not say, and is read as the floor rather than as "no wreckage allowed":
	// a zero budget would call every single rejection a cascade, which is the
	// most destructive possible reading of an unset field.
	Wreckage int
}

// wreckage is the budget this pass decides with.
func (o RecoverOptions) wreckage() int {
	if o.Wreckage <= 0 {
		return wreckageFloor
	}
	return o.Wreckage
}

// Recover decides what to do with every cell whose tip is unknown, and — when
// opts.Apply is set — writes the decision to the history store.
//
// The decision and the act of applying it are the same code path either way, so
// `rule110 recover` (dry run) shows exactly what `run -auto-recover` would do.
// Splitting them into two implementations would mean the reviewed one and the
// executed one could differ, which for something that decides whether to re-spend
// a live UTXO is not a risk worth taking for tidiness.
//
// Five kinds of cell are considered, and they are not the same problem.
//
// A cell marked Unknown has an unresolved write-ahead record: something may have
// been broadcast without being recorded, and the wallet's own actions say what.
// chain.RecoverCell decides those.
//
// A cell whose halting failure carries the "nothing was signed" sentinel and
// names no transaction never reached the network at all — a local error, most of
// them a saturated database inside CreateAction. chain.RecoverNotBroadcast
// decides those, on the two facts in the record itself.
//
// A cell halted by a UTXO_SPENT rejection is the case where not even the
// write-ahead record survived, so there is nothing in our database to reason from
// at all — but arcade's refusal NAMES the transaction that spent the tip, and
// that name is enough to verify one specific candidate from its own bytes.
// chain.RecoverSpentTip decides those.
//
// A cell halted by ANY rejection may be held down by a verdict about a parent it
// no longer has: the rejected transaction was built on an output that never
// existed, so its refusal was never about this tip. Reading the rejected
// transaction's own bytes settles it. chain.RecoverStaleRejection decides those,
// and only ever on positive proof that it spent something else.
//
// A cell halted by a refusal of a transaction that really did spend its tip, for
// a reason nobody has explained, may rebuild that generation — because a refused
// transaction spends nothing, so the retry cannot double spend. That one is
// opt-in and does the least: it proves the retry SAFE, not warranted. See
// chain.RetryRefusal and RecoverOptions.RetryRefused.
//
// # Which row each of them is about
//
// The record derivation halted on, wherever it sits — p.RejectedAt, p.RejectionErr
// and p.RejectionTxID are all read off that one row. It is usually tip+1 and used
// to be assumed to be, which is what stalled the repair: a pass that retracts a
// row leaves the next failure two or more generations above the tip, and every
// later pass then examined a generation nothing was recorded at.
//
// A CASCADE is still never recovered. That line is drawn once, in derivation, on
// how many rejections are stacked above the tip rather than on where the newest
// of them sits — see maxWreckage and noteRejection — so it holds for every
// rejection path here at once, and it holds after a repair has retracted the
// bottom row of the pile, which an adjacency test did not.
//
// The caller must hold the writer lease. Recovery reads the wallet's actions and
// writes tips; another writer doing the same thing concurrently would have both
// of them adopt against a moving target.
func Recover(ctx context.Context, l chain.Ledger, oracle chain.TxStatus, compiled *cellscript.Compiled,
	d *chain.Deployment, store *history.Store, positions []CellPosition, opts RecoverOptions,
) ([]CellPosition, []CellRecovery, error) {

	out := make([]CellPosition, len(positions))
	copy(out, positions)

	var decisions []CellRecovery
	for cell, p := range positions {
		rec, considered, err := decideCell(ctx, l, oracle, compiled, d, store, p, opts)
		if !considered {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		decisions = append(decisions, CellRecovery{Cell: cell, Recovery: rec})
		if !opts.Apply {
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

// decideCell is the whole of Recover's per-cell dispatch, extracted so the
// operator's pass and the engine's runtime repair reach the same decision by
// running the same code rather than by two implementations agreeing.
//
// considered is false for a cell with nothing to decide — a healthy cell, or one
// derivation withheld because its wreckage is a cascade (see maxWreckage).
func decideCell(ctx context.Context, l chain.Ledger, oracle chain.TxStatus, compiled *cellscript.Compiled,
	d *chain.Deployment, store *history.Store, p CellPosition, opts RecoverOptions,
) (rec chain.Recovery, considered bool, err error) {

	switch {
	case p.Unknown:
		rec, err = chain.RecoverCell(ctx, l, compiled, p.Tip, p.Attempted, d.Cells, d.Rule,
			tipWindow(opts.wreckage()))
	case p.Rejected:
		rec, err = recoverRejected(ctx, l, oracle, compiled, d, store, p, opts)
	default:
		return chain.Recovery{}, false, nil
	}
	return rec, true, err
}

// isSpentTip selects the cells whose halt might be a lost transition rather
// than a dead chain.
//
// A cell qualifies on the SHAPE of its halt alone — a rejection above the tip
// whose reason is arcade's already-spent verdict. Nothing about the candidate is
// judged here; chain.RecoverSpentTip does all of that, so a cell that qualifies
// and then fails verification is REPORTED as a halt with a reason rather than
// quietly dropped. An operator who came to look at one specific cell must find it
// in the output either way.
//
// Note that this repair is the one that does not care where the record sits.
// What it adopts is a transaction that spends THIS tip and carries the covenant
// for the row above it, so it is about generation tip+1 whichever row carried the
// message — and if it adopts, the next pass sees the remaining rejection one
// generation closer and decides about that.
func isSpentTip(p CellPosition) bool {
	return p.Rejected && chain.IsUTXOSpent(p.RejectionErr)
}

// isNeverBroadcast selects the cells whose halt is a failure of ours rather than
// a verdict of the network's.
//
// It selects on the recorded MESSAGE alone, exactly as isSpentTip does, and for
// the same reason: the second condition — that the row names no transaction — is
// judged inside chain.RecoverNotBroadcast, so a cell that has one is reported as
// a halt with a reason rather than silently falling through to a different
// repair. An operator who came to look at one specific cell must find it in the
// output whichever way it was decided.
func isNeverBroadcast(p CellPosition) bool {
	return p.Rejected && chain.IsNotBroadcast(p.RejectionErr)
}

// recoverRejected decides one cell whose halt is a failure sitting above its
// tip — the oldest such failure, which derivation identified and this does not
// go looking for again.
//
// Four repairs can apply to the same row and they are not interchangeable, so
// the order is argued rather than convenient.
//
// # Our own failure is decided first, and finally
//
// A row carrying the ErrNotBroadcast sentinel was written by THIS program about
// a transaction it never signed. That is the strongest evidence any of these
// paths has — it is a statement about our own code path rather than an inference
// from someone else's message — so it is asked first, and its answer stands.
//
// Standing is the part worth arguing. When it halts, it halts because the record
// contradicts itself: the sentinel says nothing was signed and a txid sits
// beside it saying something was. Falling through to a repair that reads that
// txid's bytes would be resolving a contradiction by picking the half that
// happens to be actionable. There is nothing to lose by stopping: the cell was
// already halted, and an operator now has a reason that names the contradiction.
//
// # The tip fix speaks first among the rest
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
// not written yet, against a row nothing has verified, with the size of the pile
// above it counted from a window that no longer describes the cell.
//
// So recovery CONVERGES instead. The next derivation reads the corrected tip out
// of the store and offers the next row up, once, with the same evidence and the
// same checks as every other cell. `rule110 recover -apply` run twice lands in
// the same place — and so does the engine on its own, from the other direction:
// an adopted cell is released, and the write-ahead record its worker writes for
// the next generation replaces the stale rejection row (cell_txs is keyed by
// generation and cell) before anything is broadcast.
//
// # The unexplained refusal is asked last, and only if asked for
//
// It is last because it is the only one that resumes without establishing what
// happened, so anything that CAN establish something must have had its say
// first — in particular the stale-rejection check, which resumes the same cell on
// stronger evidence and without a flag. It runs only when everything before it
// has halted, and only when the operator passed -retry-refused.
func recoverRejected(ctx context.Context, l chain.Ledger, oracle chain.TxStatus,
	compiled *cellscript.Compiled, d *chain.Deployment, store *history.Store, p CellPosition,
	opts RecoverOptions,
) (chain.Recovery, error) {

	// Every one of these is about the record derivation halted on, which is where
	// its generation, its txid and its message all come from together. Computing
	// tip+1 here instead is the bug this replaced: a pass that retracts a row
	// leaves the next failure two or more generations up, and cell 12 — tip 991,
	// failures at 993 and 994, nothing at 992 — then had every subsequent pass
	// examine an empty generation, decide there was nothing to do, and report
	// success while the cell stayed dead.
	//
	// What keeps this away from cells 34, 51, 64 and 91 is no longer this
	// arithmetic but the SIZE of the pile above their tips, counted in derivation:
	// p.Rejected is not set at all for a cascade, so those cells never reach here.
	// See engine.maxWreckage.
	gen := p.RejectedAt

	if isNeverBroadcast(p) {
		// Final either way; see above.
		return chain.RecoverNotBroadcast(p.Tip, gen, p.RejectionTxID, p.RejectionErr)
	}

	stale := func() (chain.Recovery, error) {
		return chain.RecoverStaleRejection(ctx, l, p.Tip, gen, p.RejectionTxID, p.RejectionErr)
	}

	// explained is every repair that establishes what actually happened, in the
	// order argued above.
	explained := func() (chain.Recovery, error) {
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

	rec, err := explained()
	if err != nil || rec.Verdict != chain.VerdictHalt || !opts.RetryRefused {
		return rec, err
	}

	// Everything that could explain this cell's halt has declined to. The retry
	// is offered on the one thing left that is certain — a refused transaction
	// spends nothing — and a halt from it keeps the earlier, more specific reason.
	retry, err := chain.RetryRefusal(ctx, l, p.Tip, gen, p.RejectionTxID, p.RejectionErr)
	if err != nil || retry.Verdict != chain.VerdictResume {
		return rec, err
	}
	return retry, nil
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
		//
		// Every generation below is one derivation READ OFF A ROW, never one
		// computed from the tip. A retraction is how a cell is released, so a
		// delete aimed at a generation nothing was verified at is either a no-op
		// that leaves the cell dead (which is what tip+1 became for cell 12) or, if
		// something were recorded there later, a row nobody decided about.
		switch {
		case p.Unknown:
			// Nothing was ever signed, so the write-ahead record is a false alarm.
			if err := store.DeleteAttempt(ctx, p.Attempted, cell); err != nil {
				return p, err
			}
		case p.Rejected && p.RejectionTxID == "":
			// A failure that names no transaction is one that was recorded before
			// anything was signed, and the only decision that resumes on such a row
			// is chain.RecoverNotBroadcast — every other path here halts on a record
			// with no transaction to read. So this branch cannot retract a row some
			// other repair reasoned about, and the store refuses to widen it: the
			// delete matches on `failed` AND on the txid being empty.
			if err := store.DeleteUnsignedRejection(ctx, p.RejectedAt, cell); err != nil {
				return p, err
			}
		case p.Rejected:
			// Either the rejected transaction was built on a parent this cell no
			// longer has, so its refusal was a verdict about that parent — or it was
			// refused for a reason nobody has explained, and a refused transaction
			// spends nothing. Both leave the tip where it is and both need the row
			// gone, keyed on the exact transaction that was examined AND at the
			// generation derivation found it at; see noteRejection.
			if err := store.DeleteRejection(ctx, p.RejectedAt, cell, p.RejectionTxID); err != nil {
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
		"recovering cells whose tip is unknown, spent out from under them, or held down by a failure",
		"cells", candidates)
	// RetryRefused is deliberately not offered here, not even as an Option. It is
	// the one repair that does not establish what happened, and the engine reaches
	// this path unattended on every acquisition of the lease — which is precisely
	// when nobody is watching. See RecoverOptions.
	out, decisions, err := Recover(ctx, e.ledger, e.oracle, e.compiled, e.deployment, e.store,
		positions, RecoverOptions{Apply: true})
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

// repairCell rebuilds one cell after a network refusal, on the cell's own worker
// goroutine and under the lease the engine already holds.
//
// # Why this may retry an unexplained refusal when recoverUnknown may not
//
// RecoverOptions.RetryRefused is documented as off by default and never set by
// the engine's own auto-recovery, and the argument there is sound: that path runs
// on every acquisition of the lease, over all 128 cells at once, unattended,
// right after a crash — which is precisely when nobody is watching and when the
// program knows least about why anything failed.
//
// This path is a different situation wearing the same words. It runs for ONE
// cell, in response to ONE observed refusal of a transition this process built
// and broadcast seconds earlier, on a cell that was demonstrably advancing until
// it was refused, and it is bounded: maxRetries consecutive refusals of the same
// generation and the cell halts for a human after all. The safety argument is
// unchanged and is the same one chain.RetryRefusal makes — a refused transaction
// spends nothing, so the tip is still unspent and rebuilding cannot double spend;
// and if the tip HAS been spent, the rebuild is refused as UTXO_SPENT naming the
// spender, which RecoverSpentTip then adopts.
//
// # The loop, and bug 9a
//
// A refusal usually leaves more than one dead row: everything the cell built on
// the refused output is doomed too, and each pass of the reviewed repair retracts
// exactly one row, deliberately, so that each row gets its own evidence.
// Iterating here clears the whole pile in one repair without inventing a bulk
// retraction that would need a new safety argument — and it must clear it, since
// derivation stops offering a cell to recovery at all once maxWreckage rejections
// are stacked above its tip.
//
// Every pass re-derives from the STORE. That is the bug 9a guard: the tip comes
// from the newest row the network actually accepted, verified against the
// covenant locking script by chain.DeriveTip, and never from the refused
// transaction's own output.
func (e *Engine) repairCell(ctx context.Context, cell int) {
	e.mu.Lock()
	delete(e.needsRepair, cell)
	e.mu.Unlock()

	// Bounded by what derivation will even offer: past the wreckage budget a cell
	// is a cascade and is withheld, so there is no pile this can usefully chase
	// further than that.
	//
	// The bound is the budget rather than a constant for the same reason the
	// budget is not one. A pass retracts a single row, so a pile of N needs N
	// passes; when this was fixed at 3 while the depth gate allowed 32 rows in
	// flight, a repair could not clear an ordinary pile even in principle.
	budget := e.wreckageBudget()
	for pass := 0; pass <= budget; pass++ {
		positions, err := DeriveTips(ctx, e.ledger, e.compiled, e.deployment, e.store, budget)
		if err != nil {
			e.haltRepair(ctx, cell, "re-derive the cell's tip: "+err.Error())
			return
		}
		if cell >= len(positions) {
			e.haltRepair(ctx, cell, "cell is outside the derived ring")
			return
		}
		p := positions[cell]

		rec, considered, err := decideCell(ctx, e.ledger, e.oracle, e.compiled, e.deployment,
			e.store, p, RecoverOptions{Apply: true, RetryRefused: true, Wreckage: budget})
		if err != nil {
			e.haltRepair(ctx, cell, "decide the repair: "+err.Error())
			return
		}
		if !considered {
			// Nothing left to repair: either the pile is gone and the cell can
			// advance, or derivation withheld it as a cascade and p.Halted says so.
			e.adoptRepaired(ctx, cell, p)
			return
		}
		if rec.Verdict == chain.VerdictHalt {
			e.haltRepair(ctx, cell, rec.Reason)
			return
		}

		applied, err := applyRecovery(ctx, e.store, cell, p, rec)
		if err != nil {
			e.haltRepair(ctx, cell, "apply the repair: "+err.Error())
			return
		}
		e.logger.InfoContext(ctx, "repaired a refused cell",
			"cell", cell, "pass", pass+1, "verdict", rec.Verdict.String(), "reason", rec.Reason)
		e.adoptRepaired(ctx, cell, applied)
	}
	e.haltRepair(ctx, cell, fmt.Sprintf(
		"still refused after %d repair passes; the wreckage above this cell's tip is not clearing",
		budget+1))
}

// wreckageBudget is the cascade budget this engine derives and repairs with.
//
// It comes from the depth gate because the gate is what decides how large a pile
// one rejection can legitimately leave: every transition the gate allows in
// flight is a transition that can be built on a verdict still in transit. See
// WreckageBudget.
func (e *Engine) wreckageBudget() int {
	return WreckageBudget(e.chain.Config.MaxUnseenDepth)
}

// adoptRepaired installs a repaired position and lets the cell run again.
func (e *Engine) adoptRepaired(ctx context.Context, cell int, p CellPosition) {
	e.mu.Lock()
	e.tips[cell] = p.Tip
	// Whether this halt is NEW, decided under the lock: a repair that re-derives
	// an already-halted cell must not re-announce it on every pass.
	newlyHalted := p.Halted && !e.halted[cell]
	if p.Halted {
		e.halted[cell] = true
		e.haltReason[cell] = p.HaltReason
	} else {
		delete(e.halted, cell)
		delete(e.haltReason, cell)
	}
	e.generation = e.frontierLocked()
	e.notify()
	e.mu.Unlock()

	// This was the ONLY path to a halted cell that said nothing at all. haltRepair
	// logs; derivation withholding a cell as a cascade came through here and set
	// the flag silently, so on 2026-08-13 four cells stopped with the reason
	// recorded in memory, absent from the log, and absent from /api/state — which
	// reports the COUNT of halted cells and no reason for any of them. An operator
	// had a number and nowhere to read why.
	if newlyHalted {
		e.logger.WarnContext(ctx, "cell halted by derivation; no repair was offered",
			"cell", cell, "tip_generation", p.Tip.Generation, "reason", p.HaltReason)
	}
}

// haltRepair stops a cell that could not be repaired, keeping the reason.
func (e *Engine) haltRepair(ctx context.Context, cell int, reason string) {
	e.logger.WarnContext(ctx, "cell halted; the refusal could not be repaired",
		"cell", cell, "reason", reason)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.halted[cell] = true
	e.haltReason[cell] = reason
	delete(e.needsRepair, cell)
	e.notify()
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
