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
	"fmt"
	"path/filepath"
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

// UnresolvedAttempts returns the cells whose newest record is a write-ahead
// attempt that was never resolved, mapped to the generation attempted.
//
// Each of these means a transaction may have been broadcast and then lost to a
// crash: the cell's real tip is unknown, and advancing it from the last recorded
// tip would double-spend. See StatusAttempting.
func (s *Store) UnresolvedAttempts(ctx context.Context) (map[int]uint64, error) {
	q := `SELECT c.cell, c.generation FROM cell_txs c
	      JOIN (SELECT cell, MAX(generation) AS g FROM cell_txs GROUP BY cell) m
	        ON m.cell = c.cell AND m.g = c.generation
	      WHERE c.status = ?`
	rows, err := s.db.QueryContext(ctx, s.rebind(q), string(StatusAttempting))
	if err != nil {
		return nil, fmt.Errorf("history: query unresolved attempts: %w", err)
	}
	defer rows.Close()

	out := map[int]uint64{}
	for rows.Next() {
		var cell int
		var generation uint64
		if err := rows.Scan(&cell, &generation); err != nil {
			return nil, fmt.Errorf("history: scan unresolved attempt: %w", err)
		}
		out[cell] = generation
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history: query unresolved attempts: %w", err)
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
