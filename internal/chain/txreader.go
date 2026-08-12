package chain

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/defs"

	_ "github.com/jackc/pgx/v5/stdlib" // postgres driver
	_ "modernc.org/sqlite"             // sqlite driver
)

// TxReader answers "what are the bytes of transaction T?" without opening the
// wallet.
//
// It exists for one reason: an audit has to be runnable against a deployment
// that is RUNNING. Chain.RawTx would answer the same question, but getting one
// costs a Chain — which migrates the wallet's storage schema, binds a wallet to
// it and starts a monitor daemon whose whole job is to write. Pointing a second
// one of those at a live deployment to read some old transactions is a bad
// trade: the audit would be interfering with the very automaton it is auditing,
// and any anomaly it then found would be partly its own doing.
//
// So this reads the one column it needs, directly, with a SELECT — the same
// posture Pruner takes towards the same database, for the same reason. It takes
// no lease and holds no lock. Two of these can run beside the engine and beside
// each other.
type TxReader struct {
	db       *sql.DB
	postgres bool

	// oracle is arcade, consulted only when the wallet's copy is gone. It is a
	// genuine fallback rather than a formality here, because `rule110 prune`
	// deliberately clears raw_tx for deeply-buried transactions: on a
	// long-running deployment the wallet is the authority for recent history and
	// nothing else.
	oracle arcade.TxOracle
}

// OpenTxReader connects to the wallet's database read-only, and to arcade if
// cfg names one.
func OpenTxReader(ctx context.Context, cfg Config, logger *slog.Logger) (*TxReader, error) {
	// The backend choice mirrors chain.Open and OpenPruner exactly, so this
	// always lands on the database the wallet is actually using. Reading a
	// different file would be worse than failing: it would report a deployment
	// that does not exist.
	driver, target, postgres := "sqlite", sqliteWalletDSN(cfg.DataDir), false
	if cfg.PostgresDSN != "" {
		driver, target, postgres = "pgx", cfg.PostgresDSN, true
	}
	db, err := sql.Open(driver, target)
	if err != nil {
		return nil, fmt.Errorf("chain: tx reader: open %s: %w", driver, err)
	}
	// A handful of connections: the audit fetches transactions one at a time and
	// the engine is already saturating this database.
	db.SetMaxOpenConns(2)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("chain: tx reader: connect: %w", err)
	}

	r := &TxReader{db: db, postgres: postgres}
	if cfg.ArcadeURL != "" {
		// No callback token and no events URL: this client only ever performs
		// GET /tx lookups, and subscribing would make the audit a second
		// consumer of the deployment's status stream.
		r.oracle = arcade.New(logger, nil, defs.Arcade{Enabled: true, URL: cfg.ArcadeURL})
	}
	return r, nil
}

// Close releases the connection.
func (r *TxReader) Close() error { return r.db.Close() }

// RawTx returns the bytes of txid, from the wallet's database if it still holds
// them and from arcade if it does not.
//
// The caller MUST check that they hash to the txid it asked for. Neither source
// is authenticated: the database is one this process opened by path, and arcade
// is a network service. Only the hash makes the bytes evidence.
func (r *TxReader) RawTx(ctx context.Context, txid string) ([]byte, error) {
	// The stored txid is BYTES, not the hex string everything above this layer
	// uses: metastore/codec.go's encTxID hex-decodes the display form and stores
	// the result. Querying with the string finds nothing at all — no error, no
	// row, just an audit that reports every transaction missing — so the
	// conversion is the whole reason this is not a one-line query.
	key, err := hex.DecodeString(txid)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("chain: %q is not a transaction id", txid)
	}

	placeholder := "?"
	if r.postgres {
		placeholder = "$1"
	}
	// known_txs.raw_tx is the toolbox's durable copy: it is written in the same
	// database transaction that records the txid and kept after mining, because
	// spending an output needs the transaction that created it. See the note at
	// the top of prune.go.
	q := "SELECT raw_tx FROM known_txs WHERE txid = " + placeholder

	var raw []byte
	err = r.db.QueryRowContext(ctx, q, key).Scan(&raw)
	switch {
	case err == nil && len(raw) > 0:
		return raw, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("chain: read raw transaction %s from the wallet database: %w", txid, err)
	}

	// Either no row, or a row whose raw_tx has been pruned to NULL. Both mean
	// the same thing to a reader: ask arcade.
	if r.oracle == nil {
		return nil, fmt.Errorf(
			"chain: no bytes for transaction %s: the wallet database has none and no arcade URL was "+
				"given to fall back on", txid)
	}
	rec, err := r.oracle.GetTx(ctx, txid)
	if err != nil {
		return nil, fmt.Errorf("chain: the wallet has no bytes for %s and arcade lookup failed: %w", txid, err)
	}
	if rec == nil || len(rec.RawTx) == 0 {
		return nil, fmt.Errorf(
			"chain: no bytes for transaction %s: the wallet database has none (pruned, or a different "+
				"wallet built it) and arcade returned none", txid)
	}
	return rec.RawTx, nil
}
