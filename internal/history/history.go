// Package history is the durable record of the automaton: every generation's
// row, and every transaction that proved a cell of it.
//
// This is the point of the project, not a cache. The rows can be recomputed
// from the seed at any time — the automaton is deterministic — but the
// transaction ids cannot. They are the evidence that each bit was proved on
// chain, and they only exist here.
//
// The volume is modest: 128 txids per generation is roughly 10 KB, so tens of
// thousands of generations fit comfortably in the database the wallet is
// already using.
package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	_ "modernc.org/sqlite"             // sqlite driver
)

// Status is a transaction's lifecycle state as recorded here.
type Status string

const (
	// StatusAttempting is written BEFORE a transaction is built, and carries no
	// txid because none exists yet.
	//
	// It is the write-ahead record. Signing a cell transition broadcasts it, so
	// there is a window between "this output is now spent on chain" and "we know
	// that". A process killed inside that window comes back believing the cell is
	// a generation behind, re-spends an output that is already spent, and loses
	// the cell to a rejection it cannot distinguish from a real failure. That is
	// exactly how cells 34 and 51 died: ffb1c43e was broadcast at 22:20:02, the
	// process restarted at 22:20:05, and the replacement was rejected at 22:20:55.
	//
	// An attempt left unresolved at startup means the tip is UNKNOWN, not stale.
	StatusAttempting Status = "attempting"

	StatusBroadcast Status = "broadcast"
	StatusSeen      Status = "seen"
	StatusMined     Status = "mined"
	StatusFailed    Status = "failed"
)

// IsTerminal reports whether a status can still change.
//
// Terminal transactions are dropped from the live tracking set: arcade has
// nothing further to say about them, so continuing to poll or index them is
// pure overhead. Their record stays in the database forever.
func (s Status) IsTerminal() bool { return s == StatusMined || s == StatusFailed }

// CellTx is one cell's transaction in one generation.
type CellTx struct {
	Generation uint64
	Cell       int
	TxID       string
	Status     Status
	Err        string
}

// Generation is one row of the automaton with the transactions that proved it.
type Generation struct {
	Number uint64
	RowHex string
	Cells  []CellTx
}

// Store persists the automaton's history.
type Store struct {
	db       *sql.DB
	postgres bool
}

// maxOpenConns bounds the history store's connection pool.
//
// database/sql defaults to UNLIMITED, and this store is written to by one
// goroutine per cell. At 128 cells every worker opened its own connection, and
// together with the wallet's own pool that ran a stock PostgreSQL (200
// max_connections) out of slots within seconds of reaching full rate.
//
// The consequence was not a slow query. `recordCell` writes the write-ahead
// record that makes a crash mid-transition survivable, and a cell whose
// write-ahead record cannot be written is halted rather than allowed to proceed
// blind — correctly, but it means connection exhaustion destroys cells. A live
// run at 4 generations/second halted nine of them in under a minute.
//
// 16 is far below what the workload would take if allowed, and that is the
// point. Each write is a single indexed INSERT or UPDATE, so 16 connections
// serve thousands per second — well past 128 cells at any rate this automaton
// runs — while queuing costs milliseconds of backpressure. Exceeding the
// server's limit costs a cell.
//
// Sized small rather than merely finite because the limit is shared and mostly
// not ours to spend. On the machine where this was diagnosed a co-tenant
// application held 179 idle connections of the server's 200, leaving about
// twenty. An application that assumes it may open as many connections as it has
// goroutines is wrong even when it happens to fit.
const maxOpenConns = 16

// Open connects to the history store, creating its schema on first use.
//
// dsn selects PostgreSQL; empty falls back to a SQLite file beside the wallet,
// so a small deployment needs no extra service.
func Open(ctx context.Context, dsn, dataDir string) (*Store, error) {
	driver, target, postgres := "sqlite", filepath.Join(dataDir, "history.db"), false
	if dsn != "" {
		driver, target, postgres = "pgx", dsn, true
	}

	db, err := sql.Open(driver, target)
	if err != nil {
		return nil, fmt.Errorf("history: open %s: %w", driver, err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	// Idle connections are kept to match, so a burst does not pay reconnection
	// cost on every generation; SQLite ignores the distinction.
	db.SetMaxIdleConns(maxOpenConns)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("history: connect: %w", err)
	}

	s := &Store{db: db, postgres: postgres}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the connection.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS generations (
			number     BIGINT PRIMARY KEY,
			row_hex    TEXT   NOT NULL,
			created_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS cell_txs (
			generation BIGINT NOT NULL,
			cell       INTEGER NOT NULL,
			txid       TEXT   NOT NULL,
			status     TEXT   NOT NULL,
			err        TEXT   NOT NULL DEFAULT '',
			updated_at TIMESTAMP NOT NULL,
			PRIMARY KEY (generation, cell)
		)`,
		// The hot query is "which transactions are still in flight". A plain
		// index on status does NOT serve it: the predicate is `status NOT IN
		// (terminal...)`, which no planner will satisfy from an index on a
		// low-cardinality column, so it degrades to a full scan of the whole
		// history — minutes, every ten seconds, once the table is large.
		//
		// A PARTIAL index over exactly the non-terminal rows does serve it, and
		// it stays small because it only ever holds the in-flight set.
		`CREATE INDEX IF NOT EXISTS cell_txs_inflight ON cell_txs (updated_at)
		   WHERE status NOT IN ('mined', 'failed')`,
		`CREATE INDEX IF NOT EXISTS cell_txs_txid ON cell_txs (txid)`,
		// The other hot query is "what is cell c's newest record", asked once per
		// cell at startup by CellTips. The primary key is (generation, cell) — the
		// wrong column order for it, since a leading generation cannot serve a
		// predicate on cell — so without this the answer costs a scan of the whole
		// table per cell, or 128 scans of a table that grows by 128 rows per
		// generation forever.
		//
		// This is the index the whole derived-tip design rests on: with it, the
		// startup cost is O(cells x depth) regardless of how long the automaton
		// has been running.
		`CREATE INDEX IF NOT EXISTS cell_txs_cell_gen ON cell_txs (cell, generation)`,
		// Single-writer election. See AcquireLease.
		`CREATE TABLE IF NOT EXISTS leases (
			name       TEXT PRIMARY KEY,
			owner      TEXT NOT NULL,
			expires_at TIMESTAMP NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("history: migrate: %w", err)
		}
	}
	return nil
}

// rebind converts ? placeholders to $N for PostgreSQL.
func (s *Store) rebind(q string) string {
	if !s.postgres {
		return q
	}
	out := make([]byte, 0, len(q)+8)
	n := 0
	for i := range len(q) {
		if q[i] == '?' {
			n++
			out = append(out, '$')
			out = append(out, []byte(fmt.Sprint(n))...)
			continue
		}
		out = append(out, q[i])
	}
	return string(out)
}

// RecordGeneration stores a generation's row. Idempotent.
func (s *Store) RecordGeneration(ctx context.Context, number uint64, rowHex string) error {
	q := `INSERT INTO generations (number, row_hex, created_at) VALUES (?, ?, ?)
	      ON CONFLICT (number) DO NOTHING`
	_, err := s.db.ExecContext(ctx, s.rebind(q), number, rowHex, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("history: record generation %d: %w", number, err)
	}
	return nil
}

// RecordTx stores a cell's transaction, replacing any previous row for that
// (generation, cell) — a retried cell supersedes its earlier attempt.
func (s *Store) RecordTx(ctx context.Context, tx CellTx) error {
	q := `INSERT INTO cell_txs (generation, cell, txid, status, err, updated_at)
	      VALUES (?, ?, ?, ?, ?, ?)
	      ON CONFLICT (generation, cell) DO UPDATE
	        SET txid = EXCLUDED.txid, status = EXCLUDED.status,
	            err = EXCLUDED.err, updated_at = EXCLUDED.updated_at`
	_, err := s.db.ExecContext(ctx, s.rebind(q),
		tx.Generation, tx.Cell, tx.TxID, string(tx.Status), tx.Err, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("history: record cell %d generation %d: %w", tx.Cell, tx.Generation, err)
	}
	return nil
}

// UpdateStatus advances a transaction's status by txid.
func (s *Store) UpdateStatus(ctx context.Context, txid string, status Status, errMsg string) error {
	q := `UPDATE cell_txs SET status = ?, err = ?, updated_at = ? WHERE txid = ?`
	_, err := s.db.ExecContext(ctx, s.rebind(q), string(status), errMsg, time.Now().UTC(), txid)
	if err != nil {
		return fmt.Errorf("history: update %s: %w", txid, err)
	}
	return nil
}

// Unsettled returns the transactions that have not reached a terminal status.
//
// This is the live tracking set. Everything else is settled history and needs
// no further attention from the status stream or the reconciler.
func (s *Store) Unsettled(ctx context.Context) ([]CellTx, error) {
	q := `SELECT generation, cell, txid, status, err FROM cell_txs
	      WHERE status NOT IN ('mined', 'failed') ORDER BY generation, cell`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("history: query unsettled: %w", err)
	}
	defer rows.Close()
	return scanCells(rows)
}

// Stale returns up to limit transactions that have been in flight for longer
// than age, oldest first.
//
// This is what the reconciler polls, and both bounds matter. The age filter
// keeps it off transactions the status stream is about to resolve on its own —
// polling those is pure duplicated work. The limit keeps one pass bounded: the
// unfiltered set grows with the automaton, and a reconciler whose pass takes
// longer than its own interval never finishes a pass again.
func (s *Store) Stale(ctx context.Context, age time.Duration, limit int) ([]CellTx, error) {
	q := `SELECT generation, cell, txid, status, err FROM cell_txs
	      WHERE status NOT IN ('mined', 'failed') AND txid <> '' AND updated_at < ?
	      ORDER BY updated_at LIMIT ?`
	rows, err := s.db.QueryContext(ctx, s.rebind(q), time.Now().UTC().Add(-age), limit)
	if err != nil {
		return nil, fmt.Errorf("history: query stale: %w", err)
	}
	defer rows.Close()
	return scanCells(rows)
}

// Load returns generations in [from, from+limit), newest-inclusive, with their
// transactions attached.
func (s *Store) Load(ctx context.Context, from uint64, limit int) ([]Generation, error) {
	q := `SELECT number, row_hex FROM generations
	      WHERE number >= ? ORDER BY number LIMIT ?`
	rows, err := s.db.QueryContext(ctx, s.rebind(q), from, limit)
	if err != nil {
		return nil, fmt.Errorf("history: load generations: %w", err)
	}
	defer rows.Close()

	var gens []Generation
	index := map[uint64]int{}
	for rows.Next() {
		var g Generation
		if err := rows.Scan(&g.Number, &g.RowHex); err != nil {
			return nil, fmt.Errorf("history: scan generation: %w", err)
		}
		index[g.Number] = len(gens)
		gens = append(gens, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: load generations: %w", err)
	}
	if len(gens) == 0 {
		return nil, nil
	}

	last := gens[len(gens)-1].Number
	cq := `SELECT generation, cell, txid, status, err FROM cell_txs
	       WHERE generation >= ? AND generation <= ? ORDER BY generation, cell`
	crows, err := s.db.QueryContext(ctx, s.rebind(cq), gens[0].Number, last)
	if err != nil {
		return nil, fmt.Errorf("history: load cells: %w", err)
	}
	defer crows.Close()

	cells, err := scanCells(crows)
	if err != nil {
		return nil, err
	}
	for _, c := range cells {
		if i, ok := index[c.Generation]; ok {
			gens[i].Cells = append(gens[i].Cells, c)
		}
	}
	return gens, nil
}

// RowAt returns the row recorded for one generation, and whether there is one.
//
// A point lookup on the primary key, for the startup cross-check that the rows
// the store recorded when they were proved are the rows the seed and the rule
// reproduce. Absent is not an error: a generation the store never saw is
// perfectly ordinary, and only a DISAGREEMENT means anything.
func (s *Store) RowAt(ctx context.Context, number uint64) (string, bool, error) {
	var rowHex string
	q := `SELECT row_hex FROM generations WHERE number = ?`
	err := s.db.QueryRowContext(ctx, s.rebind(q), number).Scan(&rowHex)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("history: read generation %d: %w", number, err)
	}
	return rowHex, true, nil
}

// Stats summarises what has been recorded.
type Stats struct {
	Generations uint64
	Txs         uint64
	Unsettled   uint64
	Latest      uint64
}

// Stats returns totals for the UI.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	q := `SELECT
	        (SELECT COUNT(*) FROM generations),
	        (SELECT COALESCE(MAX(number), 0) FROM generations),
	        (SELECT COUNT(*) FROM cell_txs),
	        (SELECT COUNT(*) FROM cell_txs WHERE status NOT IN (?, ?))`
	err := s.db.QueryRowContext(ctx, s.rebind(q), string(StatusMined), string(StatusFailed)).
		Scan(&st.Generations, &st.Latest, &st.Txs, &st.Unsettled)
	if err != nil {
		return st, fmt.Errorf("history: stats: %w", err)
	}
	return st, nil
}

func scanCells(rows *sql.Rows) ([]CellTx, error) {
	var out []CellTx
	for rows.Next() {
		var c CellTx
		var status string
		if err := rows.Scan(&c.Generation, &c.Cell, &c.TxID, &status, &c.Err); err != nil {
			return nil, fmt.Errorf("history: scan cell: %w", err)
		}
		c.Status = Status(status)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: scan cells: %w", err)
	}
	return out, nil
}

// Tip is one recorded step of one cell's chain, newest first.
type Tip struct {
	Cell       int
	Generation uint64
	TxID       string
	Status     Status
	Err        string
}

// CellTips returns the newest depth records for each cell in [0, cells),
// newest generation first.
//
// This is the whole of what the store knows about where a cell has got to, and
// it answers three questions from one set of rows: what the tip is, whether the
// cell is halted, and whether its tip is unknown. Keeping them together is not
// tidiness — asking them separately is what let a rejection halt (the newest
// record being `failed`) and a lost broadcast (the newest record being
// `attempting`) be handled by different code paths, one of which was in memory
// only and did not survive a restart.
//
// It replaces UnresolvedAttempts, which grouped over the whole of cell_txs to
// find one row per cell. That is a full scan of a table growing by `cells` rows
// per generation, executed while the process is starting and before the UI is
// up. This is one indexed lookup per cell against cell_txs_cell_gen, bounded by
// depth, so its cost does not grow with the automaton's age.
//
// depth must be large enough to cover a retry chain — a rejection followed by
// the attempt that preceded it — but it is deliberately small: the caller walks
// these rows in memory, and any answer needing more history than this is one
// the caller should be refusing to guess at.
func (s *Store) CellTips(ctx context.Context, cells, depth int) (map[int][]Tip, error) {
	if cells <= 0 {
		return nil, fmt.Errorf("history: cell tips: cells must be positive, got %d", cells)
	}
	if depth <= 0 {
		return nil, fmt.Errorf("history: cell tips: depth must be positive, got %d", depth)
	}
	q := `SELECT generation, txid, status, err FROM cell_txs
	      WHERE cell = ? ORDER BY generation DESC LIMIT ?`

	out := make(map[int][]Tip, cells)
	for cell := range cells {
		rows, err := s.db.QueryContext(ctx, s.rebind(q), cell, depth)
		if err != nil {
			return nil, fmt.Errorf("history: query tips for cell %d: %w", cell, err)
		}
		tips, err := scanTips(cell, rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(tips) > 0 {
			out[cell] = tips
		}
	}
	return out, nil
}

func scanTips(cell int, rows *sql.Rows) ([]Tip, error) {
	var out []Tip
	for rows.Next() {
		t := Tip{Cell: cell}
		var status string
		if err := rows.Scan(&t.Generation, &t.TxID, &status, &t.Err); err != nil {
			return nil, fmt.Errorf("history: scan tip for cell %d: %w", cell, err)
		}
		t.Status = Status(status)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: query tips for cell %d: %w", cell, err)
	}
	return out, nil
}

// DeleteAttempt removes a write-ahead record, and only a write-ahead record.
//
// The status check is the whole safety property: if the attempt has since been
// resolved into a real transaction, this must not touch it.
func (s *Store) DeleteAttempt(ctx context.Context, generation uint64, cell int) error {
	q := `DELETE FROM cell_txs WHERE generation = ? AND cell = ? AND status = ?`
	_, err := s.db.ExecContext(ctx, s.rebind(q), generation, cell, string(StatusAttempting))
	if err != nil {
		return fmt.Errorf("history: delete attempt for cell %d generation %d: %w", cell, generation, err)
	}
	return nil
}

// DeepTip is what derivation needs about a cell buried under more unsettled
// records than the ordinary window covers.
type DeepTip struct {
	// Settled is the newest record naming a transaction the network accepted.
	// This is the cell's real tip however far back it is.
	Settled    Tip
	HasSettled bool

	// Failed is the newest rejection above Settled, and Attempt the newest
	// unresolved write-ahead record above it. Both can be present; an attempt
	// outranks a rejection, because an attempt means the tip is UNKNOWN while a
	// rejection means it is known and the chain simply stops there.
	Failed     Tip
	HasFailed  bool
	Attempt    Tip
	HasAttempt bool
}

// DeepTip answers, for one cell, where its chain actually stops.
//
// CellTips reads a fixed shallow window because in the healthy case one record
// is enough. That window is not enough for a cell that was rejected and then
// kept being rejected: the build that produced this deployment's older history
// advanced a cell's tip onto the output of a REJECTED transaction, so a single
// rejection cascaded — every later attempt spent an output that did not exist
// and was refused in turn. Cells 34, 51 and 91 of the live automaton each carry
// about 170 consecutive failures on top of a last good transaction near
// generation 300.
//
// Derivation seeing only failures concludes "this cell's tip cannot be derived"
// and an operator reads that as a cell that is lost. It is not lost: the tip is
// unambiguous, it is just further down than the window reaches. A long run of
// rejections is not ambiguity — it is a cascade with a known cause, and the
// newest ACCEPTED record is the answer regardless of how much wreckage sits on
// top of it.
//
// This is deliberately a per-cell fallback rather than a wider default window.
// The common path stays exactly as it was, and the cost of digging is paid only
// by the handful of cells that need it.
func (s *Store) DeepTip(ctx context.Context, cell int) (DeepTip, error) {
	var out DeepTip

	newest := func(statuses ...string) (Tip, bool, error) {
		holders := make([]string, len(statuses))
		args := []any{cell}
		for i, st := range statuses {
			holders[i] = "?"
			args = append(args, st)
		}
		q := `SELECT cell, generation, txid, status, coalesce(err,'') FROM cell_txs
		      WHERE cell = ? AND status IN (` + strings.Join(holders, ",") + `)
		      ORDER BY generation DESC LIMIT 1`
		var t Tip
		err := s.db.QueryRowContext(ctx, s.rebind(q), args...).
			Scan(&t.Cell, &t.Generation, &t.TxID, &t.Status, &t.Err)
		if errors.Is(err, sql.ErrNoRows) {
			return Tip{}, false, nil
		}
		if err != nil {
			return Tip{}, false, fmt.Errorf("history: deep tip for cell %d: %w", cell, err)
		}
		return t, true, nil
	}

	var err error
	out.Settled, out.HasSettled, err = newest(string(StatusMined), string(StatusBroadcast))
	if err != nil {
		return out, err
	}

	// Only records ABOVE the settled tip say anything about where the chain
	// stops. Anything below it was superseded by the tip itself.
	out.Failed, out.HasFailed, err = newest(string(StatusFailed))
	if err != nil {
		return out, err
	}
	if out.HasFailed && out.HasSettled && out.Failed.Generation <= out.Settled.Generation {
		out.HasFailed = false
	}
	out.Attempt, out.HasAttempt, err = newest(string(StatusAttempting))
	if err != nil {
		return out, err
	}
	if out.HasAttempt && out.HasSettled && out.Attempt.Generation <= out.Settled.Generation {
		out.HasAttempt = false
	}
	return out, nil
}

// RejectedTxIDs reports which of the given txids the record says the network
// refused.
//
// This exists for the legacy migration path, and the distinction it draws is the
// one that matters most there. A rejected transaction is perfectly well-formed:
// its bytes hash to its txid and its output carries the right covenant, so every
// structural check `import-tips` can make on it passes. What it does not have is
// an output — the network never accepted it, so nothing it "created" exists to
// be spent.
//
// state.json was written by a build whose recordCell advanced a cell's tip on
// broadcast rather than on acceptance, so for any cell halted by a rejection the
// file's tip IS the rejected transaction. Trusting it would point the cell at a
// phantom outpoint and un-halt a cell that is halted for a real reason. Asking
// the record directly is the only way to tell the two apart, and it is a bounded
// question — one lookup for at most `cells` txids.
func (s *Store) RejectedTxIDs(ctx context.Context, txids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(txids) == 0 {
		return out, nil
	}
	// Built rather than using ANY(): this store runs on both SQLite and Postgres
	// and array parameters are not portable between them.
	holders := make([]string, len(txids))
	args := make([]any, 0, len(txids)+1)
	for i, id := range txids {
		holders[i] = "?"
		args = append(args, id)
	}
	args = append(args, string(StatusFailed))
	q := `SELECT txid FROM cell_txs WHERE txid IN (` + strings.Join(holders, ",") + `) AND status = ?`

	rows, err := s.db.QueryContext(ctx, s.rebind(q), args...)
	if err != nil {
		return nil, fmt.Errorf("history: look up rejected txids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("history: scan rejected txid: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: look up rejected txids: %w", err)
	}
	return out, nil
}

// HasTxID reports whether a transaction id appears anywhere in this
// deployment's own record of cell transactions, at any generation, for any
// cell, in any status.
//
// It answers one question for UTXO_SPENT recovery: arcade has told us that some
// transaction already spent a cell's tip, and before that transaction can be
// adopted as the cell's real position we have to know it is not one we already
// hold. A txid we DO hold is not a lost transition — it is our own bookkeeping
// disagreeing with itself, which is a situation to be looked at rather than
// resolved by moving a live tip. See chain.RecoverSpentTip, condition 2.
//
// Deliberately status-blind, unlike RejectedTxIDs. "Do we know this
// transaction at all" is the question; narrowing it to a status would let a
// txid we recorded as failed pass as unknown, which is exactly the confusion
// worth halting on.
//
// One point lookup against cell_txs_txid.
func (s *Store) HasTxID(ctx context.Context, txid string) (bool, error) {
	if txid == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx, s.rebind(`SELECT 1 FROM cell_txs WHERE txid = ? LIMIT 1`), txid).
		Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("history: look up txid %s: %w", txid, err)
	}
	return true, nil
}

// AcquireLease takes or renews a named lease, returning whether it is held.
//
// This is what makes the automaton safe to deploy as anything but a single
// hand-run process. The engine owns 128 live UTXO chains; two instances
// advancing them would double-spend every one, and a rolling update runs two
// pods at once by design. Whoever holds the lease advances; everyone else
// serves the UI read-only.
//
// The lease is reclaimable rather than permanent, so a pod that dies without
// releasing does not wedge the deployment — the next one takes over once the
// lease expires. That means ttl must comfortably exceed the renewal interval.
func (s *Store) AcquireLease(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	now := time.Now().UTC()
	// The WHERE on the conflict path is the mutual exclusion: an existing lease
	// is only taken over by its current owner (a renewal) or once it has
	// expired. Anything else leaves the row untouched and reports 0 rows.
	q := `INSERT INTO leases (name, owner, expires_at) VALUES (?, ?, ?)
	      ON CONFLICT (name) DO UPDATE SET owner = EXCLUDED.owner, expires_at = EXCLUDED.expires_at
	      WHERE leases.owner = EXCLUDED.owner OR leases.expires_at < ?`
	res, err := s.db.ExecContext(ctx, s.rebind(q), name, owner, now.Add(ttl), now)
	if err != nil {
		return false, fmt.Errorf("history: acquire lease %q: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("history: acquire lease %q: %w", name, err)
	}
	return n > 0, nil
}

// ReleaseLease gives up a lease we hold, so a successor need not wait out the
// TTL after a clean shutdown.
func (s *Store) ReleaseLease(ctx context.Context, name, owner string) error {
	q := `DELETE FROM leases WHERE name = ? AND owner = ?`
	if _, err := s.db.ExecContext(ctx, s.rebind(q), name, owner); err != nil {
		return fmt.Errorf("history: release lease %q: %w", name, err)
	}
	return nil
}
