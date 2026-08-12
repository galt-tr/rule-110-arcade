package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// tipDepth is how many records per cell derivation reads.
//
// One is enough in the healthy case. The rest cover a cell whose newest records
// are a rejection or an unresolved attempt sitting on top of the last good
// transaction, and a small number is deliberate: a cell buried under more
// unresolved records than this is one whose history nobody understands, and the
// right answer there is to stop and say so rather than to dig.
const tipDepth = 8

// maxWreckage is how many rejections may sit above a cell's tip before the cell
// is a CASCADE rather than a cell with one broken transition on top of it. Past
// it no repair is offered for that cell and a human decides.
//
// This is the guard that used to be spelled "the rejection must be at tip+1".
// Adjacency was a proxy for it, and a wrong one: it also refused cell 12 of the
// live deployment, whose two leftover rejections at generations 993 and 994 over
// a tip at 991 are ordinary wreckage that clears in two passes. What actually
// distinguishes the cells that must not be touched is the SIZE of the pile.
// Cells 34, 51, 64 and 91 carry about 170 stacked rejections each over tips near
// generation 300: those cells were allowed to build on phantom outputs for
// hundreds of generations, so re-creating their chains is a decision about the
// automaton's history rather than about one refused transaction. Resuming them
// would not even help — the rejections underneath the newest would halt the cell
// again at the next startup.
//
// It COUNTS the failures above the tip rather than measuring a run upward from
// tip+1, and that difference is the whole of why the count is trustworthy where
// adjacency was not. A repair retracts one row per pass, so the bottom of any
// pile is routinely missing: "consecutive from tip+1" reads cell 12's two
// leftovers as zero, and — far worse — reads a 169-deep cascade whose bottom row
// the previous pass retracted as zero too, which would hand exactly the four
// cells above to recovery.
//
// Three is the largest pile the live deployment's ordinary wreckage shows: cell
// 12's record was 991 mined and then 992, 993 and 994 all failed — one break, and
// the two transitions the old build stacked on its phantom output before the cell
// was noticed. It is a judgement rather than a measurement, so it is stated once
// here and the count is written into the halt reason, which is where an operator
// reads what was actually found.
const maxWreckage = 3

// The deep path in DeriveTips leans on this: a cell whose wreckage does not fit
// inside the derivation window has more rejections above its tip than maxWreckage
// allows, without anything having to count them. A negative constant here is a
// compile error, which is the point.
const _ = uint(tipDepth - maxWreckage - 1)

// CellPosition is one cell's derived position, and why.
//
// Every field is a conclusion drawn from the history store plus the bytes of the
// transactions it names — nothing here was read from a file that something else
// was also writing.
type CellPosition struct {
	// Tip is the output this cell should spend next. Always set: even a halted
	// cell has a last known good tip, and reporting it is what lets an operator
	// see where the ring broke.
	Tip chain.CellChain

	// Halted means this cell must not advance. HaltReason says why, and is what
	// an operator reads.
	Halted     bool
	HaltReason string

	// Attempted is set when the store holds an unresolved write-ahead record:
	// the cell's real tip is UNKNOWN, because a transition may have been
	// broadcast without being recorded. Such a cell is halted until recovery
	// decides, and recovery is the only thing allowed to clear it.
	Attempted uint64
	Unknown   bool

	// Rejected is set when a FAILED record above the tip is one recovery may
	// examine, and RejectedAt is that record's generation.
	//
	// RejectedAt is carried rather than recomputed from the tip, and that is the
	// whole of the fix for the way repair used to stall. Recovery asked about
	// tip.Generation+1, on the assumption that a failure halting a cell is always
	// the tip's own successor. It is not, once a pass has retracted a row: cell 12
	// of the live deployment sat at tip 991 with failures at 993 and 994 after 992
	// had been retracted. Derivation halted on 993; recovery examined 992, which
	// was empty; so every pass decided there was nothing to do and reported
	// success while the cell stayed dead. Repeated `recover -apply` runs converged
	// on a no-op with 23 cells halted.
	//
	// The record is the OLDEST failure above the tip. That is the break itself:
	// everything above it was built on an output the break never produced, so the
	// oldest is the only one that can possibly be a verdict about the TIP, and
	// retracting it is what lets the next pass see the one above it. In the
	// ordinary case — a single refused transition — it is tip+1, and nothing about
	// such a cell has changed.
	//
	// It is NOT set when the failures above the tip are a CASCADE; see
	// maxWreckage, which is where that line is drawn for every rejection path at
	// once, and noteRejection, which is the only place it is drawn.
	//
	// RejectionErr is the recorded reason — usually arcade's own words for why,
	// but not always: a transition that died locally before it could be signed
	// used to be recorded here too, carrying our own chain.ErrNotBroadcast
	// sentinel instead. See chain.RecoverNotBroadcast, which is what those cells
	// are recovered by.
	//
	// The raw recorded message is carried rather than HaltReason's prose because
	// recovery parses it. HaltReason is written for a human and is free to be
	// reworded; this is not.
	//
	// RejectionTxID is the transaction the record blames for that generation, and
	// it is carried for a reason the message alone cannot serve: the rejection may
	// be about a parent this cell no longer has, and the only way to tell is to
	// fetch that transaction's bytes and look at what it SPENDS. See
	// chain.RecoverStaleRejection. It is empty when the failure was recorded
	// before anything was signed, and that emptiness is itself evidence: it is
	// the second of the two facts chain.RecoverNotBroadcast requires.
	Rejected      bool
	RejectedAt    uint64
	RejectionErr  string
	RejectionTxID string
}

// DeriveTips rebuilds every cell's position from the history store and the
// chain, with nothing taken on trust.
//
// # Why this exists
//
// The tips used to live in state.json, rewritten on a one-second timer from the
// hot path. That file was therefore a second, mutable copy of something the
// history store already recorded per transaction, and the two could disagree —
// silently, because nothing compared them. Here there is one record of where a
// cell is (the store), and the bytes it refers to are fetched by txid, which
// cannot be stale: they hash to the txid or they are rejected.
//
// # What it costs
//
// One indexed query per cell against cell_txs_cell_gen, plus one bulk
// raw-transaction fetch. Bounded by the ring size, not by how long the automaton
// has been running — which the query it replaces was not.
//
// # The classification, and bug 9a
//
// The newest record decides:
//
//	(none)                  fresh genesis: the tip is the genesis output for this cell
//	broadcast/seen/mined    healthy: that is the tip
//	failed                  HALTED, permanently, and the tip stays at the record below
//	attempting              the tip is UNKNOWN; halted until recovery says otherwise
//
// The `failed` row is the fix for a bug that was destroying cells on the live
// deployment. A rejection halted the cell in memory only, while recordCell had
// already advanced the recorded tip to the REJECTED transaction's output. After
// a restart the halt was gone, the phantom tip was not, and the cell spent an
// output that had never existed — forever, once per generation, each rejection
// looking like a fresh independent failure. Reconstructing the halt from the
// same rows that carry the tip makes the two impossible to disagree.
func DeriveTips(ctx context.Context, l chain.Ledger, compiled *cellscript.Compiled,
	d *chain.Deployment, store *history.Store) ([]CellPosition, error) {

	seed, err := d.Seed()
	if err != nil {
		return nil, err
	}
	records, err := store.CellTips(ctx, d.Cells, tipDepth)
	if err != nil {
		return nil, err
	}

	// Classify first, so the raw-transaction fetch can be one bulk call rather
	// than one call per cell.
	type claim struct {
		gen  uint64
		txid string
	}
	claims := make([]claim, d.Cells)
	out := make([]CellPosition, d.Cells)
	txids := make([]string, 0, d.Cells)

	for cell := range d.Cells {
		rows := records[cell]
		if len(rows) == 0 {
			// Never advanced: cell c's tip is genesis output c.
			claims[cell] = claim{gen: 0, txid: d.GenesisTxID}
			txids = append(txids, d.GenesisTxID)
			continue
		}

		base, ok := newestSettled(rows)
		if !ok {
			// The shallow window is all wreckage. Dig for the newest record the
			// network actually accepted before concluding anything: a cell whose
			// rejection cascaded carries a long run of failures over a tip that is
			// perfectly well defined, and reporting that cell as underivable reads
			// as "lost" when it is merely buried. See history.Store.DeepTip.
			deep, err := store.DeepTip(ctx, cell)
			if err != nil {
				return nil, err
			}
			if !deep.HasSettled {
				// Genuinely nothing: no record anywhere names a transaction the
				// network was told about, so there is no tip to derive and none may
				// be guessed at.
				//
				// This halts the CELL rather than failing the whole derivation. One
				// cell buried under failures must not stop the other 127 — and it must
				// not stop `rule110 recover` either, which derives before it can do
				// anything, so failing here would leave an operator with no way in.
				// The tip is left zero, which is safe precisely because a halted cell
				// is never advanced. Unknown is deliberately NOT set: there is nothing
				// here for recovery to reason from, so a human has to look.
				out[cell].Halted = true
				out[cell].HaltReason = fmt.Sprintf(
					"no record for this cell names a transaction the network accepted "+
						"(newest is generation %d, status %q), so its tip cannot be derived",
					rows[0].Generation, rows[0].Status)
				claims[cell] = claim{}
				continue
			}

			// The tip is known. The cell still does not advance — something above
			// the tip failed or is unresolved, which is why the window was full of
			// wreckage — but now the halt names a position an operator and
			// `rule110 recover` can act on instead of a dead end.
			base = deep.Settled
			claims[cell] = claim{gen: base.Generation, txid: base.TxID}
			txids = append(txids, base.TxID)

			// A rejection is asked about FIRST here, which is the opposite of the
			// order everywhere else — Recover dispatches on Unknown before Rejected,
			// and history.DeepTip's own comment argues that an attempt outranks a
			// rejection because an attempt means the tip is unknown.
			//
			// That argument is about one crash. It does not hold at this depth: a cell
			// reaches this branch only when NOTHING in its newest tipDepth records
			// settled, so a rejection above the tip here is part of a pile deeper than
			// maxWreckage allows however it is counted. Leaving Unknown set on top of
			// such a pile would be a way straight round the cascade guard and into
			// chain.RecoverCell, which walks the WALLET's actions forward — and in a
			// cascade those link one to the next (every attempt spent the previous
			// refused transaction's output) while carrying the wallet's own lifecycle
			// status rather than arcade's verdict. It would adopt refused transactions
			// as this cell's tip.
			//
			// A pile of unresolved ATTEMPTS with no rejection in it is a different
			// thing — several transitions signed without any being recorded, which is
			// the crash chain.RecoverCell walks — and still goes to recovery.
			switch {
			case deep.HasFailed:
				// No repair is offered for a cell that got here, and Rejected is
				// deliberately left unset: the wreckage above this tip is deeper than
				// the window, which is the cascade condition, and cells 34, 51, 64 and
				// 91 are what it is for. The tip itself has also only been dug up, not
				// yet verified against its bytes, so there is nothing here a repair
				// could safely reason from even if the pile were small.
				out[cell].Halted = true
				out[cell].HaltReason = fmt.Sprintf(
					"this cell's chain ends at generation %d, buried under rejections up to "+
						"generation %d: %s. Nothing in this cell's %d newest records settled, so this is "+
						"a cascade rather than one refused transition; recovery will not touch it and a "+
						"human decides",
					base.Generation, deep.Failed.Generation, orUnknown(deep.Failed.Err), len(rows))
			case deep.HasAttempt:
				out[cell].Unknown = true
				out[cell].Halted = true
				out[cell].Attempted = base.Generation + 1
				out[cell].HaltReason = fmt.Sprintf(
					"generation %d was attempted but never resolved, so this cell's tip is UNKNOWN: "+
						"a transition may have been broadcast without being recorded. Run \"rule110 recover\"",
					deep.Attempt.Generation)
			}
			continue
		}
		claims[cell] = claim{gen: base.Generation, txid: base.TxID}
		txids = append(txids, base.TxID)

		// Everything above the tip is a record of something that did not settle.
		//
		// The rows are gathered rather than acted on inside the loop. The two kinds
		// have a precedence — an unresolved attempt outranks a rejection, exactly as
		// in Recover's own dispatch — and the loop's iteration order is not it:
		// records arrive newest first, so writing HaltReason as they go makes the
		// message describe whichever record happens to sit lowest. That is how the
		// message and the decision came apart in the first place.
		//
		// The tip being INSIDE the window is what makes the count exact: the window
		// is the newest tipDepth records, so if the tip is in it then every record
		// above the tip is in it too.
		var failed, attempt history.Tip
		failures, attempts := 0, 0
		for _, r := range rows {
			if r.Generation <= base.Generation {
				break
			}
			switch r.Status {
			case history.StatusFailed:
				// Newest first, so each assignment moves this DOWN and what survives
				// the loop is the OLDEST failure above the tip — the break itself,
				// rather than one of the refusals stacked on top of it.
				failed = r
				failures++
			case history.StatusAttempting:
				attempt = r
				attempts++
			}
		}
		cascade := failures > maxWreckage
		if failures > 0 {
			noteRejection(&out[cell], base.Generation, failed, failures)
		}
		if attempts > 0 && !cascade {
			// Last, so that its reason wins: Recover dispatches on Unknown before it
			// looks at Rejected, and the reason an operator reads has to be about the
			// record recovery is going to act on.
			//
			// Which is exactly why a cascade withholds Unknown as well as Rejected.
			// Dispatching on Unknown first means an unresolved attempt sitting on top
			// of the pile would be a way straight round the guard and into
			// chain.RecoverCell — which walks the WALLET's actions forward, and in a
			// cascade those link one to the next (every attempt spent the previous
			// refused transaction's output) while carrying the wallet's own lifecycle
			// status rather than arcade's verdict. It would adopt refused transactions
			// as this cell's tip, which is the direction that destroys a cell rather
			// than stalling it. An attempt above a cascade is wreckage too: the cell
			// was already halted, so nothing legitimate was in flight.
			//
			// Attempted stays the tip's own successor rather than the row's own
			// generation, which is this path's one remaining gap and is deliberately
			// left as it was. chain.RecoverCell reasons about tip+1 and
			// DeleteAttempt retracts tip+1, so an unresolved record sitting HIGHER
			// than that is decided about a generation it is not at — the same shape
			// as the rejection bug above. It is left alone because narrowing it here
			// would stop RecoverCell walking the wallet's own signed successors
			// forward, which is a live repair, and no such record has been seen. The
			// message names where the row actually is, so at least the report says so.
			out[cell].Unknown = true
			out[cell].Halted = true
			out[cell].Attempted = base.Generation + 1
			out[cell].HaltReason = fmt.Sprintf(
				"generation %d was attempted but never resolved, so this cell's tip is UNKNOWN: "+
					"a transition may have been broadcast without being recorded. Run \"rule110 recover\"",
				attempt.Generation)
		}
	}

	wanted := make(map[uint64]bool, d.Cells)
	highest := uint64(0)
	for _, c := range claims {
		if c.txid == "" {
			continue
		}
		wanted[c.gen] = true
		highest = max(highest, c.gen)
	}
	rows := computeRows(seed, d.Rule, wanted, highest)
	if err := crossCheckRows(ctx, store, rows); err != nil {
		return nil, err
	}

	raws, err := fetchRaw(ctx, l, txids)
	if err != nil {
		return nil, err
	}

	for cell := range d.Cells {
		c := claims[cell]
		if c.txid == "" {
			continue // undeducible and already halted above
		}
		raw, ok := raws[c.txid]
		if !ok {
			return nil, fmt.Errorf(
				"engine: cell %d is recorded at generation %d on transaction %s, but its bytes could not be "+
					"found in the wallet or at arcade, so its tip cannot be verified",
				cell, c.gen, c.txid)
		}
		tip, err := chain.DeriveTip(compiled, cell, c.gen, rows[c.gen], c.txid, raw)
		if err != nil {
			return nil, err
		}
		out[cell].Tip = tip
	}
	return out, nil
}

// noteRejection records the failure derivation halted on, and writes the halt
// reason from that same record.
//
// One record decides both, and that is the point. They used to be able to
// disagree: derivation halted on the oldest failure above the tip and said so in
// the message, while recovery went off and looked at tip.Generation+1. On cell 12
// those were generations 993 and 992, and 992 had already been retracted — so the
// operator read "generation 993 was rejected" and the tool then reported success
// having examined an empty row. Everything recovery needs about that record now
// comes from here, so the two cannot come apart again.
//
// `above` is how many failures sit above the tip in total, and it is the cascade
// guard: past maxWreckage, Rejected is left unset and no repair is dispatched for
// the cell at all. The halt still names the record and the count, because an
// operator who came to look at one specific cell must find it in the output
// however it was decided.
//
// This is the ONLY place that line is drawn on the engine side, so it holds for
// every rejection path at once — the tip fix, the not-broadcast sentinel, the
// superseded parent and the opt-in retry all reach chain through
// CellPosition.Rejected.
func noteRejection(p *CellPosition, tip uint64, r history.Tip, above int) {
	p.Halted = true
	if above > maxWreckage {
		p.HaltReason = fmt.Sprintf(
			"this cell's chain ends at generation %d with %d rejections stacked above it, the oldest "+
				"at generation %d: %s. That is a cascade rather than one refused transition — every "+
				"attempt after the first spent an output that never existed — so recovery will not "+
				"touch it and a human decides",
			tip, above, r.Generation, orUnknown(r.Err))
		return
	}
	p.Rejected = true
	p.RejectedAt = r.Generation
	p.RejectionErr = r.Err
	p.RejectionTxID = r.TxID

	if above == 1 {
		p.HaltReason = fmt.Sprintf(
			"generation %d was rejected, so this cell's chain ends at generation %d: %s",
			r.Generation, tip, orUnknown(r.Err))
		return
	}
	p.HaltReason = fmt.Sprintf(
		"generation %d was rejected, so this cell's chain ends at generation %d (%d rejections sit "+
			"above the tip; this is the oldest and the only one that can be a verdict about the tip, "+
			"so recovery examines it first): %s",
		r.Generation, tip, above, orUnknown(r.Err))
}

// newestSettled returns the newest record that names a transaction the network
// was told about. Records are newest first.
func newestSettled(rows []history.Tip) (history.Tip, bool) {
	for _, r := range rows {
		switch r.Status {
		case history.StatusBroadcast, history.StatusSeen, history.StatusMined:
			if r.TxID != "" {
				return r, true
			}
		}
	}
	return history.Tip{}, false
}

// computeRows evaluates the automaton from the seed and returns the rows at the
// generations asked for.
//
// The rows are RECOMPUTED rather than read back because the automaton is
// deterministic: the seed and the rule reproduce every row that has ever
// existed, so a stored row is a copy that can only ever be wrong.
//
// Only the wanted generations are RETAINED — at most one per cell — because a
// long-running deployment has a great many of them and the intermediate ones are
// of no interest. The walk itself is still linear in the highest generation, at
// one Step over the ring per generation: roughly 160k cell updates at the live
// deployment's highest claim of 1279, which is microseconds. It is the retention
// that would have grown without bound, not the arithmetic.
func computeRows(seed ca.Row, rule ca.Rule, wanted map[uint64]bool, highest uint64) map[uint64]ca.Row {
	out := make(map[uint64]ca.Row, len(wanted))
	row := seed
	if wanted[0] {
		out[0] = row
	}
	for g := uint64(1); g <= highest; g++ {
		row = rule.Step(row)
		if wanted[g] {
			out[g] = row
		}
	}
	return out
}

// crossCheckRows compares the recomputed rows against the ones the store
// recorded when they were proved, and refuses to start on any disagreement.
//
// Both are supposed to be the same sequence. If they are not, then either the
// seed in the deployment file is not the seed the chain actually ran from, or
// the store's generation numbering has slipped — and in both cases every
// locking script we are about to build is for the wrong row, which fails on
// chain as a bare OP_CHECKSIGVERIFY giving no hint why.
//
// Only the generations cells actually sit at are checked, which is at most one
// per cell. Checking every generation from 0 would be a point lookup per
// generation ever run — thousands of round trips at startup on the live
// deployment, and unbounded growth after that — for no more assurance: a wrong
// row matters exactly where a script is about to be built from it.
func crossCheckRows(ctx context.Context, store *history.Store, rows map[uint64]ca.Row) error {
	for g, row := range rows {
		recorded, ok, err := store.RowAt(ctx, g)
		if err != nil {
			return err
		}
		if !ok || recorded == "" {
			continue // the store simply has no record of this generation
		}
		if recorded != row.Hex() {
			return fmt.Errorf(
				"engine: generation %d was recorded as row %s but the seed and rule produce %s; "+
					"the deployment's seed does not describe the chain that ran, so nothing here can be "+
					"trusted to build the right script", g, recorded, row.Hex())
		}
	}
	return nil
}

// fetchRaw pulls the bytes for a set of txids, deduplicated.
//
// Genesis is one transaction shared by every cell, so a fresh deployment asks
// for the same txid `cells` times; deduplicating turns that into one lookup.
func fetchRaw(ctx context.Context, l chain.Ledger, txids []string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(txids))
	for _, txid := range txids {
		if _, done := out[txid]; done {
			continue
		}
		raw, err := l.RawTx(ctx, txid)
		if err != nil {
			return nil, err
		}
		out[txid] = raw
	}
	return out, nil
}

// CheckMigrationFloor refuses to start when derivation lands BELOW the tips a
// previous build recorded in state.json.
//
// This is the one place the legacy file is still consulted, and it exists
// because the failure it prevents is catastrophic and silent. The live file
// holds tips around generation 837. If the history store is behind for any cell
// — an older deployment, a restored database, a store that was never written —
// derivation says "no records, so generation 0, spend the genesis output", and
// the automaton re-spends 128 outputs that were consumed hundreds of generations
// ago. Every one of them is rejected; every cell dies at once.
//
// Deriving something HIGHER than the legacy file is expected and fine: the file
// was written on a timer and is routinely a few generations stale. Only lower is
// impossible, and impossible means stop.
//
// Legacy tips the record says the network REFUSED are skipped rather than
// treated as a floor. This is not a loosening of the check — it is the check
// being correct about what a floor is.
// The file was written by a build that advanced a cell's tip on broadcast rather
// than on acceptance, so a cell halted by a rejection carries the REJECTED
// transaction as its recorded tip. That transaction has no output. Derivation
// landing "below" it is derivation being right: it fell back to the last
// transaction that was actually accepted. Treating a phantom as a floor would
// refuse to start forever on a deployment whose store is perfectly intact, and
// the only way out an operator would find is `import-tips`, which would then
// point the cell AT the phantom. See history.Store.RejectedTxIDs.
//
// The lookup happens in here rather than being a parameter deliberately. Every
// caller of this function is on a startup path, and a caller that forgot to pass
// the rejected set would get a check that refuses to start on an intact
// deployment — a failure that only shows up against a legacy file nobody has in
// a test environment. Making it impossible to omit is worth the store argument.
// The "was it refused" question goes to the NETWORK, with our own record used
// only as a cache. That distinction is not pedantry — it was a live bug. This
// check first asked the store alone, on the reasoning that a rejection resolves
// to a `failed` row. Recovery then learned to RETRACT those rows, which is
// correct (a rejection built on a superseded parent is wreckage, not evidence
// about the tip) and which deleted the very thing this check was reading. The
// deployment could not start: three legacy tips it had previously proved refused
// were suddenly unexplained, and it refused to run over a store that was
// perfectly intact.
//
// Whether a transaction exists is not a fact our database owns. Asking arcade
// makes the answer independent of anything recovery, pruning or an operator does
// to the record. A nil oracle falls back to the record alone, which is the old
// behaviour and is what the offline tests use.
func CheckMigrationFloor(ctx context.Context, store *history.Store, oracle chain.TxStatus,
	positions []CellPosition, legacy []chain.CellChain) error {

	txids := make([]string, 0, len(legacy))
	for _, l := range legacy {
		if l.TxID != "" {
			txids = append(txids, l.TxID)
		}
	}
	rejected, err := store.RejectedTxIDs(ctx, txids)
	if err != nil {
		return err
	}

	var behind []string
	for _, l := range legacy {
		if l.Cell < 0 || l.Cell >= len(positions) || l.TxID == "" {
			continue
		}
		if rejected[l.TxID] {
			continue
		}
		got := positions[l.Cell].Tip.Generation
		if got >= l.Generation {
			continue
		}
		// The record does not say this one was refused, but the record is not the
		// authority and may have had the row retracted. Ask the network before
		// refusing to start over it.
		if refused, known := networkRefused(ctx, oracle, l.TxID); known && refused {
			continue
		}
		behind = append(behind, fmt.Sprintf("cell %d: derived %d, state.json says %d",
			l.Cell, got, l.Generation))
	}
	if len(behind) == 0 {
		return nil
	}
	sort.Strings(behind)
	return fmt.Errorf(
		"engine: the history store is BEHIND the tips recorded in state.json for %d cell(s), so starting "+
			"would re-spend outputs that are already spent:\n  %s\n"+
			"run \"rule110 import-tips\" to backfill the store from the legacy record "+
			"(it verifies every entry and is a dry run by default)",
		len(behind), strings.Join(behind, "\n  "))
}

// networkRefused asks arcade whether txid was refused, reporting separately
// whether an answer was obtained at all.
//
// The two return values matter. An unreachable arcade, an absent oracle, or a
// transaction arcade has never heard of are all "not known", and a floor is NOT
// waived on a guess: an unanswered question leaves the legacy tip standing as a
// floor and the deployment refuses to start, which is the safe direction. Only a
// definite REJECTED waives it.
func networkRefused(ctx context.Context, oracle chain.TxStatus, txid string) (refused, known bool) {
	if oracle == nil {
		return false, false
	}
	rec, err := oracle.GetTx(ctx, txid)
	if err != nil || rec == nil {
		return false, false
	}
	return chain.RefusedByNetwork(rec.Status), true
}

func orUnknown(s string) string {
	if s == "" {
		return "no reason was recorded"
	}
	return s
}
