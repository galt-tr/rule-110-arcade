package audit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// testCells is the ring these tests run on. Eight is the smallest legal ring and
// still exercises the wrap — cell 0's right neighbour is cell 7 — while keeping
// the contract compilation that dominates their runtime to one per test.
const (
	testCells = 8
	testSats  = 1000
	testFee   = 3500
)

// source is a wallet made of bytes: exactly the interface the audit needs, and
// nothing that could quietly make the audit right for the wrong reason.
type source struct {
	raw map[string][]byte
}

func (s *source) RawTx(_ context.Context, txid string) ([]byte, error) {
	raw, ok := s.raw[txid]
	if !ok {
		return nil, errors.New("fake: no such transaction")
	}
	return raw, nil
}

// tip is where one cell's chain has got to, as the harness tracks it.
type tip struct {
	txid string
	vout uint32
	row  ca.Row
	sats uint64
}

// harness builds a synthetic deployment: a real genesis transaction, real cell
// transitions with real unlocking scripts, and a real history store to record
// them in. The transactions are built by cellscript, so what the audit decodes
// is what the engine would actually have broadcast.
type harness struct {
	compiled *cellscript.Compiled
	store    *history.Store
	source   *source
	seed     ca.Row

	// recorded is the row the STORE holds for the newest generation, derived
	// from its predecessor. It is deliberately tracked apart from the per-cell
	// tips, because the gap between the two is the thing under test: a cell can
	// carry a row the record does not describe, and Script will not object.
	recorded ca.Row
	gen      uint64
	tips     map[int]tip

	// built keeps each cell's newest transition as it was assembled, with its
	// source outputs still attached. Re-parsing the bytes loses those, and
	// without them the script interpreter has nothing to verify against — which
	// matters because one test's whole point is that the chain ACCEPTS the
	// transaction the audit rejects.
	built map[int]*transaction.Transaction
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	compiled, err := cellscript.Compile(testCells, ca.Rule110)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	seed, err := ca.SeedSingle(testCells)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	store, err := history.Open(t.Context(), "", t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Genesis: one transaction, one output per cell, in cell order.
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
			Satoshis: testSats, LockingScript: script.NewFromBytes(lock),
		})
	}

	h := &harness{
		compiled: compiled,
		store:    store,
		source:   &source{raw: map[string][]byte{gen.TxID().String(): gen.Bytes()}},
		seed:     seed,
		recorded: seed,
		tips:     map[int]tip{},
		built:    map[int]*transaction.Transaction{},
	}
	for cell := range testCells {
		h.tips[cell] = tip{txid: gen.TxID().String(), vout: uint32(cell), row: seed, sats: testSats} //nolint:gosec // test ring
	}

	if err := store.RecordGeneration(t.Context(), 0, seed.Hex()); err != nil {
		t.Fatalf("record generation: %v", err)
	}
	for cell := range testCells {
		if err := store.RecordTx(t.Context(), history.CellTx{
			Generation: 0, Cell: cell, TxID: gen.TxID().String(), Status: history.StatusSeen,
		}); err != nil {
			t.Fatalf("record tx: %v", err)
		}
	}
	return h
}

// advance moves every cell forward one generation.
//
// present, when set, is the row a cell actually puts on chain — which is where
// a divergence can be injected. Each cell derives its own successor from the row
// its own UTXO carries, exactly as chain.AdvanceCell does, so a cell that was
// fed a divergent row once goes on carrying the consequences.
func (h *harness) advance(t *testing.T, present func(cell int, next ca.Row) ca.Row) {
	t.Helper()

	number := h.gen + 1
	recorded := ca.Rule110.Step(h.recorded)
	if err := h.store.RecordGeneration(t.Context(), number, recorded.Hex()); err != nil {
		t.Fatalf("record generation %d: %v", number, err)
	}

	for cell := range testCells {
		from := h.tips[cell]
		next := ca.Rule110.Step(from.row)
		if present != nil {
			next = present(cell, next)
		}
		tx := h.transition(t, cell, from, next)
		txid := tx.TxID().String()
		h.source.raw[txid] = tx.Bytes()
		if err := h.store.RecordTx(t.Context(), history.CellTx{
			Generation: number, Cell: cell, TxID: txid, Status: history.StatusMined,
		}); err != nil {
			t.Fatalf("record cell %d: %v", cell, err)
		}
		h.tips[cell] = tip{txid: txid, vout: 0, row: next, sats: from.sats}
		h.built[cell] = tx
	}

	h.recorded = recorded
	h.gen = number
}

// transition builds one cell's spend, shaped the way the toolbox builds them:
// the contract input first, a funding input after it, the continuation output at
// vout 0 and one P2PKH change output at vout 1.
func (h *harness) transition(t *testing.T, cell int, from tip, next ca.Row) *transaction.Transaction {
	t.Helper()

	lockCur, err := h.compiled.LockingScript(cell, from.row)
	if err != nil {
		t.Fatalf("locking script: %v", err)
	}
	lockNext, err := h.compiled.LockingScript(cell, next)
	if err != nil {
		t.Fatalf("locking script: %v", err)
	}
	srcTxid, err := chainhash.NewHashFromHex(from.txid)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	tx := transaction.NewTransaction()
	tx.AddInputWithOutput(&transaction.TransactionInput{
		SourceTXID:       srcTxid,
		SourceTxOutIndex: from.vout,
		SequenceNumber:   transaction.DefaultSequenceNumber,
	}, &transaction.TransactionOutput{Satoshis: from.sats, LockingScript: script.NewFromBytes(lockCur)})

	// A funding input, unique per cell and generation so no two transitions can
	// collide into the same txid.
	fuel, err := chainhash.NewHashFromHex(fmt.Sprintf("%02x%s", cell, strings.Repeat("22", 31)))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	fuelScript, err := script.NewFromHex("76a914" + strings.Repeat("cd", 20) + "88ac")
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	tx.AddInputWithOutput(&transaction.TransactionInput{
		SourceTXID:       fuel,
		SourceTxOutIndex: uint32(h.gen), //nolint:gosec // test generations are small
		SequenceNumber:   transaction.DefaultSequenceNumber,
	}, &transaction.TransactionOutput{Satoshis: 5000, LockingScript: fuelScript})

	tx.AddOutput(&transaction.TransactionOutput{
		Satoshis: from.sats, LockingScript: script.NewFromBytes(lockNext),
	})
	changeScript, err := script.NewFromHex("76a914" + strings.Repeat("ab", 20) + "88ac")
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: testFee, LockingScript: changeScript})

	unlock, err := h.compiled.UnlockingScript(cellscript.Spend{
		CellIndex:    cell,
		CurrentRow:   from.row,
		NextRow:      next,
		Tx:           tx,
		InputIndex:   0,
		PrevSatoshis: from.sats,
		ChangePKH:    bytes.Repeat([]byte{0xab}, 20),
		ChangeAmount: testFee,
		NewAmount:    from.sats,
	})
	if err != nil {
		t.Fatalf("unlocking script for cell %d: %v", cell, err)
	}
	tx.Inputs[0].UnlockingScript = script.NewFromBytes(unlock)
	return tx
}

func (h *harness) audit(t *testing.T, opts Options) *Report {
	t.Helper()
	if opts.To == 0 && opts.From == 0 {
		opts.To = h.gen
	}
	rep, err := Run(t.Context(), h.compiled, h.store, h.source, opts)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	return rep
}

// failuresOf returns the failures for one check, so a test can assert on the
// finding it is about without being brittle about the others.
func failuresOf(rep *Report, check Check) []Failure {
	var out []Failure
	for _, f := range rep.Failures {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

func requireFailure(t *testing.T, rep *Report, check Check, gen uint64, cell int) Failure {
	t.Helper()
	for _, f := range failuresOf(rep, check) {
		if f.Generation == gen && f.Cell == cell {
			return f
		}
	}
	t.Fatalf("expected a %q failure at generation %d cell %d; got:\n%s", check, gen, cell, dump(rep))
	return Failure{}
}

func dump(rep *Report) string {
	var b strings.Builder
	for _, f := range rep.Failures {
		fmt.Fprintf(&b, "  FAIL %s\n", f)
	}
	for _, g := range rep.Gaps {
		fmt.Fprintf(&b, "  GAP  %s\n", g)
	}
	if b.Len() == 0 {
		return "  (no failures, no gaps)"
	}
	return b.String()
}

// TestCleanHistoryPasses is the baseline: a history the engine would have
// produced passes every check, and the counts show the audit actually looked at
// everything rather than skipping quietly.
func TestCleanHistoryPasses(t *testing.T) {
	h := newHarness(t)
	const gens = 4
	for range gens {
		h.advance(t, nil)
	}

	rep := h.audit(t, Options{From: 0, To: h.gen, Seed: h.seed})

	if !rep.OK() {
		t.Fatalf("clean history failed its audit:\n%s", dump(rep))
	}
	if len(rep.Gaps) > 0 {
		t.Errorf("unexpected gaps:\n%s", dump(rep))
	}
	if want := gens * testCells; rep.Transactions != want+testCells {
		t.Errorf("checked %d transactions, want %d (%d generations plus genesis)",
			rep.Transactions, want+testCells, gens)
	}
	for _, check := range []Check{
		CheckLinkage, CheckCovenant, CheckNeighbourhood,
		CheckCarriedRow, CheckRule, CheckSuccessor,
	} {
		if got, want := rep.Passed[check], gens*testCells; got != want {
			t.Errorf("%q passed %d times, want %d", check, got, want)
		}
	}
	// Generation 0's row against the seed, plus one derivation per generation.
	if got, want := rep.Passed[CheckContinuity], gens+1; got != want {
		t.Errorf("row continuity passed %d times, want %d", got, want)
	}
	// Every transition plus every genesis output is attributed to its cell.
	if got, want := rep.Passed[CheckBinding], (gens+1)*testCells; got != want {
		t.Errorf("cell binding passed %d times, want %d", got, want)
	}
}

// TestNeighbourDisagreementIsCaught is the test this whole command exists for.
//
// Cell 3 is handed a row whose bit 4 — its LEFT neighbour — is wrong. Every
// other bit, including its own, is correct, so the covenant accepts it: the
// transaction below is checked with the same interpreter the engine uses before
// broadcasting, and it passes. A generation later cell 3 reads that wrong bit as
// its neighbourhood, and nothing on chain says a word about it.
//
// That is the hole. This is the test that it is now covered.
func TestNeighbourDisagreementIsCaught(t *testing.T) {
	h := newHarness(t)
	const victim = 3
	neighbour := ca.LeftIndex(testCells, victim)

	h.advance(t, nil)

	// The divergence is introduced here and read a generation later.
	h.advance(t, func(cell int, next ca.Row) ca.Row {
		if cell != victim {
			return next
		}
		bad := next.Clone()
		bad.Set(neighbour, !next.Get(neighbour))
		return bad
	})
	// The chain accepts it. This is not a hypothetical.
	if err := cellscript.VerifyInput(h.built[victim], 0); err != nil {
		t.Fatalf("the divergent transition was rejected by the covenant, so it is not the case "+
			"this test is about: %v", err)
	}

	h.advance(t, nil)

	rep := h.audit(t, Options{From: 0, To: h.gen, Seed: h.seed})
	if rep.OK() {
		t.Fatalf("the audit passed a history in which cell %d read cell %d's bit wrongly", victim, neighbour)
	}
	f := requireFailure(t, rep, CheckNeighbourhood, 3, victim)
	if !strings.Contains(f.Detail, fmt.Sprintf("left neighbour (cell %d)", neighbour)) {
		t.Errorf("failure does not name the neighbour it disagreed with: %s", f.Detail)
	}

	// And nothing else: the divergence is one bit in one cell, and an audit that
	// reported the whole generation broken would be no more useful than one that
	// reported nothing.
	if n := len(rep.Failures); n != 1 {
		t.Errorf("expected exactly one failure, got %d:\n%s", n, dump(rep))
	}
}

// TestDivergenceOutsideTheNeighbourhoodIsCaught covers the other half of the
// same hole. A bit that is not yet anyone's neighbourhood is still a row that
// the record does not describe, and it becomes a neighbourhood a few generations
// later — so it is reported, and reported as what it is.
func TestDivergenceOutsideTheNeighbourhoodIsCaught(t *testing.T) {
	h := newHarness(t)
	const victim = 3
	// Bit 6 is neither cell 3 nor either of its neighbours (2 and 4).
	const far = 6

	h.advance(t, func(cell int, next ca.Row) ca.Row {
		if cell != victim {
			return next
		}
		bad := next.Clone()
		bad.Set(far, !next.Get(far))
		return bad
	})
	h.advance(t, nil)

	rep := h.audit(t, Options{From: 1, To: h.gen})
	requireFailure(t, rep, CheckCarriedRow, 2, victim)
	if n := len(failuresOf(rep, CheckNeighbourhood)); n != 0 {
		t.Errorf("cell %d's neighbourhood was correct; %d cross-cell failures reported:\n%s",
			victim, n, dump(rep))
	}
}

// TestBrokenChainIsCaught covers the third property: a transaction that does not
// spend the cell's own previous output is not a continuation of that cell's
// chain, however well-formed it is otherwise.
func TestBrokenChainIsCaught(t *testing.T) {
	h := newHarness(t)
	h.advance(t, nil)
	h.advance(t, nil)

	// Rebuild cell 5's newest transition so that it spends something else. The
	// record still names it, which is exactly the situation worth catching: the
	// history says the cell advanced and the chain says it did not.
	const victim = 5
	old := h.tips[victim]
	tx := mustTx(t, h.source.raw[old.txid])
	elsewhere, err := chainhash.NewHashFromHex(strings.Repeat("9e", 32))
	if err != nil {
		t.Fatal(err)
	}
	tx.Inputs[0].SourceTXID = elsewhere
	delete(h.source.raw, old.txid)
	h.source.raw[tx.TxID().String()] = tx.Bytes()
	if err := h.store.RecordTx(t.Context(), history.CellTx{
		Generation: h.gen, Cell: victim, TxID: tx.TxID().String(), Status: history.StatusMined,
	}); err != nil {
		t.Fatal(err)
	}

	rep := h.audit(t, Options{From: 1, To: h.gen})
	f := requireFailure(t, rep, CheckLinkage, h.gen, victim)
	if !strings.Contains(f.Detail, "chain is broken") {
		t.Errorf("failure does not say the chain is broken: %s", f.Detail)
	}
	if n := len(rep.Failures); n != 1 {
		t.Errorf("expected exactly one failure, got %d:\n%s", n, dump(rep))
	}
}

// TestRecordedRowThatIsNotTheSuccessorIsCaught is the row-level check: the
// record's own claim about generation N is re-derived from generation N-1 rather
// than believed.
func TestRecordedRowThatIsNotTheSuccessorIsCaught(t *testing.T) {
	h := newHarness(t)

	// RecordGeneration is first-writer-wins, so writing a wrong row here is
	// enough to make the store disagree with the automaton it recorded.
	wrong, err := ca.NewRow(testCells)
	if err != nil {
		t.Fatal(err)
	}
	wrong.Set(0, true)
	wrong.Set(4, true)
	if err := h.store.RecordGeneration(t.Context(), 1, wrong.Hex()); err != nil {
		t.Fatal(err)
	}
	h.advance(t, nil)

	rep := h.audit(t, Options{From: 0, To: 1, Seed: h.seed})
	f := requireFailure(t, rep, CheckContinuity, 1, -1)
	if !strings.Contains(f.Detail, wrong.Hex()) {
		t.Errorf("failure does not quote the recorded row: %s", f.Detail)
	}
}

// TestSeedMismatchIsCaught anchors the whole derivation. Generation 0 has no
// predecessor, so the only thing it can be checked against is what the
// deployment says it was created with.
func TestSeedMismatchIsCaught(t *testing.T) {
	h := newHarness(t)
	h.advance(t, nil)

	other, err := ca.NewRow(testCells)
	if err != nil {
		t.Fatal(err)
	}
	other.Set(2, true)

	rep := h.audit(t, Options{From: 0, To: 0, Seed: other})
	requireFailure(t, rep, CheckContinuity, 0, -1)
}

// TestGenesisOutputsAreAttributed checks that generation 0's outputs really are
// one covenant per cell in cell order — the anchor every later generation's
// linkage is checked against.
func TestGenesisOutputsAreAttributed(t *testing.T) {
	h := newHarness(t)

	rep := h.audit(t, Options{From: 0, To: 0, Seed: h.seed})
	if !rep.OK() {
		t.Fatalf("genesis failed its audit:\n%s", dump(rep))
	}
	if got := rep.Passed[CheckBinding]; got != testCells {
		t.Errorf("attributed %d genesis outputs, want %d", got, testCells)
	}
}

// TestTamperedBytesAreRefused: the audit's evidence is bytes fetched from a
// database it opened by path. If those bytes are not the transaction the record
// committed to, every conclusion drawn from them is worthless — so they are
// hashed, and a mismatch is a failure rather than a shrug.
func TestTamperedBytesAreRefused(t *testing.T) {
	h := newHarness(t)
	h.advance(t, nil)

	const victim = 2
	tampered := bytes.Clone(h.source.raw[h.tips[victim].txid])
	// Flip a byte in the middle of the unlocking script.
	tampered[len(tampered)/2] ^= 0xff
	h.source.raw[h.tips[victim].txid] = tampered

	rep := h.audit(t, Options{From: 1, To: 1})
	requireFailure(t, rep, CheckIdentity, 1, victim)
}

// TestMissingEvidenceIsAGapNotAPass is the distinction the report turns on. A
// transaction whose bytes have been pruned proves nothing and disproves
// nothing, and an audit that counted it either way would be lying.
func TestMissingEvidenceIsAGapNotAPass(t *testing.T) {
	h := newHarness(t)
	h.advance(t, nil)

	const pruned = 6
	delete(h.source.raw, h.tips[pruned].txid)

	// A cell whose record says the network refused it, and a cell with no
	// record at all: both are gaps, neither is a failure.
	if err := h.store.RecordTx(t.Context(), history.CellTx{
		Generation: 1, Cell: 4, TxID: strings.Repeat("ab", 32), Status: history.StatusFailed,
	}); err != nil {
		t.Fatal(err)
	}

	rep := h.audit(t, Options{From: 1, To: 1})
	if !rep.OK() {
		t.Fatalf("missing evidence was reported as failure:\n%s", dump(rep))
	}
	if len(rep.Gaps) != 2 {
		t.Fatalf("expected 2 gaps, got %d:\n%s", len(rep.Gaps), dump(rep))
	}
	if rep.Transactions != testCells-2 {
		t.Errorf("checked %d transactions, want %d", rep.Transactions, testCells-2)
	}
}

// TestUnrecordedGenerationIsAGap: an audit whose range runs past the end of the
// record must say so rather than deriving from nothing.
func TestUnrecordedGenerationIsAGap(t *testing.T) {
	h := newHarness(t)
	h.advance(t, nil)

	rep := h.audit(t, Options{From: 1, To: 3})
	if !rep.OK() {
		t.Fatalf("unexpected failures:\n%s", dump(rep))
	}
	if rep.Generations != 1 {
		t.Errorf("examined %d generations, want 1", rep.Generations)
	}
	var missing int
	for _, g := range rep.Gaps {
		if g.Cell < 0 && strings.Contains(g.Reason, "no row recorded") {
			missing++
		}
	}
	if missing != 2 {
		t.Errorf("expected 2 unrecorded generations reported, got %d:\n%s", missing, dump(rep))
	}
}

// TestMaxFailuresStopsEarly bounds a run against a systematically broken
// deployment, and marks the report so its silence about later generations is not
// mistaken for a verdict on them.
func TestMaxFailuresStopsEarly(t *testing.T) {
	h := newHarness(t)
	// Every cell diverges, at a bit that is nobody's neighbourhood but its own
	// row: the whole generation is wrong in the same way.
	h.advance(t, func(_ int, next ca.Row) ca.Row {
		bad := next.Clone()
		bad.Set(7, !next.Get(7))
		return bad
	})
	h.advance(t, nil)

	rep := h.audit(t, Options{From: 1, To: h.gen, MaxFailures: 3})
	if len(rep.Failures) != 3 {
		t.Errorf("collected %d failures, want the 3 asked for:\n%s", len(rep.Failures), dump(rep))
	}
	if !rep.Truncated {
		t.Error("report is not marked truncated, so a reader would take it for a complete audit")
	}
}

// TestAuditRunsWhileTheEngineHoldsTheLease is a regression test for the whole
// point of the command being usable: it must never take the writer lease. An
// audit that required the automaton to be stopped would be an audit nobody runs.
func TestAuditRunsWhileTheEngineHoldsTheLease(t *testing.T) {
	h := newHarness(t)
	h.advance(t, nil)

	held, err := h.store.AcquireLease(t.Context(), "rule110-engine", "someone-else", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("could not take the lease as another writer")
	}

	rep := h.audit(t, Options{From: 0, To: h.gen, Seed: h.seed})
	if !rep.OK() {
		t.Fatalf("audit failed while the lease was held elsewhere:\n%s", dump(rep))
	}
}

// TestEmptyRangeIsRefused: a range that means nothing is a caller mistake, not
// an audit that passes vacuously.
func TestEmptyRangeIsRefused(t *testing.T) {
	h := newHarness(t)
	if _, err := Run(t.Context(), h.compiled, h.store, h.source, Options{From: 5, To: 2}); err == nil {
		t.Error("expected an error for an inverted range")
	}
}

// TestPreimageScriptCodeRoundTrip pins the one piece of hand-parsed binary in
// this package against a preimage that cellscript actually produced.
func TestPreimageScriptCodeRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.advance(t, nil)

	const cell = 1
	tx := mustTx(t, h.source.raw[h.tips[cell].txid])
	unlock, err := h.compiled.DecodeUnlocking(tx.Inputs[0].UnlockingScript.Bytes())
	if err != nil {
		t.Fatalf("decode unlocking script: %v", err)
	}
	got, err := preimageScriptCode(unlock.Preimage)
	if err != nil {
		t.Fatalf("read scriptCode out of the preimage: %v", err)
	}

	lock, err := h.compiled.LockingScript(cell, h.seed)
	if err != nil {
		t.Fatal(err)
	}
	want, err := h.compiled.ScriptCode(lock)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("scriptCode read out of the preimage is not the script being spent\n got %x\nwant %x",
			got, want)
	}
}

func TestVarIntRefusesTruncatedInput(t *testing.T) {
	for _, b := range [][]byte{nil, {0xfd}, {0xfd, 0x01}, {0xfe, 1, 2}, {0xff, 1}} {
		if _, size := varInt(b); size != 0 {
			t.Errorf("varInt(%x) claimed %d bytes from a truncated input", b, size)
		}
	}
	if n, size := varInt([]byte{0xfd, 0xb0, 0x04}); n != 1200 || size != 3 {
		t.Errorf("varInt(fdb004) = (%d, %d), want (1200, 3)", n, size)
	}
}

func mustTx(t *testing.T, raw []byte) *transaction.Transaction {
	t.Helper()
	tx, err := transaction.NewTransactionFromBytes(raw)
	if err != nil {
		t.Fatalf("parse transaction: %v", err)
	}
	return tx
}
