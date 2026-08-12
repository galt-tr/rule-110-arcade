// Package audit checks, from the record and the chain alone, that what ran
// really was Rule 110.
//
// The covenant proves one thing per transaction: cell i's next bit is the
// correct rule output for the three bits cell i CLAIMS to have read. It proves
// nothing about whether that claim matches what cells i-1 and i+1 actually did —
// there is no opcode anywhere comparing one cell's row to another's, and the
// README says so at length. A generation is therefore proved by the conjunction
// of N transactions PLUS the claim that all N were handed the same row, and only
// the first half of that is enforced on chain.
//
// This package is the second half. It re-derives every row from its predecessor
// with the reference implementation, decodes out of each transaction the
// neighbourhood its script actually read, and checks the two against each other.
// Nothing here trusts the engine that wrote the record, and nothing here runs a
// script interpreter: a covenant that verified is evidence about one bit, and
// re-running it would only re-prove the half that was never in doubt.
//
// It is deliberately read-only and lease-free. An audit that could only run
// while the automaton was stopped would be an audit nobody ever ran.
package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// TxSource supplies the bytes of a transaction by txid.
//
// Narrow on purpose, and narrower than chain.Ledger: an audit reads
// transactions and asks the wallet nothing else. Both chain.TxReader (a
// read-only database connection) and chain.Chain satisfy it, and a map
// satisfies it in tests.
type TxSource interface {
	// RawTx returns the serialized transaction, not a BEEF. It may return bytes
	// that do not hash to the requested txid; checking that is this package's
	// job, because a source that lies is one of the things being audited.
	RawTx(ctx context.Context, txid string) ([]byte, error)
}

// Check names one of the properties this package verifies. Every pass and every
// failure is attributed to exactly one, so a report says what broke and not
// merely that something did.
type Check string

const (
	// CheckContinuity is the row-level property: generation N's recorded row is
	// the Rule 110 image of generation N-1's, recomputed here from internal/ca
	// rather than taken from the record.
	CheckContinuity Check = "row continuity"

	// CheckIdentity is that a transaction's bytes hash to the txid the record
	// filed them under. Everything else rests on it.
	CheckIdentity Check = "transaction identity"

	// CheckLinkage is chain integrity: cell i's generation-N transaction spends
	// cell i's generation-(N-1) output, so the cell is one unbroken chain and
	// not a series of unrelated transactions that happen to be recorded together.
	CheckLinkage Check = "chain integrity"

	// CheckBinding is that the output being spent really is this cell's covenant
	// — decoded from the script's own neighbourhood constants, not assumed from
	// the record that pointed at it.
	CheckBinding Check = "cell binding"

	// CheckCovenant is that the sighash preimage the unlocking script carries is
	// the one this spend produces. It is what makes the decoded neighbourhood
	// evidence rather than assertion: a preimage that does not recompute
	// describes some other transaction.
	CheckCovenant Check = "covenant binding"

	// CheckNeighbourhood is the whole point of this command. The three bits cell
	// i's script read must equal the bits its neighbours actually held in the
	// recorded row. Script does not check this and cannot.
	CheckNeighbourhood Check = "cross-cell agreement"

	// CheckCarriedRow is the same comparison widened to the whole row the cell's
	// UTXO carries. A cell only needs three bits to be right for its own
	// transition to be correct, but a divergence anywhere else is a divergence
	// that becomes its neighbourhood a few generations later.
	CheckCarriedRow Check = "carried row"

	// CheckRule is Rule 110 itself, recomputed: the bit the transaction proved
	// must be the rule's output for the three bits it read. This is the property
	// the covenant enforces, checked here without a script interpreter so that
	// the audit stands on its own.
	CheckRule Check = "rule"

	// CheckSuccessor is that the continuation output carries this cell's script
	// for the row the record ascribes to generation N, so the chain the next
	// generation spends is the one the record describes.
	CheckSuccessor Check = "successor"
)

// checkOrder is the order checks are reported in: roughly the order they are
// applied, which is also the order in which one failing explains the next.
var checkOrder = []Check{
	CheckContinuity, CheckIdentity, CheckLinkage, CheckBinding, CheckCovenant,
	CheckNeighbourhood, CheckCarriedRow, CheckRule, CheckSuccessor,
}

// Failure is one property that did not hold, named precisely enough to act on.
type Failure struct {
	Generation uint64
	// Cell is the cell at fault, or -1 for a finding about a whole generation.
	Cell   int
	Check  Check
	Detail string
}

func (f Failure) String() string {
	if f.Cell < 0 {
		return fmt.Sprintf("generation %d: %s: %s", f.Generation, f.Check, f.Detail)
	}
	return fmt.Sprintf("generation %d cell %d: %s: %s", f.Generation, f.Cell, f.Check, f.Detail)
}

// Gap is something the audit could NOT check, with the reason.
//
// Gaps are reported as loudly as failures and counted separately, because the
// two mean opposite things and collapsing them is how an audit comes to say
// "pass" about evidence it never saw. An unrecorded cell, a transaction whose
// bytes have been pruned and a write-ahead record for a transition still in
// flight are all gaps: nothing is wrong, but nothing is proved either.
type Gap struct {
	Generation uint64
	Cell       int
	Reason     string
}

func (g Gap) String() string {
	if g.Cell < 0 {
		return fmt.Sprintf("generation %d: %s", g.Generation, g.Reason)
	}
	return fmt.Sprintf("generation %d cell %d: %s", g.Generation, g.Cell, g.Reason)
}

// Report is what one audit found.
type Report struct {
	From, To uint64

	// Generations is how many generations had a recorded row to examine, and
	// Transactions how many cell transactions were fully checked.
	Generations  int
	Transactions int

	// Passed counts the checks that held, per check.
	Passed map[Check]int

	Failures []Failure
	Gaps     []Gap

	// Truncated reports that the audit stopped early on Options.MaxFailures.
	// A report that is truncated is not a verdict on the generations it never
	// reached, and must not be read as one.
	Truncated bool
}

// OK reports whether every check that ran held. It says nothing about the gaps;
// see Gap.
func (r *Report) OK() bool { return len(r.Failures) == 0 }

// PassedTotal is how many individual checks held.
func (r *Report) PassedTotal() int {
	total := 0
	for _, n := range r.Passed {
		total += n
	}
	return total
}

// CheckCount is one check and how many times it held.
type CheckCount struct {
	Check Check
	N     int
}

// Counts returns the per-check pass counts in report order, skipping checks
// that never ran.
func (r *Report) Counts() []CheckCount {
	var out []CheckCount
	for _, c := range checkOrder {
		if n := r.Passed[c]; n > 0 {
			out = append(out, CheckCount{c, n})
		}
	}
	return out
}

// Options bounds one audit.
type Options struct {
	// From and To are the inclusive generation range.
	From, To uint64

	// Seed is generation 0's row, from the deployment's own record of what it
	// was created with. Only used when the range includes generation 0, which
	// has no predecessor to be derived from.
	Seed ca.Row

	// MaxFailures stops the audit once this many failures have been found, or 0
	// for no limit. A deployment that is broken in a systematic way is broken
	// identically in every generation, and fetching a hundred thousand
	// transactions to say so again is not worth the wait.
	MaxFailures int
}

// Run audits the recorded history against the transactions that proved it.
//
// It returns an error only when the audit itself could not be carried out — a
// database that will not answer, a range that makes no sense. A deployment that
// fails its own checks is a successful audit with a failing Report, and the
// caller decides what to do about it.
func Run(ctx context.Context, compiled *cellscript.Compiled, store *history.Store,
	source TxSource, opts Options) (*Report, error) {

	if store == nil || source == nil || compiled == nil {
		return nil, fmt.Errorf("audit: store, source and compiled contract are all required")
	}
	if opts.To < opts.From {
		return nil, fmt.Errorf("audit: generation range %d..%d is empty", opts.From, opts.To)
	}

	a := &auditor{
		compiled: compiled,
		cells:    compiled.Cells(),
		rule:     compiled.Rule(),
		store:    store,
		source:   source,
		opts:     opts,
		rep:      &Report{From: opts.From, To: opts.To, Passed: map[Check]int{}},
		txs:      map[string]*transaction.Transaction{},
	}
	if err := a.run(ctx); err != nil {
		return nil, err
	}
	return a.rep, nil
}

// generation is one recorded generation, reduced to what the audit needs.
type generation struct {
	number uint64
	row    ca.Row
	// cellTx is the record for each cell, by cell index. A cell absent from the
	// map has no record at all for this generation.
	cellTx map[int]history.CellTx
}

type auditor struct {
	compiled *cellscript.Compiled
	cells    int
	rule     ca.Rule
	store    *history.Store
	source   TxSource
	opts     Options
	rep      *Report

	// txs holds the transactions fetched for the generation being audited and
	// the one before it, keyed by txid.
	//
	// This is the difference between one fetch per cell transaction and two.
	// Every transition's parent is its own cell's previous transition, so the
	// parent of everything in generation N was fetched while auditing N-1. The
	// cache is rotated at each generation boundary rather than grown, because an
	// audit spanning thousands of generations must not hold them all in memory.
	txs  map[string]*transaction.Transaction
	prev map[string]*transaction.Transaction
}

func (a *auditor) run(ctx context.Context) error {
	var prevGen *generation

	for g := a.opts.From; g <= a.opts.To; g++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The predecessor is needed for the row derivation, for each cell's
		// parent outpoint, and for the neighbourhood comparison. At the start of
		// the range it has not been walked yet, so it is loaded outright.
		if prevGen == nil && g > 0 {
			loaded, err := a.load(ctx, g-1)
			if err != nil {
				return err
			}
			prevGen = loaded
		}

		cur, err := a.load(ctx, g)
		if err != nil {
			return err
		}
		if cur == nil {
			a.gap(g, -1, "no row recorded for this generation, so nothing about it can be checked")
			prevGen = nil
			a.rotate()
			continue
		}
		a.rep.Generations++

		a.auditGeneration(ctx, cur, prevGen)
		if a.stop() {
			a.rep.Truncated = true
			return nil
		}

		prevGen = cur
		a.rotate()
	}
	return nil
}

// rotate advances the transaction cache one generation, dropping everything
// older than the generation just audited.
func (a *auditor) rotate() {
	a.prev = a.txs
	a.txs = map[string]*transaction.Transaction{}
}

func (a *auditor) auditGeneration(ctx context.Context, cur, prev *generation) {
	switch {
	case cur.number == 0:
		a.auditSeedRow(cur)
	case prev == nil || prev.number != cur.number-1:
		a.gap(cur.number, -1,
			"generation "+fmt.Sprint(cur.number-1)+" is not recorded, so this row cannot be derived "+
				"and no cell's neighbourhood can be compared against it")
		return
	default:
		want := a.rule.Step(prev.row)
		if !want.Equal(cur.row) {
			a.fail(cur.number, -1, CheckContinuity, fmt.Sprintf(
				"recorded row is %s, but rule %d applied to generation %d's row %s gives %s",
				cur.row.Hex(), a.rule, prev.number, prev.row.Hex(), want.Hex()))
		} else {
			a.pass(CheckContinuity)
		}
	}

	for cell := range a.cells {
		if a.stop() {
			return
		}
		rec, ok := cur.cellTx[cell]
		if !ok {
			a.gap(cur.number, cell, "no transaction recorded, so this cell's bit is not proved")
			continue
		}
		switch {
		case rec.TxID == "":
			a.gap(cur.number, cell, fmt.Sprintf(
				"record is %q with no txid, so no transaction exists to check yet", rec.Status))
			continue
		case rec.Status == history.StatusFailed:
			a.gap(cur.number, cell, fmt.Sprintf(
				"record says the network refused %s, so this cell did not advance here", rec.TxID))
			continue
		}

		if cur.number == 0 {
			a.auditGenesisCell(ctx, cur, cell, rec)
			continue
		}
		a.auditCell(ctx, cur, prev, cell, rec)
	}
}

// auditSeedRow checks generation 0 against the seed the deployment was created
// with, which is the only thing it can be checked against: it has no
// predecessor to be derived from.
func (a *auditor) auditSeedRow(cur *generation) {
	if a.opts.Seed == nil {
		a.gap(0, -1, "generation 0 has no predecessor and no seed was supplied, so its row is taken on trust")
		return
	}
	if !a.opts.Seed.Equal(cur.row) {
		a.fail(0, -1, CheckContinuity, fmt.Sprintf(
			"recorded row is %s, but the deployment was created with seed %s",
			cur.row.Hex(), a.opts.Seed.Hex()))
		return
	}
	a.pass(CheckContinuity)
}

// auditGenesisCell checks one cell's generation-0 output.
//
// Generation 0 is not a transition: one genesis transaction created all N cell
// outputs at once, so there is no unlocking script to decode and no parent to
// link to. What can be checked is the only thing that exists — that output
// `cell` of that transaction really is cell `cell`'s covenant carrying the
// recorded row — and that is worth checking, because it is the foundation every
// later generation is anchored to.
func (a *auditor) auditGenesisCell(ctx context.Context, cur *generation, cell int, rec history.CellTx) {
	tx, err := a.tx(ctx, rec.TxID)
	if err != nil {
		a.report(0, cell, CheckIdentity, err)
		return
	}
	a.pass(CheckIdentity)

	if cell >= len(tx.Outputs) {
		a.fail(0, cell, CheckBinding, fmt.Sprintf(
			"genesis transaction %s has %d outputs, so there is none at %d for this cell",
			rec.TxID, len(tx.Outputs), cell))
		return
	}
	binding, err := a.compiled.Decode(tx.Outputs[cell].LockingScript.Bytes())
	if err != nil {
		a.fail(0, cell, CheckBinding, fmt.Sprintf("output %d of %s: %v", cell, rec.TxID, err))
		return
	}
	if binding.Cell != cell {
		a.fail(0, cell, CheckBinding, fmt.Sprintf(
			"output %d of %s carries cell %d's covenant, not cell %d's", cell, rec.TxID, binding.Cell, cell))
		return
	}
	if !binding.Row.Equal(cur.row) {
		a.fail(0, cell, CheckBinding, fmt.Sprintf(
			"output %d of %s carries row %s, but generation 0 is recorded as %s",
			cell, rec.TxID, binding.Row.Hex(), cur.row.Hex()))
		return
	}
	a.pass(CheckBinding)
	a.rep.Transactions++
}

// auditCell is the whole audit for one cell transition, and the order of its
// clauses is the order in which each makes the next meaningful.
//
// It returns as soon as something fails, rather than pressing on with checks
// whose subject has already been shown to be the wrong thing: once the output
// being spent turns out not to be this cell's covenant, "the neighbourhood it
// read disagrees with the record" is noise, not a second finding.
func (a *auditor) auditCell(ctx context.Context, cur, prev *generation, cell int, rec history.CellTx) {
	gen := cur.number

	parent, ok := prev.cellTx[cell]
	if !ok || parent.TxID == "" {
		a.gap(gen, cell, fmt.Sprintf(
			"generation %d has no transaction recorded for this cell, so there is no outpoint to "+
				"check this one against", prev.number))
		return
	}
	// The continuation of a transition is always output 0 — the covenant
	// rebuilds it there — but genesis created one output per cell, in cell
	// order. Both are facts about how the output was made, so they are asserted
	// rather than searched for.
	parentVout := uint32(0)
	if prev.number == 0 {
		parentVout = uint32(cell) //nolint:gosec // cell is bounded by the ring size
	}

	tx, err := a.tx(ctx, rec.TxID)
	if err != nil {
		a.report(gen, cell, CheckIdentity, err)
		return
	}
	a.pass(CheckIdentity)

	parentTx, err := a.tx(ctx, parent.TxID)
	if err != nil {
		a.report(gen, cell, CheckIdentity, fmt.Errorf("parent transaction: %w", err))
		return
	}

	// --- chain integrity: this transaction spends this cell's previous tip ----

	input := -1
	for i, in := range tx.Inputs {
		if in.SourceTXID != nil && in.SourceTXID.String() == parent.TxID && in.SourceTxOutIndex == parentVout {
			input = i
			break
		}
	}
	if input < 0 {
		a.fail(gen, cell, CheckLinkage, fmt.Sprintf(
			"%s does not spend this cell's generation-%d output %s:%d; it spends %s — "+
				"so this cell's chain is broken here",
			rec.TxID, prev.number, parent.TxID, parentVout, outpoints(tx)))
		return
	}
	if int(parentVout) >= len(parentTx.Outputs) {
		a.fail(gen, cell, CheckLinkage, fmt.Sprintf(
			"parent %s has %d outputs, so %s spends an outpoint that does not exist",
			parent.TxID, len(parentTx.Outputs), rec.TxID))
		return
	}
	a.pass(CheckLinkage)

	// --- what the covenant actually read -------------------------------------

	parentOut := parentTx.Outputs[parentVout]
	binding, err := a.compiled.Decode(parentOut.LockingScript.Bytes())
	if err != nil {
		a.fail(gen, cell, CheckBinding, fmt.Sprintf("output %s:%d: %v", parent.TxID, parentVout, err))
		return
	}
	if binding.Cell != cell {
		a.fail(gen, cell, CheckBinding, fmt.Sprintf(
			"%s spends %s:%d, which is cell %d's covenant, not cell %d's — so this transaction "+
				"verified another cell's bit", rec.TxID, parent.TxID, parentVout, binding.Cell, cell))
		return
	}
	a.pass(CheckBinding)

	unlock, err := a.compiled.DecodeUnlocking(tx.Inputs[input].UnlockingScript.Bytes())
	if err != nil {
		a.fail(gen, cell, CheckCovenant, fmt.Sprintf("input %d of %s: %v", input, rec.TxID, err))
		return
	}
	if !a.checkPreimage(gen, cell, rec.TxID, tx, input, parentOut, unlock) {
		return
	}
	a.pass(CheckCovenant)

	// --- the checks the chain does not make ----------------------------------

	left := ca.LeftIndex(a.cells, cell)
	right := ca.RightIndex(a.cells, cell)
	l, c, r := bit(binding.Row, left), bit(binding.Row, cell), bit(binding.Row, right)

	var wrong []string
	for _, n := range []struct {
		what  string
		index int
	}{{"left neighbour", left}, {"itself", cell}, {"right neighbour", right}} {
		if binding.Row.Get(n.index) != prev.row.Get(n.index) {
			wrong = append(wrong, fmt.Sprintf("%s (cell %d) as %d where the recorded row has %d",
				n.what, n.index, bit(binding.Row, n.index), bit(prev.row, n.index)))
		}
	}
	if len(wrong) > 0 {
		a.fail(gen, cell, CheckNeighbourhood, fmt.Sprintf(
			"the row this cell's covenant read (%s) disagrees with generation %d's recorded row (%s): "+
				"it read %s — nothing in Script checks this, which is why it is checked here",
			binding.Row.Hex(), prev.number, prev.row.Hex(), joinAnd(wrong)))
		return
	}
	a.pass(CheckNeighbourhood)

	if !binding.Row.Equal(prev.row) {
		a.fail(gen, cell, CheckCarriedRow, fmt.Sprintf(
			"this cell's UTXO carries row %s, but generation %d is recorded as %s; the neighbourhood "+
				"still agrees, so this transition is correct, but the divergence reaches this cell's "+
				"neighbourhood within %d generations",
			binding.Row.Hex(), prev.number, prev.row.Hex(), a.cells/2))
		return
	}
	a.pass(CheckCarriedRow)

	// --- the successor this transaction actually created ----------------------

	if len(tx.Outputs) == 0 {
		a.fail(gen, cell, CheckSuccessor, fmt.Sprintf("%s has no outputs", rec.TxID))
		return
	}
	next, err := a.compiled.Decode(tx.Outputs[0].LockingScript.Bytes())
	if err != nil {
		a.fail(gen, cell, CheckSuccessor, fmt.Sprintf("output 0 of %s: %v", rec.TxID, err))
		return
	}
	switch {
	case next.Cell != cell:
		a.fail(gen, cell, CheckSuccessor, fmt.Sprintf(
			"output 0 of %s carries cell %d's covenant, not cell %d's", rec.TxID, next.Cell, cell))
		return
	case !next.Row.Equal(unlock.NextRow):
		a.fail(gen, cell, CheckSuccessor, fmt.Sprintf(
			"the unlocking script presents row %s but the continuation output carries %s",
			unlock.NextRow.Hex(), next.Row.Hex()))
		return
	case next.Row.Get(cell) != cur.row.Get(cell):
		a.fail(gen, cell, CheckSuccessor, fmt.Sprintf(
			"this cell's bit in the continuation output is %d, but generation %d is recorded as "+
				"%s, whose bit %d is %d",
			bit(next.Row, cell), gen, cur.row.Hex(), cell, bit(cur.row, cell)))
		return
	}
	a.pass(CheckSuccessor)

	// Rule 110 itself, recomputed from the bits the script read. This is the one
	// property the covenant does enforce; checking it anyway is what lets this
	// audit be read without also trusting the script engine, the compiler or the
	// contract.
	if want := a.rule.Next(l, c, r); want != bit(next.Row, cell) {
		a.fail(gen, cell, CheckRule, fmt.Sprintf(
			"cell read (l=%d, c=%d, r=%d) and proved %d, but rule %d gives %d",
			l, c, r, bit(next.Row, cell), a.rule, want))
		return
	}
	a.pass(CheckRule)

	a.rep.Transactions++
}

// checkPreimage verifies that the preimage the unlocking script carries is the
// one this spend produces, and reports precisely which part of it is wrong when
// it is not.
//
// The scriptCode comparison is called out separately because it is the field the
// rest of this audit reads: it is a copy of the output being spent, so if it
// differs from the real output, the row decoded from either one is not the row
// the covenant was evaluated against and every conclusion drawn from it is void.
func (a *auditor) checkPreimage(gen uint64, cell int, txid string, tx *transaction.Transaction,
	input int, parentOut *transaction.TransactionOutput, unlock cellscript.Unlock) bool {

	prevLock := parentOut.LockingScript.Bytes()
	want, err := a.compiled.Preimage(tx, input, prevLock, parentOut.Satoshis)
	if err != nil {
		a.fail(gen, cell, CheckCovenant, err.Error())
		return false
	}
	if bytes.Equal(want, unlock.Preimage) {
		return true
	}

	wantCode, err := a.compiled.ScriptCode(prevLock)
	if err != nil {
		a.fail(gen, cell, CheckCovenant, err.Error())
		return false
	}
	gotCode, err := preimageScriptCode(unlock.Preimage)
	if err != nil {
		a.fail(gen, cell, CheckCovenant, fmt.Sprintf("input %d of %s: %v", input, txid, err))
		return false
	}
	if !bytes.Equal(wantCode, gotCode) {
		a.fail(gen, cell, CheckCovenant, fmt.Sprintf(
			"input %d of %s committed to a different script than the output it spends, so the row "+
				"its covenant read is not the row that output carries", input, txid))
		return false
	}
	a.fail(gen, cell, CheckCovenant, fmt.Sprintf(
		"input %d of %s carries a %d-byte preimage that does not describe this spend "+
			"(the script it commits to is right, so the difference is elsewhere: value, outputs, "+
			"sequence or locktime)", input, txid, len(unlock.Preimage)))
	return false
}

// preimageScriptCode pulls the scriptCode field out of a BIP-143 sighash
// preimage.
//
// The layout is fixed and defined by BIP-143:
//
//	 4  nVersion
//	32  hashPrevouts
//	32  hashSequence
//	36  outpoint
//	??  scriptCode, length-prefixed as a varint
//	 8  value of the output being spent
//	 4  nSequence
//	32  hashOutputs
//	 4  nLockTime
//	 4  sighash type
//
// which puts the scriptCode's length prefix at a constant offset of 104 bytes.
func preimageScriptCode(preimage []byte) ([]byte, error) {
	const scriptCodeOffset = 4 + 32 + 32 + 36
	// value, nSequence, hashOutputs, nLockTime, sighash type
	const trailer = 8 + 4 + 32 + 4 + 4

	if len(preimage) <= scriptCodeOffset+trailer {
		return nil, fmt.Errorf("%d bytes is too short to be a sighash preimage", len(preimage))
	}
	n, size := varInt(preimage[scriptCodeOffset:])
	if size == 0 {
		return nil, errors.New("sighash preimage has an unreadable scriptCode length")
	}
	start := scriptCodeOffset + size
	end := start + int(n) //nolint:gosec // bounded immediately below
	if n > uint64(len(preimage)) || end+trailer != len(preimage) {
		return nil, fmt.Errorf(
			"sighash preimage claims a %d-byte scriptCode, which does not fit its %d bytes", n, len(preimage))
	}
	return preimage[start:end], nil
}

// varInt reads a Bitcoin variable-length integer, returning the value and how
// many bytes it occupied, or 0 bytes if it is truncated.
func varInt(b []byte) (uint64, int) {
	if len(b) == 0 {
		return 0, 0
	}
	switch {
	case b[0] < 0xfd:
		return uint64(b[0]), 1
	case b[0] == 0xfd && len(b) >= 3:
		return uint64(b[1]) | uint64(b[2])<<8, 3
	case b[0] == 0xfe && len(b) >= 5:
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16 | uint64(b[4])<<24, 5
	case b[0] == 0xff && len(b) >= 9:
		var n uint64
		for i := 8; i >= 1; i-- {
			n = n<<8 | uint64(b[i])
		}
		return n, 9
	}
	return 0, 0
}

// missingBytes marks evidence that could not be obtained, as opposed to
// evidence that failed. One is a gap in what the audit saw; the other is a
// finding about the deployment, and reporting a pruned transaction as a failure
// would be a lie about which.
type missingBytes struct{ err error }

func (m missingBytes) Error() string { return m.err.Error() }
func (m missingBytes) Unwrap() error { return m.err }

// tx fetches a transaction and proves it is the one that was asked for.
//
// The hash check is the load-bearing line. The bytes come from a database this
// process opened by path or from a network service, neither of which is
// authenticated; the txid is what the record actually committed to. Bytes that
// do not hash to it are not this transaction, however plausible they look.
func (a *auditor) tx(ctx context.Context, txid string) (*transaction.Transaction, error) {
	if tx, ok := a.txs[txid]; ok {
		return tx, nil
	}
	if tx, ok := a.prev[txid]; ok {
		a.txs[txid] = tx
		return tx, nil
	}

	raw, err := a.source.RawTx(ctx, txid)
	if err != nil {
		return nil, missingBytes{err}
	}
	if len(raw) == 0 {
		return nil, missingBytes{fmt.Errorf("no bytes for transaction %s", txid)}
	}
	tx, err := transaction.NewTransactionFromBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("parse transaction %s: %w", txid, err)
	}
	if got := tx.TxID().String(); got != txid {
		return nil, fmt.Errorf("bytes offered for %s actually hash to %s", txid, got)
	}
	a.txs[txid] = tx
	return tx, nil
}

// report files a transaction-fetch error as a gap or a failure depending on
// which it is.
func (a *auditor) report(gen uint64, cell int, check Check, err error) {
	var missing missingBytes
	if errors.As(err, &missing) {
		a.gap(gen, cell, err.Error()+" — no bytes, so nothing about this transaction can be checked")
		return
	}
	a.fail(gen, cell, check, err.Error())
}

func (a *auditor) load(ctx context.Context, number uint64) (*generation, error) {
	// One generation at a time, deliberately. The alternative — one Load over
	// the whole range — holds every row and every txid of an arbitrarily long
	// audit in memory at once, and the audit only ever looks at two generations.
	gens, err := a.store.Load(ctx, number, 1)
	if err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	// Load returns the first generation at or after the one asked for, so a
	// number that comes back different means this one was never recorded.
	if len(gens) == 0 || gens[0].Number != number {
		return nil, nil
	}

	row, err := ca.SeedHex(a.cells, gens[0].RowHex)
	if err != nil {
		return nil, fmt.Errorf("audit: generation %d's recorded row: %w", number, err)
	}
	g := &generation{number: number, row: row, cellTx: make(map[int]history.CellTx, a.cells)}
	for _, c := range gens[0].Cells {
		if c.Cell < 0 || c.Cell >= a.cells {
			continue
		}
		g.cellTx[c.Cell] = c
	}
	return g, nil
}

func (a *auditor) pass(c Check) { a.rep.Passed[c]++ }

func (a *auditor) fail(gen uint64, cell int, check Check, detail string) {
	a.rep.Failures = append(a.rep.Failures, Failure{
		Generation: gen, Cell: cell, Check: check, Detail: detail,
	})
}

func (a *auditor) gap(gen uint64, cell int, reason string) {
	a.rep.Gaps = append(a.rep.Gaps, Gap{Generation: gen, Cell: cell, Reason: reason})
}

// stop reports whether the failure budget is spent.
func (a *auditor) stop() bool {
	return a.opts.MaxFailures > 0 && len(a.rep.Failures) >= a.opts.MaxFailures
}

func bit(row ca.Row, i int) int {
	if row.Get(i) {
		return 1
	}
	return 0
}

// outpoints renders what a transaction actually spends, for the failure message
// that says it did not spend what it should have.
func outpoints(tx *transaction.Transaction) string {
	out := make([]string, 0, len(tx.Inputs))
	for _, in := range tx.Inputs {
		if in.SourceTXID == nil {
			out = append(out, "<no source>")
			continue
		}
		out = append(out, fmt.Sprintf("%s:%d", in.SourceTXID.String(), in.SourceTxOutIndex))
	}
	if len(out) == 0 {
		return "nothing"
	}
	return joinAnd(out)
}

func joinAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return fmt.Sprintf("%s and %s", strings.Join(parts[:len(parts)-1], ", "), parts[len(parts)-1])
}
