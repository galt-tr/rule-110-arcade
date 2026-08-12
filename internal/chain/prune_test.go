package chain

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/defs"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/headers"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/storage"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/storage/perfprovider"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/wdk"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/wdk/primitives"
)

// The fixture below builds a wallet database with the TOOLBOX'S OWN schema —
// perfprovider plus Migrate, the same two calls chain.Open makes — rather than a
// hand-written CREATE TABLE. That matters more here than anywhere else in this
// package: the pruner is raw SQL against tables it does not own, so a test
// against a schema we invented would pass while the real column names, types and
// foreign keys drifted underneath it.
//
// Everything after that is inserted directly, because getting rows into these
// tables through the wallet's public API needs an arcade instance and a funded
// key, and the pruner's subject is the rows, not how they got there.

const (
	testTipHeight  = 1000
	testUserID     = 1
	testIdentity   = "02" + "ab"
	testCellBasket = CellBasket
)

// walletFixture is a migrated wallet database plus the handles to poke at it.
type walletFixture struct {
	cfg      Config
	db       *sql.DB
	provider *storage.Provider
	postgres bool

	// nextTxRow is the transaction_id to use next. Assigned explicitly rather
	// than by the identity column so a test can reason about the ordering the
	// pruner's cursor and retention floor both depend on.
	nextTxRow int64
}

func newWalletFixture(t *testing.T, cells int) *walletFixture {
	t.Helper()
	return newWalletFixtureOn(t, cells, "")
}

// postgresDSNEnv names a throwaway PostgreSQL server the dialect-specific tests
// may use. It is opt-in because the SQLite path proves the logic and this proves
// only the SQL dialect — and because the obvious server to point it at is the
// one holding the live 13 GB wallet, which these tests must never touch.
// newWalletFixtureOn creates its own uniquely-named database and drops it again.
const postgresDSNEnv = "RULE110_TEST_POSTGRES_DSN"

// newWalletFixtureOn builds the fixture on SQLite, or on a freshly-created
// throwaway PostgreSQL database when adminDSN is non-empty.
func newWalletFixtureOn(t *testing.T, cells int, adminDSN string) *walletFixture {
	t.Helper()
	ctx := t.Context()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()

	var pgDSN string
	if adminDSN != "" {
		pgDSN = createThrowawayDB(t, adminDSN)
	}

	// Arcade and headers are constructed disabled and pointed at an
	// unresolvable host: Migrate touches neither, and a test must never reach
	// the network. The URL is only present because the constructor insists on
	// one even when disabled.
	oracle := arcade.New(logger, nil, defs.Arcade{Enabled: false})
	hdrs, err := headers.New(logger, defs.ChainTracks{Enabled: false, URL: "http://headers.invalid"})
	if err != nil {
		t.Fatalf("headers: %v", err)
	}
	pcfg := perfprovider.Config{
		Backend:     perfprovider.BackendSQLite,
		SQLitePath:  filepath.Join(dir, "wallet.db"),
		Network:     defs.NetworkTSTN,
		StorageName: storageName,
	}
	driver, target := "sqlite", sqliteWalletDSN(dir)
	if pgDSN != "" {
		pcfg.Backend, pcfg.PostgresDSN, pcfg.SQLitePath = perfprovider.BackendPostgres, pgDSN, ""
		driver, target = "pgx", pgDSN
	}
	provider, closeProvider, err := perfprovider.New(ctx, logger, pcfg, oracle, hdrs)
	if err != nil {
		t.Fatalf("perfprovider: %v", err)
	}
	if _, err := provider.Migrate(ctx, storageName, testIdentity); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = closeProvider(context.WithoutCancel(ctx)) })

	// Same locking posture as the wallet and the pruner — see sqliteWalletDSN.
	db, err := sql.Open(driver, target)
	if err != nil {
		t.Fatalf("open wallet: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.Cells = cells
	cfg.PostgresDSN = pgDSN
	w := &walletFixture{cfg: cfg, db: db, provider: provider, postgres: pgDSN != "", nextTxRow: 1}
	w.exec(t, `INSERT INTO users (user_id, identity_key, active_storage, created_at, updated_at)
	           `+w.overriding()+`VALUES (?, ?, ?, ?, ?) ON CONFLICT (user_id) DO NOTHING`,
		testUserID, testIdentity, storageName, w.encTime(time.Now()), w.encTime(time.Now()))

	// One mined transaction at testTipHeight, so the wallet has a recent block
	// to measure MinConfirmations against. Without it the tip would be whatever
	// the chain under test reached and every spender would look unburied, which
	// is a confusing way for an unrelated test to fail. Its own output is never
	// spent, so it is never prunable and never perturbs a count.
	w.insertTx(t, txSpec{mined: true, height: testTipHeight, outs: []outSpec{{basket: "default"}}})
	return w
}

// q rebinds ? placeholders for whichever engine the fixture is on, so one set of
// fixture SQL serves both. Same trick as history.Store.rebind.
func (w *walletFixture) q(query string) string {
	if !w.postgres {
		return query
	}
	var b strings.Builder
	n := 0
	for i := range len(query) {
		if query[i] == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteByte(query[i])
	}
	return b.String()
}

// encTime and encBool mirror sqlkit.EncTime and sqlkit.BoolVal: SQLite stores
// times as unix microseconds and booleans as 0/1, PostgreSQL takes both
// natively. The fixture writes rows the toolbox will read back, so it has to
// encode them the way the toolbox does.
func (w *walletFixture) encTime(tm time.Time) any {
	if w.postgres {
		return tm.UTC()
	}
	return tm.UTC().UnixMicro()
}

func (w *walletFixture) encBool(b bool) any {
	if w.postgres {
		return b
	}
	if b {
		return int64(1)
	}
	return int64(0)
}

// overriding is the clause an explicit primary key needs on PostgreSQL, where
// the identity columns are GENERATED ALWAYS. SQLite's INTEGER PRIMARY KEY takes
// the value directly and wants nothing. The fixture assigns ids itself so a test
// can reason about the ordering the cursor and the retention floor depend on.
func (w *walletFixture) overriding() string {
	if w.postgres {
		return "OVERRIDING SYSTEM VALUE "
	}
	return ""
}

func (w *walletFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := w.db.ExecContext(t.Context(), w.q(query), args...); err != nil {
		t.Fatalf("exec %.60s: %v", query, err)
	}
}

// createThrowawayDB makes a uniquely-named database on adminDSN's server and
// drops it when the test ends.
//
// Uniquely-named and dropped afterwards because the obvious server to run this
// against is the one holding the live wallet. Nothing here may touch an existing
// database: it CREATEs a name that cannot collide and DROPs exactly that name.
func createThrowawayDB(t *testing.T, adminDSN string) string {
	t.Helper()
	admin, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer func() { _ = admin.Close() }()

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	name := "rule110_test_" + hex.EncodeToString(suffix[:])
	if _, err := admin.ExecContext(t.Context(), `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	t.Cleanup(func() {
		db, err := sql.Open("pgx", adminDSN)
		if err != nil {
			t.Errorf("reopen admin connection to drop %s: %v", name, err)
			return
		}
		defer func() { _ = db.Close() }()
		// WITH (FORCE) so a connection the provider has not finished closing
		// cannot leave the throwaway database behind.
		if _, err := db.ExecContext(context.Background(),
			`DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("drop %s: %v", name, err)
		}
	})

	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse admin dsn: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// testMinedHeight is where every transaction in a fixture chain is mined,
// far enough below testTipHeight to be buried by any sane MinConfirmations.
const testMinedHeight = 100

// txSpec describes one transaction to insert.
type txSpec struct {
	// spends is the outpoint this transaction consumes, or nil for a root.
	spends *outpoint

	// outs are the outputs it creates, in vout order.
	outs []outSpec

	// mined controls both status columns. An unmined transaction is inserted
	// as 'unproven'/'unconfirmed' with no block height, which is exactly the
	// shape the live database showed for in-flight rows.
	mined bool

	// height is the block it was mined in. Below testTipHeight-MinConfirmations
	// it counts as buried.
	height int

	// knownTxStatus overrides the known_txs status while leaving the block
	// height in place. That combination is not artificial: it is the shape a
	// transaction takes when it was mined and then reorged or found to be a
	// double spend, and it is the only case where the status check does work
	// the burial check does not already do.
	knownTxStatus string
}

type outSpec struct {
	basket string
	// generation is written into custom_instructions the way Genesis and
	// AdvanceCell write it, which is where the retention floor reads it from.
	cell, generation int
	// hasGeneration distinguishes a cell output from a change output, which
	// carries no custom instructions at all.
	hasGeneration bool
}

type outpoint struct {
	txRow int64
	vout  int
}

// insertTx writes one real transaction into the wallet tables and returns its
// transaction_id.
//
// The raw transaction is genuinely serialized and its txid genuinely derived
// from those bytes, because the point of the final test is to hand the result
// back to the toolbox's own BEEF assembly, which parses raw_tx and matches it
// against the txid it was looked up by. A stub blob would pass every assertion
// about the pruner and prove nothing about the wallet.
func (w *walletFixture) insertTx(t *testing.T, spec txSpec) int64 {
	t.Helper()

	tx := transaction.NewTransaction()
	// A distinct input per transaction keeps every txid distinct even when two
	// transactions have identical outputs.
	prevTxID := fmt.Sprintf("%064x", w.nextTxRow)
	if err := tx.AddInputFrom(prevTxID, 0,
		"76a914"+strings.Repeat("11", 20)+"88ac", 100000, nil); err != nil {
		t.Fatalf("AddInputFrom: %v", err)
	}
	for range spec.outs {
		lock, err := script.NewFromHex("76a914" + strings.Repeat("22", 20) + "88ac")
		if err != nil {
			t.Fatalf("locking script: %v", err)
		}
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: 1000, LockingScript: lock})
	}
	raw := tx.Bytes()
	// The stored txid is a plain hex-decode of the DISPLAY hex, which is the
	// reverse of chainhash's internal byte order — see metastore/codec.go's
	// encTxID. Getting this wrong would still satisfy every assertion here,
	// because the pruner treats the column as an opaque key, but it would make
	// the row unfindable by the toolbox that has to read it back.
	rawID, err := hex.DecodeString(tx.TxID().String())
	if err != nil {
		t.Fatalf("decode txid: %v", err)
	}

	txRow := w.nextTxRow
	w.nextTxRow++
	now := w.encTime(time.Now())

	txStatus, ktStatus := "unproven", "unconfirmed"
	var height any
	var merkle any
	if spec.mined {
		txStatus, ktStatus = "completed", "completed"
		height = spec.height
		// A mined row carries a real, parseable proof. It has to be real: the
		// toolbox parses merkle_path when it assembles a BEEF, so a stub blob
		// would fail the assembly for a reason that has nothing to do with
		// pruning. A single-leaf tree is the smallest valid BUMP.
		yes := true
		mp := transaction.MerklePath{
			BlockHeight: uint32(spec.height),
			Path: [][]*transaction.PathElement{{
				{Offset: 0, Hash: tx.TxID(), Txid: &yes},
			}},
		}
		merkle = mp.Bytes()
	}
	if spec.knownTxStatus != "" {
		ktStatus = spec.knownTxStatus
	}

	w.exec(t, `INSERT INTO transactions
		   (transaction_id, user_id, status, reference, txid, is_outgoing, satoshis,
		    description, version, lock_time, input_beef, created_at, updated_at)
		 `+w.overriding()+`VALUES (?, ?, ?, ?, ?, ?, 1000, 'test transaction', 1, 0, NULL, ?, ?)`,
		txRow, testUserID, txStatus, fmt.Sprintf("ref-%d", txRow), rawID,
		w.encBool(true), now, now)

	w.exec(t, `INSERT INTO known_txs
		   (txid, status, attempts, rebroadcast_attempts, was_broadcast, notified,
		    notify, raw_tx, input_beef, block_height, merkle_path, created_at, updated_at)
		 VALUES (?, ?, 1, 0, ?, ?, '{}', ?, NULL, ?, ?, ?, ?)`,
		rawID, ktStatus, w.encBool(true), w.encBool(true), raw, height, merkle, now, now)

	for vout, o := range spec.outs {
		var instr any
		if o.hasGeneration {
			b, err := json.Marshal(map[string]any{"cell": o.cell, "generation": o.generation})
			if err != nil {
				t.Fatalf("encode custom instructions: %v", err)
			}
			instr = string(b)
		}
		w.exec(t, `INSERT INTO outputs
			   (user_id, transaction_id, vout, satoshis, locking_script, basket, spent_by,
			    change, output_type, provided_by, purpose, description, custom_instructions,
			    created_at, updated_at)
			 VALUES (?, ?, ?, 1000, NULL, ?, NULL, ?, 'custom', 'you', '', 'out', ?, ?, ?)`,
			testUserID, txRow, vout, o.basket, w.encBool(false), instr, now, now)
	}

	if spec.spends != nil {
		w.exec(t, `UPDATE outputs SET spent_by = ? WHERE transaction_id = ? AND vout = ?`,
			txRow, spec.spends.txRow, spec.spends.vout)
	}
	return txRow
}

// rawTxLen reports the stored raw_tx length for a transaction row, or 0 if it
// has been pruned.
func (w *walletFixture) rawTxLen(t *testing.T, txRow int64) int {
	t.Helper()
	var n sql.NullInt64
	err := w.db.QueryRowContext(t.Context(), w.q(
		`SELECT LENGTH(k.raw_tx) FROM known_txs k
		   JOIN transactions t ON t.txid = k.txid WHERE t.transaction_id = ?`), txRow).Scan(&n)
	if err != nil {
		t.Fatalf("read raw_tx for %d: %v", txRow, err)
	}
	if !n.Valid {
		return 0
	}
	return int(n.Int64)
}

func (w *walletFixture) merklePathLen(t *testing.T, txRow int64) int {
	t.Helper()
	var n sql.NullInt64
	err := w.db.QueryRowContext(t.Context(), w.q(
		`SELECT LENGTH(k.merkle_path) FROM known_txs k
		   JOIN transactions t ON t.txid = k.txid WHERE t.transaction_id = ?`), txRow).Scan(&n)
	if err != nil {
		t.Fatalf("read merkle_path for %d: %v", txRow, err)
	}
	if !n.Valid {
		return 0
	}
	return int(n.Int64)
}

func (w *walletFixture) pruner(t *testing.T, opts PruneOptions) *Pruner {
	t.Helper()
	p, err := OpenPruner(t.Context(), w.cfg, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("OpenPruner: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// testOptions is DefaultPruneOptions with the sweep made instantaneous, since a
// unit test has no interest in the throttle.
func testOptions(retain uint64) PruneOptions {
	o := DefaultPruneOptions()
	o.RetainGenerations = retain
	o.Pause = 0
	o.DryRun = false
	return o
}

// buildChain lays down one cell's chain of `gens` transitions plus a funding
// transaction, and returns the transaction_id of each generation's transaction.
//
// The shape is the real one: every transition spends the PREVIOUS generation's
// continuation output at vout 0 and creates two outputs — the continuation at
// vout 0 and change at vout 1 — because the covenant reconstructs exactly those
// two and can satisfy no other shape (see singleChangeOutput).
func buildChain(t *testing.T, w *walletFixture, cells, gens int, minedThrough int) [][]int64 {
	t.Helper()
	rows := make([][]int64, cells)
	prev := make([]outpoint, cells)

	for gen := range gens {
		for cell := range cells {
			var spends *outpoint
			if gen > 0 {
				p := prev[cell]
				spends = &p
			}
			id := w.insertTx(t, txSpec{
				spends: spends,
				mined:  gen <= minedThrough,
				height: testMinedHeight,
				outs: []outSpec{
					{basket: testCellBasket, cell: cell, generation: gen, hasGeneration: true},
					{basket: "default"},
				},
			})
			rows[cell] = append(rows[cell], id)
			prev[cell] = outpoint{txRow: id, vout: 0}
		}
	}
	return rows
}

// spendChange marks every generation's change output as spent by a mined
// transaction, which is what the real fuel keeper eventually does to it.
func (w *walletFixture) spendChange(t *testing.T, txRows []int64, height int) {
	t.Helper()
	for _, r := range txRows {
		sink := w.insertTx(t, txSpec{
			spends: &outpoint{txRow: r, vout: 1},
			mined:  true,
			height: height,
			outs:   []outSpec{{basket: "default"}},
		})
		_ = sink
	}
}

// TestPruneRequiresEveryOutputSpentAndMined is the safety property, and it fails
// against the obvious wrong implementation.
//
// "Prune generation N once N+1 is mined" is true of the CELL chain and false of
// the transaction: a transition has two outputs and only the continuation is
// spent by the next generation. The change coin goes back to the fuel pool and
// is spent later, or never. If the pruner looked only at the continuation it
// would drop the raw_tx of a transaction whose change is still fundable — and
// buildBEEF does not error on a missing row, it merges a txid-only stub
// (beef.go:59-63), so the corruption would surface much later as an
// unbroadcastable transaction.
func TestPruneRequiresEveryOutputSpentAndMined(t *testing.T) {
	w := newWalletFixture(t, 1)
	rows := buildChain(t, w, 1, 12, 11)
	cell := rows[0]

	p := w.pruner(t, testOptions(2))
	if _, err := p.Sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	// Every generation's successor is mined, but no change output has been
	// spent, so nothing at all may be pruned.
	for gen, row := range cell {
		if got := w.rawTxLen(t, row); got == 0 {
			t.Fatalf("generation %d was pruned while its change output at vout 1 is still "+
				"unspent; the funder can still allocate that coin and would need this raw_tx", gen)
		}
	}

	// Now settle the change of the early generations and sweep again. Those
	// become prunable; the rest must not.
	settled := cell[:8]
	w.spendChange(t, settled, testMinedHeight)

	p2 := w.pruner(t, testOptions(2))
	if _, err := p2.Sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for gen, row := range settled {
		if got := w.rawTxLen(t, row); got != 0 {
			t.Errorf("generation %d has every output spent by a mined, buried transaction "+
				"and is below the retention floor, but still holds %d bytes of raw_tx", gen, got)
		}
	}
	for gen := 8; gen < len(cell); gen++ {
		if got := w.rawTxLen(t, cell[gen]); got == 0 {
			t.Errorf("generation %d still has an unspent change output but was pruned", gen)
		}
	}
}

// TestPruneKeepsAnUnminedSuccessor covers the other half of the predicate: a
// spender that has been broadcast but not mined is not good enough, because it
// can still be rejected and its inputs released.
func TestPruneKeepsAnUnminedSuccessor(t *testing.T) {
	w := newWalletFixture(t, 1)

	// A mined parent whose only output is spent by an UNMINED transaction.
	parent := w.insertTx(t, txSpec{
		mined: true, height: testMinedHeight,
		outs: []outSpec{{basket: testCellBasket, cell: 0, generation: 0, hasGeneration: true}},
	})
	w.insertTx(t, txSpec{
		spends: &outpoint{txRow: parent, vout: 0},
		mined:  false,
		outs:   []outSpec{{basket: testCellBasket, cell: 0, generation: 1, hasGeneration: true}},
	})
	// Enough retained frontier for a floor to exist at all.
	buildChain(t, w, 1, 6, 5)

	p := w.pruner(t, testOptions(1))
	if _, err := p.Sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := w.rawTxLen(t, parent); got == 0 {
		t.Error("a transaction whose only spender is unmined was pruned; that spender can " +
			"still be rejected, which would put the coin back in play with no local raw_tx to rebuild from")
	}

	// The other shape: a spender that DID reach a block and then stopped being
	// mined. It still carries a buried block_height, so the confirmation depth
	// check passes it; only the status check catches it.
	parent2 := w.insertTx(t, txSpec{
		mined: true, height: testMinedHeight,
		outs: []outSpec{{basket: testCellBasket, cell: 0, generation: 0, hasGeneration: true}},
	})
	w.insertTx(t, txSpec{
		spends:        &outpoint{txRow: parent2, vout: 0},
		mined:         true,
		height:        testMinedHeight,
		knownTxStatus: "doubleSpend",
		outs:          []outSpec{{basket: testCellBasket, cell: 0, generation: 1, hasGeneration: true}},
	})

	p2 := w.pruner(t, testOptions(1))
	if _, err := p2.Sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := w.rawTxLen(t, parent2); got == 0 {
		t.Error("a transaction whose spender is buried but no longer 'completed' (reorged, or " +
			"found to be a double spend) was pruned; its output is back in play and there is " +
			"now no local raw_tx to rebuild a spend from")
	}
}

// TestPruneKeepsAnUnburiedSpender covers the reorg guard. The spender is mined,
// but not deep enough: un-mining it would put the coin back in play.
func TestPruneKeepsAnUnburiedSpender(t *testing.T) {
	w := newWalletFixture(t, 1)
	rows := buildChain(t, w, 1, 10, 9)
	w.spendChange(t, rows[0], testTipHeight)

	opts := testOptions(1)
	opts.MinConfirmations = 6
	p := w.pruner(t, opts)

	// Every spender sits at the wallet's own tip height, so none of them is
	// six deep. Nothing may be pruned.
	if _, err := p.Sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	for gen, row := range rows[0] {
		if got := w.rawTxLen(t, row); got == 0 {
			t.Fatalf("generation %d was pruned although its spender is at the chain tip "+
				"and only %d confirmations deep", gen, 0)
		}
	}
}

// TestRetainGenerationsFloorIsAbsolute proves the floor overrides the safety
// check rather than merely agreeing with it. Everything here is safe to prune;
// the floor must still refuse.
func TestRetainGenerationsFloorIsAbsolute(t *testing.T) {
	w := newWalletFixture(t, 2)
	rows := buildChain(t, w, 2, 20, 19)
	for _, cell := range rows {
		w.spendChange(t, cell, testMinedHeight)
	}

	// Retain 5 generations of 2 cells.
	p := w.pruner(t, testOptions(5))
	rep, err := p.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.FloorTxID == 0 {
		t.Fatal("no retention floor was established, so the test proves nothing")
	}
	if rep.FloorGeneration != 14 {
		t.Errorf("floor generation = %d, want 14 (max 19 minus 5 retained)", rep.FloorGeneration)
	}

	for _, cell := range rows {
		for gen := 15; gen < 20; gen++ {
			if got := w.rawTxLen(t, cell[gen]); got == 0 {
				t.Errorf("generation %d is inside the 5-generation retention floor but was pruned", gen)
			}
		}
	}
	// And the floor is a floor, not a freeze: older generations did go.
	pruned := 0
	for _, cell := range rows {
		for gen := range 10 {
			if w.rawTxLen(t, cell[gen]) == 0 {
				pruned++
			}
		}
	}
	if pruned == 0 {
		t.Error("nothing below the floor was pruned either, so the floor is not what stopped it")
	}
}

// TestDryRunWritesNothing is the guard on the default. Someone will run this
// against the production wallet.
func TestDryRunWritesNothing(t *testing.T) {
	w := newWalletFixture(t, 1)
	rows := buildChain(t, w, 1, 20, 19)
	w.spendChange(t, rows[0], testMinedHeight)

	before := make([]int, len(rows[0]))
	for i, r := range rows[0] {
		before[i] = w.rawTxLen(t, r)
	}

	opts := testOptions(2)
	opts.DryRun = true
	p := w.pruner(t, opts)
	rep, err := p.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if rep.Prunable == 0 {
		t.Fatal("the dry run found nothing prunable, so it cannot show that it wrote nothing")
	}
	if rep.Pruned != 0 {
		t.Errorf("dry run reports %d rows pruned; it must report zero", rep.Pruned)
	}
	if rep.Bytes == 0 {
		t.Error("dry run reported no byte total; the whole point is to size the saving before applying")
	}
	for i, r := range rows[0] {
		if got := w.rawTxLen(t, r); got != before[i] {
			t.Errorf("dry run changed generation %d's raw_tx from %d to %d bytes", i, before[i], got)
		}
	}

	// DefaultPruneOptions must itself be a dry run, since that is what every
	// caller starts from.
	if !DefaultPruneOptions().DryRun {
		t.Error("DefaultPruneOptions is not a dry run")
	}
}

// TestPruneKeepsTheProof checks the deliberate omission. merkle_path is the same
// order of size as raw_tx and dropping it would nearly double the saving, but it
// is the evidence the transaction was mined, and this project exists to keep
// that evidence.
func TestPruneKeepsTheProof(t *testing.T) {
	w := newWalletFixture(t, 1)
	rows := buildChain(t, w, 1, 20, 19)
	w.spendChange(t, rows[0], testMinedHeight)

	p := w.pruner(t, testOptions(2))
	if _, err := p.Sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	prunedAny := false
	for gen, r := range rows[0] {
		if w.rawTxLen(t, r) != 0 {
			continue
		}
		prunedAny = true
		if got := w.merklePathLen(t, r); got == 0 {
			t.Errorf("generation %d lost its merkle path along with its raw_tx", gen)
		}
	}
	if !prunedAny {
		t.Fatal("nothing was pruned, so the test proves nothing")
	}
}

// beefForBasket asks the TOOLBOX — not our own SQL — to assemble the BEEF for
// every spendable output in a basket, and returns it parsed.
//
// This is the call that matters. ListOutputs with IncludeTransactions is one of
// exactly two entry points into storage's buildBEEF (the other is CreateAction,
// over the coins the funder just allocated), and buildBEEF is the only thing on
// the spend path that reads known_txs.raw_tx. Running it against a pruned
// database is as close as this package can get to proving a pruned wallet can
// still fund a transaction.
func (w *walletFixture) beefForBasket(t *testing.T, basket string) (*transaction.Beef, []*wdk.WalletOutput) {
	t.Helper()
	uid := testUserID
	res, err := w.provider.ListOutputs(t.Context(),
		wdk.AuthID{IdentityKey: testIdentity, UserID: &uid},
		wdk.ListOutputsArgs{
			Basket:              primitives.StringUnder300(basket),
			IncludeTransactions: true,
			Limit:               1000,
		})
	if err != nil {
		t.Fatalf("ListOutputs(%s): %v", basket, err)
	}
	if len(res.Outputs) == 0 {
		t.Fatalf("ListOutputs(%s) returned no outputs; the fixture is wrong", basket)
	}
	beef, err := transaction.NewBeefFromBytes(res.BEEF)
	if err != nil {
		t.Fatalf("parse BEEF for %s: %v", basket, err)
	}
	return beef, res.Outputs
}

// TestAPrunedChainStillAdvances is the test the feature exists to pass.
//
// The automaton advances a cell by handing CreateAction the cell's own tip
// transaction as InputBEEF (CellChain.tipBEEF, read from the state file) and
// letting the funder allocate a coin to pay the fee. The tip comes from our
// state file, so pruning cannot touch it — but the FUNDING coin's source
// transaction is read out of known_txs.raw_tx by the toolbox itself, and that is
// exactly what this prunes. If the pruner is wrong, the funder silently gets a
// BEEF with the prevout missing (beef.go:59-63 merges a txid-only stub and
// returns no error) and the transaction fails at broadcast.
//
// So: build a chain, prune it hard, and then ask the toolbox to assemble the
// BEEF for every coin that is still spendable. Every one must come back as a
// real transaction rather than a txid-only stub.
//
// What this does NOT prove: that a real CreateAction/SignAction round trip
// succeeds against a pruned wallet. That needs a funded key and a live arcade,
// and it is the one thing left to check by hand — see the branch notes. This
// proves the database precondition that round trip depends on.
func TestAPrunedChainStillAdvances(t *testing.T) {
	w := newWalletFixture(t, 2)
	rows := buildChain(t, w, 2, 30, 29)
	// Settle the change of the older two thirds, leaving the recent third's
	// coins unspent and therefore still fundable.
	for _, cell := range rows {
		w.spendChange(t, cell[:20], testMinedHeight)
	}

	p := w.pruner(t, testOptions(3))
	rep, err := p.Sweep(t.Context())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if rep.Pruned == 0 {
		t.Fatal("nothing was pruned, so this proves nothing about a pruned chain")
	}

	// Only the UNSPENT outputs. ListOutputs returns spent ones too, and their
	// source transactions are exactly what the pruner is entitled to have
	// dropped — asserting over the whole list would fail on the feature
	// working correctly.
	unspent := w.unspentOutpoints(t)
	if len(unspent) == 0 {
		t.Fatal("no unspent outputs in the fixture, so there is nothing left to fund from")
	}

	checked := 0
	for _, basket := range []string{testCellBasket, "default"} {
		beef, outs := w.beefForBasket(t, basket)
		for _, o := range outs {
			if _, ok := unspent[string(o.Outpoint)]; !ok {
				continue
			}
			checked++
			txid, _, err := parseOutpoint(string(o.Outpoint))
			if err != nil {
				t.Fatalf("parse outpoint %q: %v", o.Outpoint, err)
			}
			hash, err := chainhash.NewHashFromHex(txid)
			if err != nil {
				t.Fatalf("parse txid %q: %v", txid, err)
			}
			entry, present := beef.Transactions[*hash]
			if !present {
				t.Errorf("basket %s: spendable outpoint %s has no BEEF entry at all",
					basket, o.Outpoint)
				continue
			}
			// DataFormat is the assertion, NOT entry.Transaction != nil. The SDK
			// fills Transaction in even for a txid-only entry, so a nil check
			// passes on exactly the shape we are trying to catch — which it
			// duly did, silently, until this was checked against a deliberately
			// over-pruned database.
			if entry.DataFormat == transaction.TxIDOnly {
				t.Errorf("basket %s: the source transaction of spendable outpoint %s came back "+
					"as a txid-only stub. That is what buildBEEF leaves behind when raw_tx has "+
					"been deleted from under it (beef.go:59-63), and it does not error — the "+
					"funder would go on to ship a transaction whose prevout is missing",
					basket, o.Outpoint)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no unspent outpoint was reached through ListOutputs, so nothing was proved")
	}
	t.Logf("pruned %d transactions; %d still-fundable coins all resolved to real transactions",
		rep.Pruned, checked)
}

// unspentOutpoints is the set of outputs the funder could still be asked to
// spend, keyed the way ListOutputs formats an outpoint.
func (w *walletFixture) unspentOutpoints(t *testing.T) map[string]struct{} {
	t.Helper()
	rows, err := w.db.QueryContext(t.Context(),
		`SELECT t.txid, o.vout FROM outputs o
		   JOIN transactions t ON t.transaction_id = o.transaction_id
		  WHERE o.spent_by IS NULL`)
	if err != nil {
		t.Fatalf("read unspent outputs: %v", err)
	}
	defer rows.Close()

	out := map[string]struct{}{}
	for rows.Next() {
		var txid []byte
		var vout int
		if err := rows.Scan(&txid, &vout); err != nil {
			t.Fatalf("scan unspent output: %v", err)
		}
		out[fmt.Sprintf("%s.%d", hex.EncodeToString(txid), vout)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read unspent outputs: %v", err)
	}
	return out
}

// parseOutpoint splits a "txid.vout" outpoint string.
func parseOutpoint(s string) (string, string, error) {
	i := strings.LastIndex(s, ".")
	if i < 0 {
		return "", "", fmt.Errorf("no vout separator in %q", s)
	}
	return s[:i], s[i+1:], nil
}

// TestPruneRefusesWhatItCannotSee covers the two conditions that guard against
// acting on an incomplete picture rather than on a proven-safe one.
//
// The first is a transaction that is not itself mined. Its raw_tx is what the
// delayed-broadcast drainer resends from — FindResendable selects on
// "was_broadcast = true AND raw_tx IS NOT NULL" — so dropping it strands a
// transaction that has not yet made it onto a chain, which is far worse than
// keeping a kilobyte.
//
// The second is a transaction with no outputs rows at all. "Every one of its
// outputs is spent" is vacuously true of an empty set, and a transaction whose
// outputs we cannot enumerate is precisely the one we know least about.
func TestPruneRefusesWhatItCannotSee(t *testing.T) {
	w := newWalletFixture(t, 1)

	// An unmined transaction whose only output is nonetheless spent by a mined,
	// buried transaction. Contrived, but it is exactly what the candidate
	// query's status guard exists to refuse.
	inFlight := w.insertTx(t, txSpec{
		mined: false,
		outs:  []outSpec{{basket: testCellBasket, cell: 0, generation: 0, hasGeneration: true}},
	})
	w.insertTx(t, txSpec{
		spends: &outpoint{txRow: inFlight, vout: 0},
		mined:  true, height: testMinedHeight,
		outs: []outSpec{{basket: testCellBasket, cell: 0, generation: 1, hasGeneration: true}},
	})

	// A mined transaction the wallet recorded no outputs for.
	noOutputs := w.insertTx(t, txSpec{mined: true, height: testMinedHeight})

	buildChain(t, w, 1, 6, 5)

	p := w.pruner(t, testOptions(1))
	if _, err := p.Sweep(t.Context()); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if got := w.rawTxLen(t, inFlight); got == 0 {
		t.Error("an unmined transaction was pruned; its raw_tx is what the delayed-broadcast " +
			"drainer resends from, so it has been stranded")
	}
	if got := w.rawTxLen(t, noOutputs); got == 0 {
		t.Error("a transaction with no recorded outputs was pruned on the vacuous truth that " +
			"all zero of its outputs are spent")
	}
}

// TestPruneOnPostgres re-runs the safety property against PostgreSQL, because
// the SQLite tests do not exercise the dialect at all.
//
// Three things differ and only one of them would show up as a compile error:
// every placeholder has to be rewritten from ? to $N; the UPDATE takes its row
// locks through an ORDER BY'd FOR UPDATE sub-select that has no SQLite
// equivalent; and txid is BYTEA rather than BLOB. A dialect bug in any of those
// would sail through the SQLite suite and fail on the only database that
// matters.
//
// Opt-in via RULE110_TEST_POSTGRES_DSN, pointed at a server — NOT a database —
// on which this may freely CREATE and DROP. It builds its own
// rule110_test_<random> and drops it afterwards; it never touches an existing
// database.
func TestPruneOnPostgres(t *testing.T) {
	adminDSN := os.Getenv(postgresDSNEnv)
	if adminDSN == "" {
		t.Skipf("set %s to a server this may create and drop databases on", postgresDSNEnv)
	}

	w := newWalletFixtureOn(t, 2, adminDSN)
	rows := buildChain(t, w, 2, 20, 19)
	for _, cell := range rows {
		w.spendChange(t, cell[:12], testMinedHeight)
	}

	// Dry run first, exactly as an operator would.
	dry := testOptions(3)
	dry.DryRun = true
	rep, err := w.pruner(t, dry).Sweep(t.Context())
	if err != nil {
		t.Fatalf("dry sweep: %v", err)
	}
	if rep.Prunable == 0 {
		t.Fatal("the dry run found nothing prunable on PostgreSQL")
	}
	if rep.Pruned != 0 {
		t.Errorf("dry run pruned %d rows on PostgreSQL", rep.Pruned)
	}
	dryPrunable, dryBytes := rep.Prunable, rep.Bytes

	applied, err := w.pruner(t, testOptions(3)).Sweep(t.Context())
	if err != nil {
		t.Fatalf("applied sweep: %v", err)
	}
	if applied.Pruned != dryPrunable {
		t.Errorf("the dry run promised %d rows and the real run pruned %d; the dry run is "+
			"the number an operator decides on, so the two must agree", dryPrunable, applied.Pruned)
	}
	if applied.Bytes != dryBytes {
		t.Errorf("dry run measured %d bytes, real run %d", dryBytes, applied.Bytes)
	}

	// The safety property, restated on this engine.
	for _, cell := range rows {
		for gen := 12; gen < 20; gen++ {
			if got := w.rawTxLen(t, cell[gen]); got == 0 {
				t.Errorf("generation %d still has an unspent change output but was pruned", gen)
			}
		}
	}

	// And a second pass must be a no-op rather than re-reporting the same rows.
	again, err := w.pruner(t, testOptions(3)).Sweep(t.Context())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again.Prunable != 0 {
		t.Errorf("a second pass found %d more prunable rows; the sweep is not converging",
			again.Prunable)
	}
}

// TestTheDefaultRingCanEstablishARetentionFloor is the regression guard for
// pruning switching itself off when the ring got wider.
//
// The lookback is cells × (retain+1), a product of two numbers set in different
// places: the ring size is fixed at genesis and -retain-generations is a pruner
// flag. At the default retention of 1000 the old 250,000 cap admitted 250 cells
// and refused 256, so widening the ring disabled pruning entirely — and it
// failed loudly in the wrong direction, because Run logs the error and carries
// on, which reads as a pruner that is running.
func TestTheDefaultRingCanEstablishARetentionFloor(t *testing.T) {
	cells := DefaultConfig().Cells
	retain := DefaultPruneOptions().RetainGenerations

	window, err := floorWindow(cells, retain)
	if err != nil {
		t.Fatalf("the shipped defaults cannot establish a retention floor: %v", err)
	}
	if want := cells * int(retain+1); window != want {
		t.Errorf("lookback = %d, want %d", window, want)
	}
}

// The cap still has to fail closed. Scanning an unbounded slice of a table
// measured in gigabytes is the thing it exists to refuse.
func TestAnOversizedRetentionWindowIsRefused(t *testing.T) {
	if _, err := floorWindow(DefaultConfig().Cells, 1_000_000); err == nil {
		t.Error("floorWindow accepted a lookback that would scan the whole outputs table")
	}
	if _, err := floorWindow(0, 1000); err == nil {
		t.Error("floorWindow accepted a ring of no cells")
	}
}

// The sweep has to clear rows faster than the automaton writes them, or it
// never gains ground and is indistinguishable from a pruner that is not
// running. The ring at the configured rate is the bar it has to clear.
func TestTheSweepOutrunsTheAutomaton(t *testing.T) {
	opts := DefaultPruneOptions()
	perSecond := float64(opts.BatchSize) / opts.Pause.Seconds()

	// One transaction per cell per generation, at the rate the public
	// deployment is locked to, before the fuel keeper's own leaves.
	const lockedRate = 0.5
	produced := float64(DefaultConfig().Cells) * lockedRate

	if perSecond <= produced {
		t.Errorf("sweep clears %.0f rows/s against %.0f transactions/s of production; "+
			"a pruner slower than the workload never catches up", perSecond, produced)
	}
}
