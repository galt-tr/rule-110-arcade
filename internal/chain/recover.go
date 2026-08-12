package chain

import (
	"bytes"
	"context"
	"fmt"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
)

// Verdict is what to do with a cell whose tip is unknown.
type Verdict int

const (
	// VerdictHalt is the default and the safe one: leave the cell where it is
	// and tell a human why. It is deliberately the zero value, so a Recovery
	// that was never filled in cannot be mistaken for permission to spend.
	VerdictHalt Verdict = iota
	// VerdictResume means nothing was ever signed, so the recorded tip is still
	// unspent and the cell can carry on from it.
	VerdictResume
	// VerdictAdopt means a signed successor exists and has been verified; the
	// cell's tip moves to it without anything being re-spent.
	VerdictAdopt
)

func (v Verdict) String() string {
	switch v {
	case VerdictResume:
		return "resume"
	case VerdictAdopt:
		return "adopt"
	default:
		return "halt"
	}
}

// Recovery is the decision about one cell, as data.
//
// It is returned rather than acted on so the same code can drive a dry run and
// a real one. `rule110 recover` prints it; the engine applies it. There is no
// second implementation to disagree with the one that was reviewed.
type Recovery struct {
	Verdict Verdict

	// Tip is the cell's tip after recovery. Set for VerdictAdopt (the adopted
	// successor) and for VerdictResume (the tip that was passed in, unchanged).
	Tip CellChain

	// Reason is always set. This is what an operator reads, and for a halt it is
	// the only thing that says which of the many ways to be uncertain occurred.
	Reason string

	// Steps is the walk that was verified, oldest first, for the dry-run report.
	Steps []CellChain
}

// RecoverCell decides what to do with a cell that has an unresolved write-ahead
// record for generation `attempted`.
//
// # The evidence is positive, never an absence
//
// Signing broadcasts. So between "this output is now spent on chain" and "we
// recorded that" there is a window, and a process killed inside it comes back
// not knowing whether its tip is still unspent. There are exactly three
// distinguishable states, and each rests on something OBSERVED:
//
//	newest labelled action's generation < attempted   CreateAction never completed
//	action at `attempted` carrying no txid            the action exists, unsigned
//	action at `attempted` carrying a txid             a signed transaction exists
//
// Facts 1 and 2 come from the wallet writing its action row before signing, and
// 3 from processNewTx committing the txid and the raw bytes in one database
// transaction BEFORE broadcastOne touches the network. So a signed transition
// always leaves a durable row, written strictly earlier than the network could
// have seen it.
//
// # Why it is asymmetric
//
// Two errors are possible and they are not equally bad.
//
// ERROR A — adopt a successor that never actually reached the network. We do not
// re-spend the tip, so there is no double spend. The wallet's own resend path
// selects exactly this shape (was_broadcast=0, raw_tx present, status unsent or
// unprocessed) and the monitor rebroadcasts it. Worst case the next generation
// is rejected for a missing input and the cell halts, visibly.
//
// ERROR B — conclude "nothing was broadcast" and re-spend the tip when the
// successor was in fact on the network. Then we build on a phantom output, and
// the resulting rejection is indistinguishable from an ordinary one. That is not
// hypothetical: it is what killed cells 34 and 51 — ffb1c43e broadcast at
// 22:20:02, restart at 22:20:05, replacement rejected at 22:20:55.
//
// Error A costs a stalled cell. Error B destroys one. So: adopt whenever a
// signed transaction exists, and conclude "not broadcast" ONLY from the positive
// evidence above — never from a failed lookup, a timeout, an empty page or a
// 404. Every error path in here returns a halt.
//
// # The one assumption
//
// All of this presumes the transaction was built by THIS wallet database.
// Restoring an old wallet snapshot makes "nothing was signed" a lie, and the
// evidence would look identical. Hence the refusal below when a cell that
// history says is past generation 0 has no actions at all: that combination
// cannot happen in a wallet that ran this cell, so it means the wallet is not
// the one that did.
//
// # What is never recovered
//
// A rejection resolves the record to `failed`, so it is never an unresolved
// attempt and never reaches here. If the newest action at `attempted` carries a
// txid but the wallet has marked it failed, that is arcade having refused it:
// the tip stays where it is, the cell does not advance, and a human decides.
func RecoverCell(ctx context.Context, l Ledger, compiled *cellscript.Compiled, tip CellChain,
	attempted uint64, cells int, rule ca.Rule, maxWalk int) (Recovery, error) {

	cell := tip.Cell
	if attempted != tip.Generation+1 {
		return halt(fmt.Sprintf(
			"cell %d: the unresolved attempt is for generation %d but the recorded tip is at generation %d; "+
				"they must be adjacent, so the record itself is inconsistent",
			cell, attempted, tip.Generation)), nil
	}
	if maxWalk <= 0 {
		maxWalk = defaultMaxWalk
	}

	actions, total, err := l.CellActions(ctx, cell, maxWalk)
	if err != nil {
		// A failed lookup is not evidence of anything. See Error B.
		return halt(fmt.Sprintf("cell %d: could not read the wallet's actions, so nothing is known: %v",
			cell, err)), nil
	}
	if total == 0 {
		if tip.Generation > 0 {
			return halt(fmt.Sprintf(
				"cell %d: the wallet reports no actions at all, but this cell is recorded at generation %d — "+
					"so this is not the wallet that advanced it (restored from a snapshot?). "+
					"Recovering from here would treat live transactions as never signed",
				cell, tip.Generation)), nil
		}
		return Recovery{
			Verdict: VerdictResume, Tip: tip,
			Reason: fmt.Sprintf("cell %d: no action was ever created, so nothing was signed; "+
				"resuming from generation %d", cell, tip.Generation),
		}, nil
	}

	// The newest action carrying a readable generation is the only one that can
	// speak for the cell. Position in the page says nothing: a create that failed
	// leaves a gap, and an action with no cell output is not ours to interpret.
	newest, ok := newestCellAction(actions)
	if !ok {
		return halt(fmt.Sprintf(
			"cell %d: the wallet has %d actions with this cell's label but none carries a readable "+
				"cell/generation instruction, so none of them can be matched to a generation",
			cell, total)), nil
	}

	switch {
	case newest.Generation < attempted:
		// CreateAction never completed. The tip was never offered to anything.
		return Recovery{
			Verdict: VerdictResume, Tip: tip,
			Reason: fmt.Sprintf(
				"cell %d: the newest action is generation %d, below the attempted %d, so the attempt never "+
					"produced an action; resuming from generation %d",
				cell, newest.Generation, attempted, tip.Generation),
		}, nil

	case newest.Generation == attempted && newest.TxID == "":
		// The action exists but was never signed, so nothing was broadcast.
		return Recovery{
			Verdict: VerdictResume, Tip: tip,
			Reason: fmt.Sprintf(
				"cell %d: generation %d was created but never signed (the action carries no txid), "+
					"so nothing reached the network; resuming from generation %d",
				cell, attempted, tip.Generation),
		}, nil
	}

	// From here a signed transaction exists. Walk it forward: the attempt may
	// have been followed by further generations before the crash, and each link
	// must spend the previous one's adopted output.
	return walkForward(ctx, l, compiled, tip, actions, newest.Generation, cells, rule, maxWalk)
}

// defaultMaxWalk bounds how many generations recovery will chase forward.
//
// The unresolved attempt is written one generation ahead of the tip, so in the
// ordinary crash the walk is one link long. A longer run means several
// transitions were signed without any of them being recorded, which is not a
// shape this program produces — the write-ahead record is written before every
// one. So the bound is small on purpose: past it, the right answer is to stop
// and describe what was found rather than keep following a chain nobody
// understands.
const defaultMaxWalk = 8

// walkForward adopts the signed successors of tip, one generation at a time.
func walkForward(ctx context.Context, l Ledger, compiled *cellscript.Compiled, tip CellChain,
	actions []CellAction, newestGen uint64, cells int, rule ca.Rule, maxWalk int) (Recovery, error) {

	cell := tip.Cell
	byGen := make(map[uint64]CellAction, len(actions))
	for _, a := range actions {
		if a.HasGeneration {
			byGen[a.Generation] = a
		}
	}

	var steps []CellChain
	current := tip
	for gen := tip.Generation + 1; gen <= newestGen; gen++ {
		if len(steps) >= maxWalk {
			return Recovery{
				Verdict: VerdictHalt, Steps: steps,
				Reason: fmt.Sprintf(
					"cell %d: more than %d unrecorded generations to adopt (the wallet's newest is %d, "+
						"the record's tip is %d); stopping rather than following a chain this long unattended",
					cell, maxWalk, newestGen, tip.Generation),
			}, nil
		}

		a, ok := byGen[gen]
		if !ok {
			return Recovery{Verdict: VerdictHalt, Steps: steps, Reason: fmt.Sprintf(
				"cell %d: the wallet has an action for generation %d but none for %d in between, "+
					"so the chain from %d cannot be followed",
				cell, newestGen, gen, tip.Generation)}, nil
		}
		if a.TxID == "" {
			// An unsigned link stops the walk. Everything before it was adopted
			// and is safe; this generation was never signed, so the cell resumes
			// from what we have adopted so far.
			return resumeAfter(cell, current, steps, fmt.Sprintf(
				"generation %d was created but never signed", gen)), nil
		}
		if a.Status == statusFailed {
			// Arcade refused it. Do NOT adopt (its output does not exist) and do
			// NOT resume (the transaction it spent is gone). A human decides.
			return Recovery{Verdict: VerdictHalt, Steps: steps, Reason: fmt.Sprintf(
				"cell %d: generation %d (%s) is marked failed by the wallet — arcade rejected it, so this "+
					"cell's chain is broken at generation %d and cannot be continued automatically",
				cell, gen, a.TxID, gen)}, nil
		}

		raw, err := l.RawTx(ctx, a.TxID)
		if err != nil {
			return Recovery{Verdict: VerdictHalt, Steps: steps, Reason: fmt.Sprintf(
				"cell %d: generation %d was signed as %s but its bytes could not be read, so it cannot be "+
					"verified and must not be assumed unbroadcast: %v", cell, gen, a.TxID, err)}, nil
		}
		next, err := VerifySuccessor(compiled, current, cells, rule, a.TxID, raw)
		if err != nil {
			return Recovery{Verdict: VerdictHalt, Steps: steps, Reason: fmt.Sprintf(
				"cell %d: generation %d (%s) did not verify as this cell's successor: %v",
				cell, gen, a.TxID, err)}, nil
		}
		// Cross-check the wallet's own record of the output against the
		// transaction. VerifySuccessor has proved what the TRANSACTION contains;
		// this proves the wallet was building the same thing, so an action whose
		// instructions were written for one generation but whose transaction is
		// another cannot pass on a row that happens to recur.
		//
		// The script is only compared when the wallet returned one: a page
		// without expanded outputs is a thinner answer, not a wrong one.
		if err := agreesWithWallet(compiled, a, next, cells); err != nil {
			return Recovery{Verdict: VerdictHalt, Steps: steps, Reason: fmt.Sprintf(
				"cell %d: generation %d (%s) verified against the chain, but the wallet's own record of "+
					"its output disagrees: %v", cell, gen, a.TxID, err)}, nil
		}

		current = next
		steps = append(steps, next)
	}

	if len(steps) == 0 {
		return halt(fmt.Sprintf(
			"cell %d: nothing to adopt and no reason to resume; the wallet's newest generation is %d and "+
				"the record's tip is %d", cell, newestGen, tip.Generation)), nil
	}
	return Recovery{
		Verdict: VerdictAdopt, Tip: current, Steps: steps,
		Reason: fmt.Sprintf(
			"cell %d: adopting %d signed generation(s), tip moves %d -> %d (%s). Nothing is re-spent; "+
				"if any of these never reached the network the wallet's own resend path rebroadcasts it",
			cell, len(steps), tip.Generation, current.Generation, current.TxID),
	}, nil
}

// resumeAfter reports a walk that adopted some generations and then found an
// unsigned one. What was adopted stands; the cell resumes from there.
func resumeAfter(cell int, tip CellChain, steps []CellChain, why string) Recovery {
	v := VerdictAdopt
	if len(steps) == 0 {
		v = VerdictResume
	}
	return Recovery{
		Verdict: v, Tip: tip, Steps: steps,
		Reason: fmt.Sprintf("cell %d: %s, so nothing beyond generation %d reached the network; "+
			"tip is generation %d (%s)", cell, why, tip.Generation, tip.Generation, tip.TxID),
	}
}

// statusFailed is the wallet's own terminal status for an action it knows was
// refused. It is the one status recovery reads specifically: every other value
// describes the wallet's optimism about a transaction, which says nothing about
// whether the network accepted it.
const statusFailed = "failed"

// newestCellAction returns the action with the highest generation, read from
// the customInstructions rather than from the page order.
func newestCellAction(actions []CellAction) (CellAction, bool) {
	var newest CellAction
	found := false
	for _, a := range actions {
		if !a.HasGeneration {
			continue
		}
		if !found || a.Generation > newest.Generation {
			newest, found = a, true
		}
	}
	return newest, found
}

// agreesWithWallet checks the wallet's recorded output against the tip
// VerifySuccessor derived from the transaction itself.
func agreesWithWallet(compiled *cellscript.Compiled, a CellAction, tip CellChain, cells int) error {
	if a.Satoshis != tip.Satoshis {
		return fmt.Errorf("the wallet recorded %d satoshis, the transaction carries %d",
			a.Satoshis, tip.Satoshis)
	}
	if len(a.LockingScript) == 0 {
		return nil
	}
	row, err := tip.Row(cells)
	if err != nil {
		return err
	}
	want, err := compiled.LockingScript(tip.Cell, row)
	if err != nil {
		return err
	}
	if !bytes.Equal(a.LockingScript, want) {
		return fmt.Errorf("the wallet's recorded locking script is not cell %d's script for generation %d",
			tip.Cell, tip.Generation)
	}
	return nil
}

// halt builds the safe verdict. Every uncertain path in this file ends here.
func halt(reason string) Recovery { return Recovery{Verdict: VerdictHalt, Reason: reason} }
