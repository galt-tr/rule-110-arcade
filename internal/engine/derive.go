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

	// Rejected is set when the generation DIRECTLY above the tip FAILED, and
	// RejectionErr is the recorded reason — usually arcade's own words for why,
	// but not always: a transition that died locally before it could be signed
	// used to be recorded here too, carrying our own chain.ErrNotBroadcast
	// sentinel instead. See chain.RecoverNotBroadcast, which is what those cells
	// are recovered by.
	//
	// "Directly above" is the whole of the condition. A rejection one generation
	// past the tip is a single refused transition with a reason attached, and its
	// reason may name the transaction that really holds that generation — see
	// chain.RecoverSpentTip. A rejection further up is a CASCADE: the cell built
	// on a phantom output and every later attempt was refused in turn, so the
	// newest reason describes the wreckage rather than the original event, and
	// nothing in it points at a recoverable tip.
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

			switch {
			case deep.HasAttempt:
				out[cell].Unknown = true
				out[cell].Halted = true
				out[cell].Attempted = base.Generation + 1
				out[cell].HaltReason = fmt.Sprintf(
					"generation %d was attempted but never resolved, so this cell's tip is UNKNOWN: "+
						"a transition may have been broadcast without being recorded. Run \"rule110 recover\"",
					deep.Attempt.Generation)
			case deep.HasFailed:
				out[cell].Halted = true
				out[cell].HaltReason = fmt.Sprintf(
					"this cell's chain ends at generation %d, buried under rejections up to "+
						"generation %d: %s",
					base.Generation, deep.Failed.Generation, orUnknown(deep.Failed.Err))
				noteRejection(&out[cell], base.Generation, deep.Failed)
			}
			continue
		}
		claims[cell] = claim{gen: base.Generation, txid: base.TxID}
		txids = append(txids, base.TxID)

		// Everything above the tip is a record of something that did not settle.
		for _, r := range rows {
			if r.Generation <= base.Generation {
				break
			}
			switch r.Status {
			case history.StatusFailed:
				out[cell].Halted = true
				out[cell].HaltReason = fmt.Sprintf(
					"generation %d was rejected, so this cell's chain ends at generation %d: %s",
					r.Generation, base.Generation, orUnknown(r.Err))
				noteRejection(&out[cell], base.Generation, r)
			case history.StatusAttempting:
				out[cell].Unknown = true
				out[cell].Halted = true
				out[cell].Attempted = base.Generation + 1
				out[cell].HaltReason = fmt.Sprintf(
					"generation %d was attempted but never resolved, so this cell's tip is UNKNOWN: "+
						"a transition may have been broadcast without being recorded. Run \"rule110 recover\"",
					r.Generation)
			}
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

// noteRejection records a rejection that sits DIRECTLY on the cell's tip, and
// only such a rejection.
//
// Both derivation paths call it with their own idea of "the failure above the
// tip" — the shallow window walks every record above the tip, the deep dig
// returns only the newest — and the generation test is what makes the two mean
// the same thing. Without it the deep path would hand a cascade's 170th
// rejection to recovery as if it described the break, and recovery would parse
// a message about an output that never existed.
//
// The same test is the only thing keeping chain.RecoverStaleRejection away from
// cells 34, 51, 64 and 91: every rejection in a cascade after the first spends
// the previous rejected transaction's output rather than the tip, so every one of
// them would satisfy that check on its own. Adjacency is what distinguishes one
// refused transition from 170 stacked ones.
func noteRejection(p *CellPosition, tip uint64, r history.Tip) {
	if r.Generation != tip+1 {
		return
	}
	p.Rejected = true
	p.RejectionErr = r.Err
	p.RejectionTxID = r.TxID
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
func CheckMigrationFloor(ctx context.Context, store *history.Store,
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
		if got := positions[l.Cell].Tip.Generation; got < l.Generation {
			behind = append(behind, fmt.Sprintf("cell %d: derived %d, state.json says %d",
				l.Cell, got, l.Generation))
		}
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

func orUnknown(s string) string {
	if s == "" {
		return "no reason was recorded"
	}
	return s
}
