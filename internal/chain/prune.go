package chain

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	_ "modernc.org/sqlite"             // sqlite driver
)

// The wallet database grows forever, and nothing in the toolbox bounds it.
//
// A cell transition costs one row in known_txs and one in transactions, plus
// two outputs, two transaction_labels and two output_tags. The toolbox clears
// both input_beef columns when a transaction is mined (SetProof for known_txs,
// ClearInputBEEFByTxID for transactions) but deliberately keeps known_txs.raw_tx
// — the comment at metastore/knowntx.go:510 says why: "raw_tx is kept (needed to
// reconstruct the tx when spending its outputs)". So raw_tx is the one blob that
// survives to steady state, and it survives forever.
//
// Measured on the live 13 GB deployment (2026-08-12, 132,366 known_txs rows):
//
//	raw_tx          4,082 bytes uncompressed, 1,115 bytes stored after TOAST
//	merkle_path     1,086 bytes, incompressible
//	input_beef      NULL on every one of the 123,750 completed rows but eight
//
// So the recoverable payload is ~1.1 kB per transaction, not the ~4 kB the raw
// transaction suggests: PostgreSQL's pglz gets 3.7x on a covenant script that is
// mostly repeated opcodes. At the measured rate — 20,000 to 47,000 transactions
// an hour, ~31,000 on average — that is ~830 MB of raw_tx a day and ~300 GB a
// year, and it never comes back.
//
// This file deletes exactly that, and nothing else. See prunable for the
// argument that it is safe to.
//
// What it does NOT do is shrink the database that already exists, and the
// difference is stark enough to be worth stating here. Of the 13 GB, the live
// payload of both big tables together is about 1.1 GB: 687 MB across known_txs
// (132 MB raw_tx, 128 MB merkle_path, 418 MB of input_beef still held by
// unconfirmed, invalidTx and doubleSpend rows) and 428 MB across transactions
// (almost all of it input_beef on failed rows). The remaining ~11.6 GB is dead
// tuples in the two TOAST relations, left behind because every MINED update
// rewrites the row to clear input_beef. Autovacuum returns that space to the
// free space map and reuses it, but never to the operating system. Pruning caps
// the growth; only VACUUM FULL or pg_repack reclaims the existing 11.6 GB, and
// neither is this program's business.

// PruneOptions configures a sweep. The zero value is not usable; see
// DefaultPruneOptions.
type PruneOptions struct {
	// DryRun reports what would be pruned without writing. It defaults to TRUE
	// everywhere it is constructed, and every CLI entry point requires an
	// explicit flag to turn it off. This feature deletes data from a wallet
	// that holds real money and has no backup, so the default has to be the
	// harmless one.
	DryRun bool

	// RetainGenerations is the floor: transactions belonging to the newest N
	// generations are never touched, whatever the safety check says.
	//
	// This is a second, independent brake. The safety check below is what makes
	// pruning correct; this is what makes a BUG in the safety check survivable,
	// because the automaton's live frontier stays intact even if the check is
	// wrong. Set it well above any depth the automaton can be running at.
	RetainGenerations uint64

	// MinConfirmations is how deep the SPENDING transaction must be buried
	// before its parent's raw_tx is dropped.
	//
	// A reorg that un-mines the spender puts its inputs back in play, and the
	// wallet would then need the parent's raw_tx again to rebuild the spend.
	// Six blocks is the conventional depth and costs nothing: at 35,000
	// transactions an hour the prunable backlog is enormous either way.
	MinConfirmations uint64

	// BatchSize bounds one statement. The engine writes to this database at up
	// to several hundred statements a second, so a sweep must never hold row
	// locks long enough to be noticed.
	BatchSize int

	// Pause is the gap between batches. This is the throttle that makes the
	// sweep a background job rather than a competitor: at the defaults it
	// prunes ~250 rows a second, which is below the automaton's own write rate
	// but still clears a day's accumulation in well under a day.
	Pause time.Duration

	// Interval is how long Run waits after a pass finds nothing left to do.
	Interval time.Duration

	// MaxBatches bounds one pass, or 0 for unbounded. A bounded pass is what
	// makes a dry run against a 13 GB database finish in a knowable time.
	MaxBatches int
}

// DefaultPruneOptions returns conservative settings: dry run, a deep retention
// floor, and a slow enough sweep to be invisible to the automaton.
func DefaultPruneOptions() PruneOptions {
	return PruneOptions{
		DryRun:            true,
		RetainGenerations: 1000,
		MinConfirmations:  6,
		BatchSize:         500,
		Pause:             2 * time.Second,
		Interval:          5 * time.Minute,
	}
}

// PruneReport is what one pass did, or would have done.
type PruneReport struct {
	// Scanned is how many mined transactions were examined.
	Scanned int64

	// Prunable is how many passed every check. In a dry run nothing was
	// written and this is the whole result; otherwise Pruned says how many
	// rows the UPDATE actually matched, which can be lower if a concurrent
	// writer moved a row out from under us.
	Prunable int64
	Pruned   int64

	// Bytes is the raw_tx payload behind Prunable, measured with LENGTH()
	// before anything was written. This is the on-disk stored size on
	// PostgreSQL (TOAST-compressed) and the uncompressed size on SQLite.
	Bytes int64

	// FloorTxID is the transaction_id at and above which RetainGenerations
	// forbade any pruning, and FloorGeneration the generation it came from.
	// Zero means the floor could not be established and nothing was eligible.
	FloorTxID       int64
	FloorGeneration uint64

	// TipHeight is the highest block the wallet has seen, which is what
	// MinConfirmations is measured against.
	TipHeight uint64
}

// Pruner reclaims raw_tx from wallet transactions that can no longer be spent.
type Pruner struct {
	db       *sql.DB
	postgres bool
	cells    int
	opts     PruneOptions
	logger   *slog.Logger

	// cursor is where the next batch resumes. Sweeping oldest-first with a
	// cursor is what keeps a pass O(batch) instead of O(table): without it
	// every pass would re-examine the rows it already pruned.
	cursor int64
}

// OpenPruner connects to the wallet database directly.
//
// Directly, because there is no other way: the toolbox exposes no retention API
// at all — no Prune, Purge, Compact or delete-by-age anywhere in
// /git/go-arcade-toolbox/pkg, and its only bounded-growth mechanisms are the two
// input_beef clears and the utxostore's RemoveSpentBy. The backend choice
// mirrors chain.Open exactly, so the pruner always lands on the same database
// the wallet is using.
func OpenPruner(ctx context.Context, cfg Config, opts PruneOptions, logger *slog.Logger) (*Pruner, error) {
	if opts.BatchSize <= 0 {
		return nil, fmt.Errorf("chain: prune: batch size must be positive")
	}
	if cfg.Cells <= 0 {
		return nil, fmt.Errorf("chain: prune: cells must be positive")
	}

	driver, target, postgres := "sqlite", sqliteWalletDSN(cfg.DataDir), false
	if cfg.PostgresDSN != "" {
		driver, target, postgres = "pgx", cfg.PostgresDSN, true
	}
	db, err := sql.Open(driver, target)
	if err != nil {
		return nil, fmt.Errorf("chain: prune: open %s: %w", driver, err)
	}
	// One connection. The sweep is deliberately serial and slow, and a pool
	// would only add contention to a database the automaton is already
	// saturating.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("chain: prune: connect: %w", err)
	}
	return &Pruner{db: db, postgres: postgres, cells: cfg.Cells, opts: opts, logger: logger}, nil
}

// sqliteWalletDSN opens the wallet file the way the toolbox opens it.
//
// The pragmas are not optional and not cosmetic. sqlkit.SQLiteDSN builds exactly
// this set and says why: "two stores sharing one SQLite file MUST open it with
// the same locking posture (WAL journaling, a busy timeout, and
// _txlock=immediate so a write transaction takes its RESERVED lock up front
// rather than deadlocking on upgrade)". The pruner is a third writer on that
// same file, so it has to match. It cannot import sqlkit — that package is
// internal to the toolbox — so the set is reproduced here and must be kept in
// step with it.
//
// PostgreSQL needs no equivalent: the DSN the wallet is given is the DSN the
// pruner is given, and connection-level locking posture is not a thing there.
func sqliteWalletDSN(dataDir string) string {
	q := url.Values{}
	q.Set("_journal_mode", "WAL")
	q.Set("_busy_timeout", "5000")
	q.Set("_foreign_keys", "on")
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "synchronous(NORMAL)")
	return filepath.Join(dataDir, "wallet.db") + "?" + q.Encode()
}

// Close releases the connection.
func (p *Pruner) Close() error { return p.db.Close() }

// rebind converts ? placeholders to $N for PostgreSQL, as history.Store does.
func (p *Pruner) rebind(q string) string {
	if !p.postgres {
		return q
	}
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	for i := range len(q) {
		if q[i] == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

// Run sweeps continuously until ctx is cancelled.
//
// Shaped like the engine's own background loops (reconcile, checkpoint): ticker,
// return on ctx.Done, log and carry on after an error. A pruner that dies on the
// first transient database error is a pruner that silently stops working weeks
// into a months-long run.
func (p *Pruner) Run(ctx context.Context) {
	tick := time.NewTicker(p.opts.Interval)
	defer tick.Stop()
	for {
		rep, err := p.Sweep(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			p.logger.WarnContext(ctx, "prune sweep", "err", err)
		} else if rep.Prunable > 0 {
			p.logger.InfoContext(ctx, "prune sweep",
				"dryRun", p.opts.DryRun, "scanned", rep.Scanned,
				"prunable", rep.Prunable, "pruned", rep.Pruned, "bytes", rep.Bytes)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Sweep makes one pass in bounded batches, pausing between them.
//
// It deliberately does NOT wrap the pass in a transaction. Each batch is one
// short UPDATE over at most BatchSize rows, so the longest lock this holds is
// milliseconds; a pass-wide transaction would instead hold a snapshot open for
// however long the pass takes, which on a 13 GB database is long enough to stop
// autovacuum reclaiming anything and to bloat the very table we are trying to
// shrink.
//
// A dry run costs more than a real one, which is the opposite of the intuition:
// it never clears raw_tx, so nothing drops out of the candidate query and the
// pass walks every mined transaction in the table. On the live deployment that
// is ~143,000 rows, which at the default batch and pause is roughly ten minutes.
// Use MaxBatches to bound it.
func (p *Pruner) Sweep(ctx context.Context) (PruneReport, error) {
	var rep PruneReport

	tip, err := p.tipHeight(ctx)
	if err != nil {
		return rep, err
	}
	rep.TipHeight = tip
	if tip < p.opts.MinConfirmations {
		// Nothing can be buried deep enough yet. Not an error: a fresh
		// deployment is simply not prunable.
		return rep, nil
	}

	floorTx, floorGen, err := p.floor(ctx)
	if err != nil {
		return rep, err
	}
	rep.FloorTxID, rep.FloorGeneration = floorTx, floorGen
	if floorTx <= 0 {
		// The automaton has not yet run past RetainGenerations, so the floor
		// covers everything. Refusing here is the point of the floor.
		return rep, nil
	}

	for batch := 0; p.opts.MaxBatches == 0 || batch < p.opts.MaxBatches; batch++ {
		n, err := p.sweepBatch(ctx, floorTx, tip, &rep)
		if err != nil {
			return rep, err
		}
		if n == 0 {
			// Exhausted. Rewind so the next pass re-examines from the start:
			// transactions become prunable as their successors are mined, so a
			// row skipped this pass may well be eligible next time.
			p.cursor = 0
			return rep, nil
		}
		select {
		case <-ctx.Done():
			return rep, ctx.Err()
		case <-time.After(p.opts.Pause):
		}
	}
	return rep, nil
}

// candidate is one mined transaction being considered.
type candidate struct {
	txID  int64
	rawID []byte
}

// sweepBatch examines one batch and returns how many candidates it scanned.
func (p *Pruner) sweepBatch(ctx context.Context, floorTx int64, tip uint64, rep *PruneReport) (int, error) {
	cands, err := p.candidates(ctx, floorTx)
	if err != nil {
		return 0, err
	}
	if len(cands) == 0 {
		return 0, nil
	}
	p.cursor = cands[len(cands)-1].txID
	rep.Scanned += int64(len(cands))

	safe, err := p.prunable(ctx, cands, tip)
	if err != nil {
		return len(cands), err
	}
	if len(safe) == 0 {
		return len(cands), nil
	}
	rep.Prunable += int64(len(safe))

	// Measure before writing, so a dry run and a real run report the same
	// number and the dry run's estimate is the truth rather than a guess.
	freed, err := p.payloadBytes(ctx, safe)
	if err != nil {
		return len(cands), err
	}
	rep.Bytes += freed

	if p.opts.DryRun {
		return len(cands), nil
	}
	pruned, err := p.clear(ctx, safe)
	if err != nil {
		return len(cands), err
	}
	rep.Pruned += pruned
	return len(cands), nil
}

// candidates reads the next batch of mined transactions that still carry a
// raw_tx, oldest first and strictly below the retention floor.
func (p *Pruner) candidates(ctx context.Context, floorTx int64) ([]candidate, error) {
	// Both status columns are checked. They are separate enums maintained by
	// separate code paths — transactions.status is wdk.TxStatus ("completed"),
	// known_txs.status is wdk.ProvenTxReqStatus (also "completed", but with
	// "unconfirmed", "invalidTx" and "doubleSpend" as its other live values) —
	// and requiring both to say mined costs one indexed lookup.
	q := `SELECT t.transaction_id, t.txid
	        FROM transactions t
	        JOIN known_txs k ON k.txid = t.txid
	       WHERE t.transaction_id > ? AND t.transaction_id < ?
	         AND t.status = 'completed' AND k.status = 'completed'
	         AND k.raw_tx IS NOT NULL
	       ORDER BY t.transaction_id
	       LIMIT ?`
	rows, err := p.db.QueryContext(ctx, p.rebind(q), p.cursor, floorTx, p.opts.BatchSize)
	if err != nil {
		return nil, fmt.Errorf("chain: prune: read candidates: %w", err)
	}
	defer rows.Close()

	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.txID, &c.rawID); err != nil {
			return nil, fmt.Errorf("chain: prune: scan candidate: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chain: prune: read candidates: %w", err)
	}
	return out, nil
}

// prunable filters a batch down to the transactions whose raw_tx can no longer
// be needed, and IS THE ENTIRE FEATURE. The argument, in full:
//
// The only thing that reads known_txs.raw_tx on the spend path is the toolbox's
// buildBEEF (pkg/storage/beef.go:55-65). It is called from exactly two places —
// CreateAction, over the txids of the coins the funder just ALLOCATED, and
// ListOutputs with IncludeBEEF — and in both it looks up the funding
// transaction of an output the wallet still considers spendable. So raw_tx is
// needed for exactly as long as some output of that transaction can still be
// selected as an input.
//
// An output can no longer be selected once it is spent by a transaction that is
// mined: the spend is confirmed, the coin is gone, and no funder can allocate
// it again. Therefore a transaction ALL of whose outputs are spent by mined
// transactions can never be an input source again, and its raw_tx is dead.
//
// Hence the conditions below, all required. The first is enforced upstream in
// candidates and re-enforced in clear, so it survives either one being wrong:
//
//   - the transaction is ITSELF mined. An unmined transaction's raw_tx is what
//     the delayed-broadcast drainer resends from — FindResendable selects on
//     "was_broadcast = true AND raw_tx IS NOT NULL" (metastore/knowntx.go:264) —
//     so dropping it strands a transaction that has not yet reached a block;
//
//   - the transaction has at least one recorded output. A transaction with no
//     outputs row is one we do not understand, and "all zero of its outputs are
//     spent" is a vacuous truth we must not act on. In practice the GROUP BY
//     below already enforces this — a transaction with no outputs produces no
//     group and so never reaches the filter — so the explicit total > 0 test is
//     unreachable today. It is kept because the property is the one that
//     matters, not the query shape that currently happens to imply it;
//
//   - every one of those outputs has spent_by set, and the spender is
//     'completed' in known_txs, i.e. actually mined and not merely broadcast;
//
//   - the spender is buried MinConfirmations deep, so a reorg cannot put the
//     coin back in play after we have destroyed the only local copy of the
//     transaction that created it.
//
// This is strictly stronger than "the cell's next generation is mined", which
// is the property the automaton itself guarantees. A cell transition has TWO
// outputs — the covenant continuation at vout 0 and the P2PKH change at vout 1
// — and only the continuation is spent by the next generation. The change coin
// goes back to the fuel pool and is spent by some unrelated later transaction,
// possibly much later or never. Pruning on the continuation alone would destroy
// the raw_tx of a transaction whose change is still fundable, and the failure
// would be SILENT: buildBEEF does not error on a missing row, it merges a
// txid-only stub and returns nil (beef.go:59-63), so the wallet would write a
// structurally valid BEEF with the prevout missing and only fail later, at
// broadcast, with an error that points nowhere near here.
//
// What this does NOT prune, deliberately:
//
//   - merkle_path (~1,086 bytes a row, the same order as raw_tx). It is the
//     proof the transaction was mined, buildBEEF reads it to terminate an
//     ancestry walk, and this project exists to keep evidence that each bit was
//     proved on chain. Halving the saving is the right price for that.
//   - any row in transactions. Three foreign keys point at it —
//     outputs.transaction_id, outputs.spent_by and
//     transaction_labels.transaction_id — with no ON DELETE clause anywhere in
//     the schema, so a delete would simply fail; and forcing it by deleting the
//     children first would destroy the outputs-to-txid mapping (txid lives only
//     on transactions) and the derivation prefix and suffix needed to re-derive
//     spending keys. Measurement says there is nothing there to win anyway: the
//     123,750 completed rows hold 1,399 bytes of input_beef between them.
//   - the known_txs row itself. Nothing at the SQL layer would stop us — it has
//     no inbound foreign keys — but keeping the row costs ~200 bytes and keeps
//     the monitor's status bookkeeping, the txid record and the proof intact.
//     Nulling a column is also idempotent and cannot be half-done.
//
// Two readers can still observe the missing raw_tx, and neither is on the spend
// path. applyMinedBatch parses it to enumerate spent inputs and documents its
// own fallback ("A proof whose raw tx is absent or unparseable keeps spentOps
// nil and falls back to the scan"), and it only runs while a transaction is
// BECOMING mined, which ours already is. The double-spend reconciler's
// localWinnerInputs looks competitors up by txid and treats a miss as
// "unresolved", continuing (reconciler.go:406). A transaction all of whose
// outputs are spent by transactions buried six blocks deep is not a live
// double-spend candidate, but this is stated rather than proved.
func (p *Pruner) prunable(ctx context.Context, cands []candidate, tip uint64) ([]candidate, error) {
	byID := make(map[int64]candidate, len(cands))
	ids := make([]any, 0, len(cands))
	for _, c := range cands {
		byID[c.txID] = c
		ids = append(ids, c.txID)
	}

	// SUM(CASE ...) rather than COUNT(*) FILTER: both engines have FILTER in
	// recent versions, but CASE is the form that is certainly portable.
	//
	// The joins are LEFT so an unspent output still produces a row: spent_by
	// NULL leaves both joined tables NULL, the CASE yields 0, and the
	// transaction fails the settled = outputs test — which is exactly the
	// answer we want.
	q := `SELECT o.transaction_id,
	             COUNT(*),
	             SUM(CASE WHEN sk.status = 'completed'
	                       AND sk.block_height IS NOT NULL
	                       AND sk.block_height + ? <= ?
	                      THEN 1 ELSE 0 END)
	        FROM outputs o
	        LEFT JOIN transactions st ON st.transaction_id = o.spent_by
	        LEFT JOIN known_txs   sk ON sk.txid = st.txid
	       WHERE o.transaction_id IN (` + placeholders(len(ids)) + `)
	       GROUP BY o.transaction_id`
	args := append([]any{int64(p.opts.MinConfirmations), int64(tip)}, ids...)

	rows, err := p.db.QueryContext(ctx, p.rebind(q), args...)
	if err != nil {
		return nil, fmt.Errorf("chain: prune: check outputs: %w", err)
	}
	defer rows.Close()

	var safe []candidate
	for rows.Next() {
		var txID, total, settled int64
		if err := rows.Scan(&txID, &total, &settled); err != nil {
			return nil, fmt.Errorf("chain: prune: scan output check: %w", err)
		}
		if total > 0 && settled == total {
			safe = append(safe, byID[txID])
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("chain: prune: check outputs: %w", err)
	}

	// Ascending raw txid order, which is the order the toolbox's own bulk
	// mutators take their row locks in (metastore/lockorder.go). Matching it is
	// what stops this sweep deadlocking against a concurrent status apply.
	slices.SortFunc(safe, func(a, b candidate) int { return bytes.Compare(a.rawID, b.rawID) })
	return safe, nil
}

// payloadBytes measures the raw_tx about to be dropped.
//
// LENGTH() on a bytea is the uncompressed byte count, so on PostgreSQL this
// OVER-states the disk saving: the live deployment stores 4,082-byte raw
// transactions in 1,115 bytes after TOAST compression. It is reported as-is
// rather than scaled by a guessed ratio; divide by ~3.7 for that database.
func (p *Pruner) payloadBytes(ctx context.Context, safe []candidate) (int64, error) {
	ids := make([]any, len(safe))
	for i, c := range safe {
		ids[i] = c.rawID
	}
	q := `SELECT COALESCE(SUM(LENGTH(raw_tx)), 0) FROM known_txs
	       WHERE txid IN (` + placeholders(len(ids)) + `) AND raw_tx IS NOT NULL`
	var n int64
	if err := p.db.QueryRowContext(ctx, p.rebind(q), ids...).Scan(&n); err != nil {
		return 0, fmt.Errorf("chain: prune: measure payload: %w", err)
	}
	return n, nil
}

// clear drops the payload, re-checking the status it was selected on.
//
// The re-check is not decoration. Candidates are chosen and then written in
// separate statements, and a reorg between the two could move a row back out of
// 'completed'. Folding the condition into the UPDATE makes the write atomic
// with respect to that: a row that stopped being mined simply does not match.
func (p *Pruner) clear(ctx context.Context, safe []candidate) (int64, error) {
	ids := make([]any, len(safe))
	for i, c := range safe {
		ids[i] = c.rawID
	}
	in := "txid IN (" + placeholders(len(ids)) + ")"
	if p.postgres && len(ids) > 1 {
		// The toolbox's txidLockOrderedIn, reproduced: force the plan to take
		// its row locks in ascending txid order rather than whatever order it
		// would otherwise choose, so this can never invert against a concurrent
		// bulk status apply and deadlock (SQLSTATE 40P01). That deadlock is not
		// hypothetical — metastore/lockorder.go was written for it, after it was
		// seen under sustained load and absorbed four times by a retry wrapper
		// before anyone noticed.
		//
		// Untested, and not testable here: it only matters against a concurrent
		// writer, which a single-threaded test cannot produce. Removing this line
		// breaks nothing in the suite. It is here on the strength of the
		// toolbox's own argument, not on the strength of a test.
		in = "txid IN (SELECT txid FROM known_txs WHERE " + in + " ORDER BY txid FOR UPDATE)"
	}
	// updated_at is deliberately left alone. The monitor scans known_txs by
	// (status, updated_at); bumping it would move every pruned row to the head
	// of that index for no reason, and the row has not meaningfully changed.
	q := `UPDATE known_txs SET raw_tx = NULL, input_beef = NULL
	       WHERE ` + in + ` AND status = 'completed' AND raw_tx IS NOT NULL`

	res, err := p.db.ExecContext(ctx, p.rebind(q), ids...)
	if err != nil {
		return 0, fmt.Errorf("chain: prune: clear payload: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("chain: prune: clear payload: %w", err)
	}
	return n, nil
}

// tipHeight is the highest block the wallet has a proof for.
//
// The wallet's own view, not the network's, and that is the safe direction: if
// the wallet is behind, tip is low, fewer spenders clear MinConfirmations, and
// less is pruned.
func (p *Pruner) tipHeight(ctx context.Context) (uint64, error) {
	var h sql.NullInt64
	q := `SELECT MAX(block_height) FROM known_txs WHERE status = 'completed'`
	if err := p.db.QueryRowContext(ctx, q).Scan(&h); err != nil {
		return 0, fmt.Errorf("chain: prune: read tip height: %w", err)
	}
	if !h.Valid || h.Int64 < 0 {
		return 0, nil
	}
	return uint64(h.Int64), nil
}

// floorWindowCap bounds the retention-floor lookup.
//
// The floor is found by reading the newest cells*(retain+1) cell outputs, so a
// large -retain-generations turns into a large query. Past this cap the pruner
// refuses to establish a floor at all rather than scanning an unbounded slice of
// a 13 GB table — failing closed, which for this feature is the only acceptable
// direction to fail in.
const floorWindowCap = 250_000

// floor returns the transaction_id at and above which nothing may be pruned,
// and the generation that produced it.
//
// The wallet schema has no notion of a generation, but this deployment writes
// one into every cell output: CreateActionOutput.CustomInstructions carries
// {"cell":C,"generation":G} (see Genesis and AdvanceCell). That is read back
// here and parsed in Go rather than in SQL, because SQLite and PostgreSQL spell
// JSON extraction differently and this has to work on both.
//
// The floor is the LOWEST transaction_id among transactions that produced a cell
// output in the retained window. Using a transaction_id rather than a generation
// is what makes the floor protect the fuel and change transactions interleaved
// with the retained generations too — they carry no generation of their own, but
// they were created in the same span of ids.
func (p *Pruner) floor(ctx context.Context) (int64, uint64, error) {
	window := p.cells * int(p.opts.RetainGenerations+1)
	if window <= 0 || window > floorWindowCap {
		return 0, 0, fmt.Errorf(
			"chain: prune: -retain-generations %d over %d cells needs a %d-row lookback, "+
				"above the %d cap; lower it", p.opts.RetainGenerations, p.cells, window, floorWindowCap)
	}

	q := `SELECT transaction_id, custom_instructions
	        FROM outputs
	       WHERE basket = ? AND custom_instructions IS NOT NULL AND custom_instructions <> ''
	       ORDER BY transaction_id DESC
	       LIMIT ?`
	rows, err := p.db.QueryContext(ctx, p.rebind(q), CellBasket, window)
	if err != nil {
		return 0, 0, fmt.Errorf("chain: prune: read retention floor: %w", err)
	}
	defer rows.Close()

	type seen struct {
		txID int64
		gen  uint64
	}
	var all []seen
	var maxGen uint64
	for rows.Next() {
		var txID int64
		var instr string
		if err := rows.Scan(&txID, &instr); err != nil {
			return 0, 0, fmt.Errorf("chain: prune: scan retention floor: %w", err)
		}
		var ci struct {
			Cell       int    `json:"cell"`
			Generation uint64 `json:"generation"`
		}
		if err := json.Unmarshal([]byte(instr), &ci); err != nil {
			// An output whose instructions we cannot read tells us nothing
			// about the frontier. Skipping it only shrinks the window, which
			// raises the floor and prunes less.
			continue
		}
		all = append(all, seen{txID: txID, gen: ci.Generation})
		if ci.Generation > maxGen {
			maxGen = ci.Generation
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("chain: prune: read retention floor: %w", err)
	}
	if len(all) < window {
		// Fewer cell outputs exist than the retention window asks for, so the
		// whole automaton is inside the floor. Nothing is eligible.
		return 0, 0, nil
	}
	if maxGen < p.opts.RetainGenerations {
		return 0, 0, nil
	}
	floorGen := maxGen - p.opts.RetainGenerations

	var floorTx int64
	for _, s := range all {
		if s.gen <= floorGen {
			continue
		}
		if floorTx == 0 || s.txID < floorTx {
			floorTx = s.txID
		}
	}
	return floorTx, floorGen, nil
}

// placeholders renders n comma-separated ? binds.
func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
