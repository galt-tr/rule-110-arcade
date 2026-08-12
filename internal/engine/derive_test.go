package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// testCells is the ring these tests use. Small, because compiling the contract
// dominates their runtime and the ring size changes nothing about derivation —
// every cell is derived independently.
const testCells = 8

// countingLedger is a wallet made of maps that counts what was asked of it, so
// a test can assert that derivation stays bounded rather than merely correct.
type countingLedger struct {
	raw      map[string][]byte
	rawCalls atomic.Int64
	rawErr   error
}

func (l *countingLedger) RawTx(_ context.Context, txid string) ([]byte, error) {
	l.rawCalls.Add(1)
	if l.rawErr != nil {
		return nil, l.rawErr
	}
	raw, ok := l.raw[txid]
	if !ok {
		return nil, errors.New("fake: no such transaction")
	}
	return raw, nil
}

func (l *countingLedger) CellActions(context.Context, int, int) ([]chain.CellAction, int, error) {
	return nil, 0, errors.New("fake: not used by derivation")
}

// a deployment plus the genesis transaction every cell starts from.
type fixture struct {
	compiled *cellscript.Compiled
	facts    *chain.Deployment
	store    *history.Store
	ledger   *countingLedger
	seed     ca.Row
	genesis  *transaction.Transaction
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	compiled, err := cellscript.Compile(testCells, ca.Rule110)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	seed, err := ca.SeedSingle(testCells)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	store, err := history.Open(t.Context(), "", t.TempDir(), 0)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	gen := transaction.NewTransaction()
	src, err := chainhash.NewHashFromHex(strings.Repeat("f0", 32))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	gen.Inputs = append(gen.Inputs, &transaction.TransactionInput{SourceTXID: src})
	for cell := range testCells {
		lock, err := compiled.LockingScript(cell, seed)
		if err != nil {
			t.Fatalf("locking script: %v", err)
		}
		gen.AddOutput(&transaction.TransactionOutput{
			Satoshis: 1, LockingScript: script.NewFromBytes(lock),
		})
	}

	f := &fixture{
		compiled: compiled,
		store:    store,
		seed:     seed,
		genesis:  gen,
		ledger:   &countingLedger{raw: map[string][]byte{gen.TxID().String(): gen.Bytes()}},
		facts: &chain.Deployment{
			Cells: testCells, Rule: ca.Rule110,
			GenesisTxID: gen.TxID().String(), SeedHex: seed.Hex(), CellSatoshis: 1,
		},
	}
	if err := store.RecordGeneration(t.Context(), 0, seed.Hex()); err != nil {
		t.Fatalf("record generation: %v", err)
	}
	return f
}

// advance records a real transition for one cell and registers its bytes, so
// derivation has something to find.
func (f *fixture) advance(t *testing.T, cell int, from chain.CellChain, status history.Status) chain.CellChain {
	t.Helper()
	next := f.build(t, cell, from, 0)
	if err := f.store.RecordGeneration(t.Context(), next.Generation, next.RowHex); err != nil {
		t.Fatalf("record generation: %v", err)
	}
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Generation: next.Generation, Cell: cell, TxID: next.TxID, Status: status,
	}); err != nil {
		t.Fatalf("record tx: %v", err)
	}
	return next
}

// build makes a real transition for one cell and registers its bytes with the
// ledger, telling the history store NOTHING.
//
// Recovery reasons from transactions the record does not know about — that is the
// whole damage class — so a test needs to be able to produce one.
//
// salt goes into the locktime and changes nothing else. It is how two DIFFERENT
// transactions that spend the same output are built: the phantom and the
// transition that really won, which is the shape a lost write-ahead record leaves
// behind.
func (f *fixture) build(t *testing.T, cell int, from chain.CellChain, salt uint32) chain.CellChain {
	t.Helper()
	row, err := from.Row(testCells)
	if err != nil {
		t.Fatalf("row: %v", err)
	}
	next := ca.Rule110.Step(row)
	lock, err := f.compiled.LockingScript(cell, next)
	if err != nil {
		t.Fatalf("locking script: %v", err)
	}
	src, err := chainhash.NewHashFromHex(from.TxID)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	change, err := script.NewFromHex("76a914" + strings.Repeat("33", 20) + "88ac")
	if err != nil {
		t.Fatalf("change script: %v", err)
	}

	tx := transaction.NewTransaction()
	tx.LockTime = salt
	tx.Inputs = append(tx.Inputs, &transaction.TransactionInput{
		SourceTXID: src, SourceTxOutIndex: from.Vout,
	})
	tx.AddOutput(&transaction.TransactionOutput{
		Satoshis: from.Satoshis, LockingScript: script.NewFromBytes(lock),
	})
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 999, LockingScript: change})

	f.ledger.raw[tx.TxID().String()] = tx.Bytes()
	return chain.CellChain{
		Cell: cell, TxID: tx.TxID().String(), Vout: 0, Satoshis: from.Satoshis,
		Generation: from.Generation + 1, RowHex: next.Hex(),
	}
}

func (f *fixture) genesisTip(cell int) chain.CellChain {
	return chain.CellChain{
		Cell: cell, TxID: f.facts.GenesisTxID, Vout: uint32(cell), //nolint:gosec // test ring
		Satoshis: 1, Generation: 0, RowHex: f.seed.Hex(),
	}
}

func (f *fixture) derive(t *testing.T) []CellPosition {
	t.Helper()
	positions, err := DeriveTips(t.Context(), f.ledger, f.compiled, f.facts, f.store)
	if err != nil {
		t.Fatalf("DeriveTips: %v", err)
	}
	return positions
}

// A deployment whose store has nothing but generation 0 derives every cell to
// its own genesis output — which is the ONE case where getting it wrong
// re-spends 128 live UTXOs, so it has to come from the genesis transaction's
// real outputs rather than an assumption about their order.
func TestDeriveGenesisTipsWithNoHistory(t *testing.T) {
	f := newFixture(t)
	positions := f.derive(t)

	for cell, p := range positions {
		if p.Halted {
			t.Errorf("cell %d halted on a fresh deployment: %s", cell, p.HaltReason)
		}
		if p.Tip.TxID != f.facts.GenesisTxID || p.Tip.Generation != 0 {
			t.Errorf("cell %d tip = %+v, want generation 0 on the genesis transaction", cell, p.Tip)
		}
		if p.Tip.Vout != uint32(cell) { //nolint:gosec // test ring
			t.Errorf("cell %d derived vout %d, want %d", cell, p.Tip.Vout, cell)
		}
	}
}

// TestFailedNewestRecordHaltsTheCell is the regression test for bug 9a, which
// was destroying cells on the live deployment.
//
// A rejection used to halt the cell in memory only, while recordCell had already
// advanced the recorded tip to the REJECTED transaction's output. A restart lost
// the halt but not the phantom tip, so the cell resumed and spent an output that
// had never existed — every generation, forever, each rejection looking like a
// fresh independent failure.
//
// The halt now comes from the same records the tip comes from, so the two cannot
// disagree: a cell whose newest record is `failed` is halted, and its tip stays
// at the last transaction that was actually broadcast.
func TestFailedNewestRecordHaltsTheCell(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	// Cell 3 advances once, then its next generation is rejected.
	good := f.advance(t, 3, f.genesisTip(3), history.StatusBroadcast)
	rejected := f.advance(t, 3, good, history.StatusBroadcast)
	if err := f.store.RecordTx(ctx, history.CellTx{
		Generation: rejected.Generation, Cell: 3, TxID: rejected.TxID,
		Status: history.StatusFailed, Err: "arcade: REJECTED: utxo already spent",
	}); err != nil {
		t.Fatalf("record rejection: %v", err)
	}

	// ...and the process restarts.
	positions := f.derive(t)

	p := positions[3]
	if !p.Halted {
		t.Fatal("a cell rejected before the restart came back running; " +
			"it will spend an output that does not exist")
	}
	if p.Tip.TxID != good.TxID || p.Tip.Generation != good.Generation {
		t.Errorf("cell 3 tip = %+v, want the last BROADCAST transaction (%s at generation %d), "+
			"not the rejected one", p.Tip, good.TxID, good.Generation)
	}
	if !strings.Contains(p.HaltReason, "rejected") {
		t.Errorf("halt reason should say it was rejected, got: %q", p.HaltReason)
	}

	// Every other cell is untouched: one rejection halts one cell.
	for cell, other := range positions {
		if cell != 3 && other.Halted {
			t.Errorf("cell %d halted too; a rejection is per-cell", cell)
		}
	}
}

// TestASingleRejectionAboveTheTipIsUnchanged pins the ordinary cell against the
// change that let recovery examine a record further up.
//
// One refused transition directly above the tip is what almost every halted cell
// is, and nothing about it may have moved: the same tip, the same flags, the same
// transaction offered for examination, and the same sentence in the same words —
// asserted literally, because the halt reason is what an operator reads and it is
// now written by the same code that decides which row recovery acts on.
func TestASingleRejectionAboveTheTipIsUnchanged(t *testing.T) {
	f := newFixture(t)
	const cell = 3
	tip := f.advance(t, cell, f.genesisTip(cell), history.StatusMined)

	refused := f.build(t, cell, tip, 3)
	const why = "arcade: REJECTED: PROCESSING (4): failed to validate transaction"
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Generation: refused.Generation, Cell: cell, TxID: refused.TxID,
		Status: history.StatusFailed, Err: why,
	}); err != nil {
		t.Fatalf("record the refusal: %v", err)
	}

	p := f.derive(t)[cell]
	if !p.Halted || p.Unknown {
		t.Fatalf("cell %d = %+v, want halted by a rejection rather than flagged unknown", cell, p)
	}
	if p.Tip.TxID != tip.TxID || p.Tip.Generation != tip.Generation {
		t.Errorf("tip = %s at generation %d, want the last accepted record %s at %d",
			p.Tip.TxID, p.Tip.Generation, tip.TxID, tip.Generation)
	}
	if !p.Rejected || p.RejectionTxID != refused.TxID || p.RejectionErr != why {
		t.Errorf("cell %d = %+v, want the refusal offered for examination", cell, p)
	}
	if p.RejectedAt != tip.Generation+1 || p.RejectedAt != refused.Generation {
		t.Errorf("RejectedAt = %d, want the tip's own successor at %d", p.RejectedAt, refused.Generation)
	}
	want := fmt.Sprintf(
		"generation %d was rejected, so this cell's chain ends at generation %d: %s",
		refused.Generation, tip.Generation, why)
	if p.HaltReason != want {
		t.Errorf("the halt reason for the ordinary case changed.\n got: %q\nwant: %q", p.HaltReason, want)
	}
}

// An unresolved write-ahead record means the tip is UNKNOWN rather than merely
// stale, so the cell is halted AND flagged for recovery — and the tip reported
// is the last one actually broadcast.
func TestUnresolvedAttemptMarksTheTipUnknown(t *testing.T) {
	f := newFixture(t)
	good := f.advance(t, 5, f.genesisTip(5), history.StatusBroadcast)
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Generation: good.Generation + 1, Cell: 5, Status: history.StatusAttempting,
	}); err != nil {
		t.Fatalf("record attempt: %v", err)
	}

	p := f.derive(t)[5]
	if !p.Unknown || !p.Halted {
		t.Fatalf("cell 5 = %+v, want halted with an unknown tip", p)
	}
	if p.Attempted != good.Generation+1 {
		t.Errorf("attempted generation = %d, want %d", p.Attempted, good.Generation+1)
	}
	if p.Tip.Generation != good.Generation {
		t.Errorf("tip = generation %d, want the last broadcast one at %d",
			p.Tip.Generation, good.Generation)
	}
}

// TestDerivationIsBounded covers the cost, not the correctness.
//
// The query this replaces, UnresolvedAttempts, grouped over the whole of
// cell_txs — a table that grows by `cells` rows every generation — on every
// startup, before the HTTP server came up. Two things now bound the work:
// CellTips reads at most `depth` rows per cell through cell_txs_cell_gen, and
// the raw-transaction fetch is deduplicated, so a fresh ring of 128 cells
// sharing one genesis transaction costs one lookup rather than 128.
func TestDerivationIsBounded(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	// A long-running cell: 20,000 settled generations behind its tip.
	for gen := uint64(100); gen < 20100; gen++ {
		if err := f.store.RecordTx(ctx, history.CellTx{
			Generation: gen, Cell: 0, TxID: "old", Status: history.StatusMined,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	tips, err := f.store.CellTips(ctx, testCells, tipDepth)
	if err != nil {
		t.Fatalf("CellTips: %v", err)
	}
	if got := len(tips[0]); got != tipDepth {
		t.Errorf("CellTips returned %d rows for a cell with 20,000, want at most %d — "+
			"the read must be bounded by depth, not by how long the automaton has run", got, tipDepth)
	}
	if got := tips[0][0].Generation; got != 20099 {
		t.Errorf("newest row is generation %d, want 20099 (newest first)", got)
	}

	// And the fetch is deduplicated: every cell of a fresh ring points at the
	// same genesis transaction.
	fresh := newFixture(t)
	fresh.derive(t)
	if got := fresh.ledger.rawCalls.Load(); got != 1 {
		t.Errorf("derivation made %d raw-transaction lookups for %d cells all on the genesis "+
			"transaction, want 1", got, testCells)
	}
}

// TestLegacyStateAheadOfHistoryRefusesToStart is the migration guard.
//
// The live state.json holds tips around generation 837. If the history store is
// behind it — an older deployment, a restored database, a store that was wiped —
// derivation concludes "no records, so generation 0" and the automaton re-spends
// 128 outputs that were consumed hundreds of generations ago. Every one is
// rejected and every cell dies at once, so this must be refused rather than
// reported.
func TestLegacyStateAheadOfHistoryRefusesToStart(t *testing.T) {
	f := newFixture(t)
	positions := f.derive(t)

	legacy := []chain.CellChain{
		{Cell: 2, TxID: strings.Repeat("aa", 32), Generation: 837},
		{Cell: 6, TxID: strings.Repeat("bb", 32), Generation: 806},
	}
	err := CheckMigrationFloor(t.Context(), f.store, nil, positions, legacy)
	if err == nil {
		t.Fatal("started with the history store hundreds of generations behind state.json; " +
			"every cell would re-spend a spent output")
	}
	for _, want := range []string{"cell 2", "cell 6", "import-tips"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q, got: %v", want, err)
		}
	}

	// Deriving AHEAD of the legacy file is expected — it was written on a timer
	// and is routinely stale — and must not be refused.
	if err := CheckMigrationFloor(t.Context(), f.store, nil, positions, []chain.CellChain{
		{Cell: 2, TxID: strings.Repeat("aa", 32), Generation: 0},
	}); err != nil {
		t.Errorf("refused a legacy file that is merely stale: %v", err)
	}
}

// TestLegacyTipThatWasRejectedIsNotAFloor covers the case that actually occurred
// on the live deployment, and that the floor check as first written would have
// wedged permanently.
//
// state.json was produced by a build whose recordCell advanced a cell's tip when
// a transition was BROADCAST, not when it was accepted. So every cell halted by
// an arcade rejection carries the rejected transaction as its recorded tip —
// generation 994 on cell 12, where 991 was the last one mined. Derivation
// correctly falls back to 991. Naively that reads as "the store is 3 generations
// behind the file", which is the exact shape of the catastrophe the floor check
// exists to prevent, so it refuses to start; and the remedy it names,
// `import-tips`, would then write the rejected transaction in as the tip and
// point the cell at an outpoint that does not exist.
//
// A floor is only a floor if the transaction under it has an output.
func TestLegacyTipThatWasRejectedIsNotAFloor(t *testing.T) {
	f := newFixture(t)
	positions := f.derive(t)

	phantom := strings.Repeat("cc", 32)
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Cell: 2, Generation: 994, TxID: phantom, Status: history.StatusFailed,
		Err: "arcade: REJECTED: PROCESSING (4)",
	}); err != nil {
		t.Fatalf("record the rejection: %v", err)
	}

	legacy := []chain.CellChain{{Cell: 2, TxID: phantom, Generation: 994}}
	if err := CheckMigrationFloor(t.Context(), f.store, nil, positions, legacy); err != nil {
		t.Errorf("refused to start because the legacy file points at a transaction the record says "+
			"was REJECTED; that transaction has no output, so it is not a floor: %v", err)
	}

	// The guard must still fire for a legacy tip that was genuinely accepted —
	// otherwise this fix has removed the protection instead of sharpening it.
	real := strings.Repeat("dd", 32)
	if err := f.store.RecordTx(t.Context(), history.CellTx{
		Cell: 3, Generation: 994, TxID: real, Status: history.StatusMined,
	}); err != nil {
		t.Fatalf("record the mined tip: %v", err)
	}
	if err := CheckMigrationFloor(t.Context(), f.store, nil, positions,
		[]chain.CellChain{{Cell: 3, TxID: real, Generation: 994}}); err == nil {
		t.Error("started with the store behind a legacy tip that was MINED; that output really is spent")
	}
}

// TestCascadedRejectionsStillYieldATip reproduces the shape cells 34, 51 and 91
// of the live automaton are in, and that the shallow derivation window reported
// as unrecoverable.
//
// The build that wrote this deployment's older history advanced a cell's tip
// when a transition was BROADCAST rather than when it was accepted. So one
// rejection cascaded: the cell's next attempt spent the rejected transaction's
// output, which does not exist, and was refused in turn — about 170 times over,
// on top of a last good transaction near generation 300.
//
// A window of 8 records sees nothing but failures and concludes the tip cannot
// be derived, which reads as "this cell is lost". It is not. A long run of
// rejections is a cascade with a known cause, not ambiguity, and the newest
// ACCEPTED record is the tip no matter how much wreckage is stacked on it.
func TestCascadedRejectionsStillYieldATip(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	const cell, lastGood = 2, 5
	tip := f.genesisTip(cell)
	for range lastGood {
		tip = f.advance(t, cell, tip, history.StatusMined)
	}

	// Bury it under far more consecutive rejections than tipDepth can see.
	for gen := uint64(lastGood + 1); gen <= lastGood+170; gen++ {
		if err := f.store.RecordTx(ctx, history.CellTx{
			Cell: cell, Generation: gen, TxID: strings.Repeat("0", 58) + fmt.Sprintf("%06x", gen),
			Status: history.StatusFailed, Err: "arcade: REJECTED: PROCESSING (4)",
		}); err != nil {
			t.Fatalf("record rejection at %d: %v", gen, err)
		}
	}

	positions := f.derive(t)
	got := positions[cell]

	if !got.Halted {
		t.Fatal("a cell under 170 rejections must not advance")
	}
	if got.Tip.Generation != lastGood {
		t.Errorf("derived tip generation %d, want %d — the newest ACCEPTED record is the tip "+
			"however deep the wreckage above it goes", got.Tip.Generation, lastGood)
	}
	if got.Unknown {
		t.Error("rejections are not ambiguity: the tip is known, so Unknown must stay clear")
	}
	if strings.Contains(got.HaltReason, "cannot be derived") {
		t.Errorf("the halt still reports the cell as underivable, which reads as lost: %q", got.HaltReason)
	}
}

// A tip whose bytes cannot be found anywhere is a tip that cannot be verified,
// and an unverified tip must never be spent from.
func TestDerivationRefusesWhenBytesAreMissing(t *testing.T) {
	f := newFixture(t)
	f.ledger.rawErr = errors.New("wallet unreachable")

	if _, err := DeriveTips(t.Context(), f.ledger, f.compiled, f.facts, f.store); err == nil {
		t.Fatal("derived tips without being able to read a single transaction")
	}
}

// The seed reproduces every row that has ever existed, so a store whose recorded
// row disagrees with it means the deployment file does not describe the chain
// that ran — and every script built from here would be for the wrong row, which
// fails on chain as a bare OP_CHECKSIGVERIFY with no hint why.
func TestDerivationRefusesARowDisagreement(t *testing.T) {
	f := newFixture(t)

	// Generation 1 was recorded as something the seed does not produce.
	if err := f.store.RecordGeneration(t.Context(), 1, "ff"); err != nil {
		t.Fatalf("record generation: %v", err)
	}
	f.advance(t, 1, f.genesisTip(1), history.StatusBroadcast)

	_, err := DeriveTips(t.Context(), f.ledger, f.compiled, f.facts, f.store)
	if err == nil {
		t.Fatal("started with the store's rows disagreeing with the seed")
	}
	if !strings.Contains(err.Error(), "seed") {
		t.Errorf("the refusal should point at the seed, got: %v", err)
	}
}

// A cell buried under records that name no broadcast transaction has no
// derivable tip — but it must halt that CELL, not the deployment.
//
// Failing the whole derivation would also fail `rule110 recover`, which derives
// before it can decide anything, leaving an operator with no way to inspect the
// problem at all. The other cells are unaffected and the buried one is halted,
// which is what stops its zero tip from ever being spent.
func TestUnderivableCellHaltsOnlyItself(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	// Cell 7's whole visible history is failures, with nothing broadcast below.
	for gen := uint64(1); gen <= tipDepth+2; gen++ {
		if err := f.store.RecordTx(ctx, history.CellTx{
			Generation: gen, Cell: 7, Status: history.StatusFailed, Err: "never got out",
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	positions := f.derive(t)

	p := positions[7]
	if !p.Halted {
		t.Fatal("a cell with no derivable tip came back running")
	}
	if p.Unknown {
		t.Error("there is nothing here for recovery to reason from; it must not be offered as a candidate")
	}
	if p.Tip.TxID != "" {
		t.Errorf("a tip was invented for an underivable cell: %+v", p.Tip)
	}
	for cell, other := range positions {
		if cell != 7 && other.Halted {
			t.Errorf("cell %d halted too; one buried cell must not stop the ring", cell)
		}
	}
}
