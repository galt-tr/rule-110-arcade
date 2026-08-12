package chain

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"

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

// TxStatus is arcade, narrowed to the one question adoption asks of it: what
// does the network say became of this transaction?
//
// Narrow and separate from Ledger for the same reason Ledger is narrow: the
// decision it feeds either moves a cell onto a live UTXO or leaves it halted,
// and both branches — including the ones reached only when arcade is
// unreachable or answers "never heard of it" — have to be testable without a
// network. *Chain's Oracle satisfies it, as does any arcade.TxOracle.
type TxStatus interface {
	GetTx(ctx context.Context, txid string) (*arcade.TxRecord, error)
}

// OnRecord answers whether a transaction id already appears in this
// deployment's own record of cell transactions.
//
// It is a callback rather than a plain bool because the txid being asked about
// is read out of a rejection MESSAGE, which only RecoverSpentTip parses — a
// caller that pre-computed the answer would have to parse the same string with
// the same rules and hope the two agree. And it is a callback rather than a
// store handle because internal/chain must not depend on internal/history: this
// package is the one that has to be exercisable with maps.
//
// An error MUST become a halt, never a "no". "The lookup failed" is not
// evidence that a transaction is not ours; see the asymmetry argument on
// RecoverCell.
type OnRecord func(ctx context.Context, txid string) (bool, error)

// utxoSpentMarker is arcade's own name for "an input of this transaction was
// already spent" — the wire-level reason code rather than prose, which is what
// makes it safe to key on. Held lower-cased because every match against it is
// made against a lower-cased copy of the message.
const utxoSpentMarker = "utxo_spent"

// spentByPhrase is the part of the rejection that names the conflicting spend.
// Everything before it is a prefix and is deliberately not parsed; see SpentBy.
const spentByPhrase = "utxo already spent by tx"

// IsUTXOSpent reports whether a recorded rejection is arcade's "already spent"
// verdict, whatever else the message does or does not contain.
//
// This is the SELECTION predicate, and it is deliberately weaker than SpentBy:
// a message that says UTXO_SPENT but whose txid cannot be read must still be
// EXAMINED, so that `rule110 recover` reports it as a halt with a reason rather
// than silently omitting the cell and leaving an operator wondering why the one
// cell they came to look at is not in the output.
//
// Every other rejection stays outside recovery entirely, exactly as before. A
// cell refused for a bad script, a low fee or a malformed transaction has no
// conflicting spend to find and nothing to adopt.
func IsUTXOSpent(rejection string) bool {
	return strings.Contains(strings.ToLower(rejection), utxoSpentMarker)
}

// SpentBy reports which transaction a UTXO_SPENT rejection blames.
//
// # Why the message is the evidence
//
// Arcade refuses a transaction whose input is already spent with a message that
// NAMES the transaction that spent it:
//
//	arcade: REJECTED: UTXO_SPENT (70): UTXO_SPENT (70): 8d3f…:0 utxo already spent by tx b9432ffe…
//
// For the damage class RecoverSpentTip exists to repair, that name is the only
// surviving pointer to the cell's real tip. The transition we lost the record
// of was signed, broadcast and mined; nothing in our own database mentions it;
// and the ONLY reason we know it exists at all is that the replacement we built
// was refused on account of it.
//
// # Parsing defensively
//
// The prefix is doubled on the live deployment — arcade wraps a reason that
// already carries its own prefix — so any parser that anchors on
// "UTXO_SPENT (70): " and takes the remainder is reading the second copy of the
// prefix as the body. This anchors on the prefix not at all. It requires the
// marker to appear SOMEWHERE (once, twice or ten times, with or without the
// numeric code) and reads the txid from the phrase that actually names one, so
// a tripled prefix, a different code, or arcade dropping the code entirely all
// parse identically.
//
// The message is matched case-insensitively and the txid is returned from the
// lower-cased copy, which is also the canonical form every txid in this program
// is written in.
//
// # What does not parse
//
// ok is false when the marker is absent, when the naming phrase is absent, when
// what follows it is not exactly one 64-character hex string, when that string
// is the null hash, or when the message names two DIFFERENT transactions. A
// false here means HALT — it never means "no conflict". Guessing which of two
// named transactions is ours, or reading a truncated id, would put a cell onto
// an outpoint nobody has verified, and that is the direction that destroys
// cells rather than merely stalling them.
func SpentBy(rejection string) (string, bool) {
	s := strings.ToLower(rejection)
	if !strings.Contains(s, utxoSpentMarker) {
		return "", false
	}

	found := ""
	for rest := s; ; {
		i := strings.Index(rest, spentByPhrase)
		if i < 0 {
			break
		}
		rest = strings.TrimLeft(rest[i+len(spentByPhrase):], " \t:")
		txid, ok := leadingTxID(rest)
		if !ok {
			// The message names a conflicting spend and we cannot read which one.
			// That is strictly worse than a message that names nothing, so it must
			// not fall through to "nothing found".
			return "", false
		}
		if found != "" && found != txid {
			return "", false
		}
		found = txid
	}
	return found, found != ""
}

// leadingTxID reads a transaction id off the front of s.
//
// Exactly 64 hex characters, no more and no fewer: arcade appends the offending
// input index in brackets ("…8808[0]"), which stops the run on its own, but a
// 63- or 65-character run is a message we do not understand rather than one to
// truncate into shape.
func leadingTxID(s string) (string, bool) {
	n := 0
	for n < len(s) && isHexDigit(s[n]) {
		n++
	}
	if n != 64 {
		return "", false
	}
	txid := s[:64]
	if txid == zeroTxID {
		// The null hash is what a missing txid decodes to, not a transaction.
		return "", false
	}
	return txid, true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// RecoverSpentTip decides what to do with a cell halted by a UTXO_SPENT
// rejection that names a transaction we have no record of.
//
// # The damage
//
// Until commit 081bd2a a cell could broadcast a transition whose durable record
// could not be written. When BOTH the write-ahead row and the broadcast row
// failed to persist, the cell was left with no record of that transition AT
// ALL — not an unresolved attempt, not a rejection, nothing. So:
//
//	the record says cell 56's tip is generation 1508
//	in reality a transaction we signed and broadcast spent that tip at 1509, and was MINED
//	on restart the cell derives 1508, re-spends it, and arcade answers UTXO_SPENT
//	the cell halts, permanently
//
// RecoverCell cannot reach this: it triggers on an unresolved `attempting`
// record, and here there is no record to trigger on. Nor is walkForward usable,
// because at these generations the WALLET holds two actions for the same
// generation — the lost mined one and the later rejected re-spend — so
// newestCellAction cannot say which of them is the cell.
//
// The rejection, however, names the lost transaction outright. So this is
// targeted rather than a search: one candidate, named by arcade, verified from
// its own bytes.
//
// # Every condition, and why each one is load-bearing
//
// The verdict is adopt ONLY if all of these hold. Any failure halts, naming
// which one.
//
//  1. The error parses as a UTXO_SPENT rejection naming exactly one txid. See
//     SpentBy. Anything else is not this damage class and is not guessed at.
//  2. That txid is ABSENT from our own record of cell transactions. A named
//     txid we DO hold is not the lost transition — it is our bookkeeping
//     disagreeing with itself (the same txid on another cell, a transaction we
//     recorded as failed and arcade did not, a generation recorded twice), and
//     none of those are situations to resolve by moving a live tip.
//  3. Its bytes hash to that txid. Neither the wallet nor arcade is
//     authenticated; the hash is what makes fetched bytes evidence at all.
//  4. It SPENDS THIS CELL'S RECORDED TIP. Nothing else establishes that it is
//     ours. Rule 110 on a finite ring must cycle, so a row recurs and so does
//     the locking script carrying it: a transition from an earlier pass around
//     the cycle satisfies a script-only check EXACTLY while spending an output
//     consumed long ago. Anchoring on the outpoint is the only thing that tells
//     them apart, and it is readable only from the raw transaction.
//  5. Its output 0 carries this cell's covenant for the recomputed row at
//     tip.Generation+1. DeriveTip proves 3 and 5 together and hands back the
//     tip they imply.
//  6. Arcade reports the network ACCEPTED it. Not "does not say it was
//     rejected" — a positive verdict. An unreachable arcade, an unknown
//     transaction and a rejected one all halt.
//
// # The one that must never be relaxed
//
// A UTXO_SPENT naming a transaction that is NOT ours means somebody else spent
// this cell's output — a genuine foreign double spend, and the end of that
// cell's chain. Adopting anything on that evidence would point the cell at an
// outpoint we cannot spend and, worse, would do it automatically for all 128.
// The same goes for a candidate that fails any structural check: it is an
// unknown situation, and an unknown situation resolved by moving a tip is how
// this deployment lost cells 34, 51 and 91.
//
// Staying halted costs one stalled cell and a human's attention. Adopting
// wrongly costs the cell outright. So every path out of here that is not all
// six conditions is a halt.
//
// # What it does NOT prove
//
// Not that we signed it. A transaction can satisfy every condition above and
// have been built by somebody else — but only by spending our tip and
// reproducing our covenant's continuation at the row the automaton demands,
// which is precisely what the covenant forces any spender to do. Such a
// transaction IS this cell's next generation regardless of who assembled it,
// and adopting it re-spends nothing.
func RecoverSpentTip(ctx context.Context, l Ledger, oracle TxStatus, compiled *cellscript.Compiled,
	tip CellChain, rejection string, next ca.Row, onRecord OnRecord) (Recovery, error) {

	cell, gen := tip.Cell, tip.Generation+1

	// 1. The rejection names a transaction.
	txid, ok := SpentBy(rejection)
	if !ok {
		return halt(fmt.Sprintf(
			"cell %d: generation %d was rejected, but the recorded reason does not parse as a UTXO_SPENT "+
				"naming exactly one transaction, so there is no candidate to verify: %q",
			cell, gen, rejection)), nil
	}

	// 2. It is not already one of ours.
	mine, err := onRecord(ctx, txid)
	if err != nil {
		// A failed lookup is not evidence that the transaction is unknown to us.
		return halt(fmt.Sprintf(
			"cell %d: generation %d was rejected as already spent by %s, but our own record could not be "+
				"searched for it, so nothing is known: %v", cell, gen, txid, err)), nil
	}
	if mine {
		return halt(fmt.Sprintf(
			"cell %d: generation %d was rejected as already spent by %s — but that transaction IS on our "+
				"record, so this is not a lost transition, it is the record disagreeing with itself. "+
				"A human decides", cell, gen, txid)), nil
	}

	// 3. Its bytes are real and hash to the name arcade gave us.
	raw, err := l.RawTx(ctx, txid)
	if err != nil {
		return halt(fmt.Sprintf(
			"cell %d: generation %d was rejected as already spent by %s, but that transaction's bytes could "+
				"not be read, so it cannot be verified as ours: %v", cell, gen, txid, err)), nil
	}
	tx, err := parseTip(cell, txid, raw)
	if err != nil {
		return halt(fmt.Sprintf(
			"cell %d: the bytes offered for the transaction named by the rejection are not that "+
				"transaction: %v", cell, err)), nil
	}

	// 4. It spends OUR tip. Checked before anything about its outputs, because a
	// repeated row makes the outputs alone ambiguous and this is the clause that
	// removes the ambiguity.
	spends := false
	for _, in := range tx.Inputs {
		if in.SourceTXID != nil && in.SourceTXID.String() == tip.TxID && in.SourceTxOutIndex == tip.Vout {
			spends = true
			break
		}
	}
	if !spends {
		return halt(fmt.Sprintf(
			"cell %d: %s is named as having spent this cell's tip, but its inputs do not include %s:%d — "+
				"so either the rejection was about a different input of ours (the fuel coin, say) or this is "+
				"a transaction we have no business adopting", cell, txid, tip.TxID, tip.Vout)), nil
	}

	// 5. Its continuation is this cell's covenant at the next row. DeriveTip
	// re-checks the hash from 3 as well; the duplication is deliberate, because
	// neither function may be safe only by virtue of its caller being careful.
	adopted, err := DeriveTip(compiled, cell, gen, next, txid, raw)
	if err != nil {
		return halt(fmt.Sprintf(
			"cell %d: %s spends this cell's tip, but it does not carry this cell's covenant for "+
				"generation %d: %v", cell, txid, gen, err)), nil
	}
	if adopted.Satoshis != tip.Satoshis {
		return halt(fmt.Sprintf(
			"cell %d: %s continues cell %d with %d satoshis where the tip carries %d; a cell's value is "+
				"constant across generations, so this is not our transition",
			cell, txid, cell, adopted.Satoshis, tip.Satoshis)), nil
	}

	// 6. Arcade agrees the network took it.
	if oracle == nil {
		return halt(fmt.Sprintf(
			"cell %d: %s verifies as this cell's generation %d, but no arcade client was supplied, so "+
				"whether the network accepted it is unknown and it must not be adopted", cell, txid, gen)), nil
	}
	rec, err := oracle.GetTx(ctx, txid)
	if err != nil || rec == nil {
		return halt(fmt.Sprintf(
			"cell %d: %s verifies as this cell's generation %d, but arcade has no verdict for it, so the "+
				"one thing left to establish — that the network accepted it — is unknown: %v",
			cell, txid, gen, err)), nil
	}
	if !acceptedByNetwork(rec.Status) {
		return halt(fmt.Sprintf(
			"cell %d: %s verifies as this cell's generation %d, but arcade reports it as %q rather than "+
				"accepted; adopting a transaction the network did not take would point this cell at an "+
				"output that does not exist", cell, txid, gen, string(rec.Status))), nil
	}

	return Recovery{
		Verdict: VerdictAdopt, Tip: adopted, Steps: []CellChain{adopted},
		Reason: fmt.Sprintf(
			"cell %d: generation %d was refused because %s had already spent this cell's tip — and that "+
				"transaction is a transition of ours that was never recorded: it spends %s:%d, carries "+
				"cell %d's covenant for generation %d at output 0, is on no record of ours, and arcade "+
				"reports it %s. Adopting it; tip moves %d -> %d. Nothing is re-spent",
			cell, gen, txid, tip.TxID, tip.Vout, cell, gen, string(rec.Status),
			tip.Generation, adopted.Generation),
	}, nil
}

// acceptedByNetwork reports whether arcade's verdict is that the network took
// this transaction.
//
// The set is exactly the one engine.stateFor maps to seen-or-mined, so the two
// places that judge an arcade status agree by construction. Everything else —
// UNKNOWN, RECEIVED, SENT_TO_NETWORK, PENDING_RETRY, STUMP_PROCESSING — says
// arcade has the transaction in hand but the network has not spoken, and
// REJECTED and DOUBLE_SPEND_ATTEMPTED say it spoke against. None of those is a
// reason to move a tip; the status set is deliberately a whitelist so that a
// status arcade adds tomorrow halts rather than adopts.
func acceptedByNetwork(s arcade.Status) bool {
	switch s {
	case arcade.StatusAcceptedByNetwork, arcade.StatusSeenOnNetwork,
		arcade.StatusSeenMultipleNodes, arcade.StatusMined, arcade.StatusImmutable:
		return true
	default:
		return false
	}
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
