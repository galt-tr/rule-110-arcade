package chain

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	sdk "github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
)

// CellBasket holds the automaton's cell UTXOs.
//
// Note these are NOT wallet-spendable in the usual sense: only change outputs
// are minted into the utxostore, so ListOutputs reports them Spendable=false by
// design. We spend them by naming their outpoints explicitly, which is why the
// outpoint has to come from somewhere else — the history store records which
// transaction each cell is on, and DeriveTip reconstructs the rest from the
// transaction itself.
const CellBasket = "rule110cells"

// stateFile records the deployment's immutable facts. The name is unchanged
// from when it also held the automaton's position, so an existing deployment
// keeps running.
const stateFile = "state.json"

// Deployment is what genesis fixed and nothing may change afterwards.
//
// It used to be called State and it used to carry the per-cell tips as well.
// That was the bug the whole tip rework exists to remove: a tip is MUTABLE
// (it moves every generation, 128 times per row) while everything here is
// immutable (it is compiled into the locking scripts and cannot change without
// a new genesis), and keeping both in one file meant a durable record that had
// to be rewritten on the hot path, was written on a one-second timer, and could
// therefore disagree with the history store about where a cell actually was.
// It did disagree: the live file held one cell at generation 1279 while the rest
// sat at 806-837, and nothing in the program cross-checked it against anything.
//
// Written once, at genesis, and read-only from then on. Where each cell has got
// to now lives in exactly one place — the history store — and the bytes behind
// each tip are fetched by txid, which cannot go stale. See DeriveTip.
type Deployment struct {
	// Cells is the ring size and Rule the automaton rule, both fixed at genesis
	// because they are compiled into every cell's locking script.
	Cells int     `json:"cells"`
	Rule  ca.Rule `json:"rule"`

	// GenesisTxID is the transaction that created generation 0.
	GenesisTxID string `json:"genesisTxid"`

	// SeedHex is generation 0's row. The automaton is deterministic, so the
	// seed plus the rule reproduces every row that has ever existed — which is
	// what lets the diagram be rebuilt after a restart instead of starting
	// blank at whatever generation the chain had reached, and what lets a tip's
	// row be RECOMPUTED rather than recorded.
	SeedHex string `json:"seed"`

	// CellSatoshis is what each cell UTXO was created carrying. Informational:
	// derivation takes a tip's value from the output itself, because the output
	// is the authority and this is only a note about what was intended. A file
	// written before this field existed simply has 0 here.
	CellSatoshis uint64 `json:"cellSatoshis,omitempty"`

	// legacyTips is the `chains[]` array of a deployment written before tips
	// were derived. It is NOT authoritative and is never written back — it
	// exists so startup can refuse to run when the history store is behind it
	// (which would silently re-spend live outputs), and so `import-tips` can
	// backfill the store from it after verifying every entry. See LegacyTips.
	legacyTips []CellChain
}

// legacyState is the on-disk shape, including the fields Deployment no longer
// owns. Parsed, never written.
type legacyState struct {
	Deployment
	Generation uint64      `json:"generation"`
	RowHex     string      `json:"row"`
	Chains     []CellChain `json:"chains"`
}

// LegacyTips returns the per-cell tips recorded by an older build, or nil.
//
// The caller must treat these as a claim to be checked, never as a source of
// truth: they are the record that was allowed to drift, and acting on one
// without verifying its bytes and its script is exactly the mistake that
// re-spends a spent output.
func (d *Deployment) LegacyTips() []CellChain { return d.legacyTips }

// Seed returns generation 0's row.
func (d *Deployment) Seed() (ca.Row, error) { return ca.SeedHex(d.Cells, d.SeedHex) }

// CellChain is one cell's position: the UTXO to spend next, plus the bytes of
// the transaction that created it.
//
// Not durable, and no longer serialized anywhere the program reads back. It is
// DERIVED — see DeriveTip — from a txid and a generation held in the history
// store. The JSON tags survive only so a legacy state.json's `chains[]` can
// still be parsed for the migration floor check and by `import-tips`.
type CellChain struct {
	Cell       int    `json:"cell"`
	TxID       string `json:"txid"`
	Vout       uint32 `json:"vout"`
	Satoshis   uint64 `json:"satoshis"`
	Generation uint64 `json:"generation"`

	// RowHex is the row THIS cell's UTXO carries. Cells sit at different
	// generations, so a single automaton-wide row is not a safe substitute: the
	// locking script and the sighash preimage are both derived from the row the
	// UTXO actually holds, and using the wrong one fails with a bare
	// OP_CHECKSIGVERIFY that gives no hint why.
	//
	// Recomputed from the seed at the cell's own generation, then CHECKED against
	// the locking script the transaction really carries. It is not taken on trust
	// from anywhere.
	RowHex string `json:"row"`

	// RawTxHex is the transaction that created this UTXO, and ONLY that
	// transaction. CreateAction needs the source transaction as InputBEEF when
	// spending, because it is not recovered automatically for caller-provided
	// inputs — but it needs nothing behind it.
	//
	// Storing the atomic BEEF here instead is the single most expensive mistake
	// this program has made. A cell is an unbroken self-spending chain, so its
	// BEEF carries every generation back to genesis: it grew ~11 KB per
	// generation per cell, reached 694 KB per cell by generation 63, and was
	// handed straight back to CreateAction as InputBEEF, which storage then
	// persisted twice per transaction (known_txs and transactions). The state
	// file hit 175 MB and the wallet database 12 GB after 8,000 transactions.
	// The successor is rebuilt from these bytes on demand by tipBEEF.
	RawTxHex string `json:"rawTx"`

	// LegacyBEEFHex reads the field RawTxHex replaced, so an automaton written
	// by an older build keeps running instead of needing a fresh genesis.
	// LoadDeployment converts it and drops it. Transitional: it can go once no
	// deployment needs `import-tips` any more.
	LegacyBEEFHex string `json:"beef,omitempty"`
}

// tipBEEF wraps the tip transaction in a BEEF carrying nothing but itself.
//
// That is all CreateAction wants: storage's hydrateInputs only looks the source
// transaction up by hash, and arcade validates the extended-format broadcast
// from each input's inline prevout, so the ancestry behind it is never read. The
// create path does not call VerifyBeef at all — only InternalizeAction does.
func (c CellChain) tipBEEF() ([]byte, error) {
	raw, err := hex.DecodeString(c.RawTxHex)
	if err != nil {
		return nil, fmt.Errorf("chain: decode stored transaction for cell %d: %w", c.Cell, err)
	}
	beef := transaction.NewBeefV2()
	if _, err := beef.MergeRawTx(raw, nil); err != nil {
		return nil, fmt.Errorf("chain: wrap tip transaction for cell %d: %w", c.Cell, err)
	}
	out, err := beef.Bytes()
	if err != nil {
		return nil, fmt.Errorf("chain: encode tip beef for cell %d: %w", c.Cell, err)
	}
	return out, nil
}

// Genesis creates generation 0: one output per cell, all carrying the same row.
//
// Every cell is created in a SINGLE transaction, which is what establishes the
// invariant the whole design rests on — all N chains start from an identical
// row, so any later divergence is visible.
func (c *Chain) Genesis(ctx context.Context, compiled *cellscript.Compiled, seed ca.Row) (*Deployment, error) {
	n := compiled.Cells()
	if seed.Cells() != n {
		return nil, fmt.Errorf("chain: seed has %d cells, contract expects %d", seed.Cells(), n)
	}

	// Refuse before spending anything. A second genesis would mint 128 new cells
	// and overwrite the record of the 128 that already exist, orphaning every
	// live UTXO the deployment owns with no way back to them — the file is the
	// only record of the genesis txid the old cells descend from.
	path := filepath.Join(c.Config.DataDir, stateFile)
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf(
			"chain: %s already exists, so this deployment has already had a genesis; "+
				"move it aside deliberately if you really mean to abandon the existing cells", path)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("chain: check %s: %w", path, err)
	}

	outputs := make([]sdk.CreateActionOutput, 0, n)
	for cell := range n {
		lock, err := compiled.LockingScript(cell, seed)
		if err != nil {
			return nil, err
		}
		instructions, err := json.Marshal(map[string]any{"cell": cell, "generation": 0})
		if err != nil {
			return nil, fmt.Errorf("chain: encode custom instructions: %w", err)
		}
		outputs = append(outputs, sdk.CreateActionOutput{
			LockingScript: lock,
			Satoshis:      c.Config.CellSatoshis,
			// Descriptions have a 5-byte minimum.
			OutputDescription:  fmt.Sprintf("rule110 cell %d generation 0", cell),
			Basket:             CellBasket,
			CustomInstructions: string(instructions),
			Tags:               []string{"rule110", "genesis"},
		})
	}

	no, yes := false, true
	args := sdk.CreateActionArgs{
		Description: fmt.Sprintf("rule110 genesis: %d cells, rule %d", n, compiled.Rule()),
		Outputs:     outputs,
		Labels:      []string{"rule110", "genesis"},
		Options: &sdk.CreateActionOptions{
			// Cell i MUST land at vout i: the state file and every later spend
			// address cells by output index. RandomizeOutputs defaults to true.
			RandomizeOutputs: &no,
			SignAndProcess:   &yes,
			// Broadcast inline so genesis either succeeds or fails now, rather
			// than being queued for the monitor.
			AcceptDelayedBroadcast: &no,
		},
	}

	// Retry while the funder reports no usable coins.
	//
	// BasketClaimableCount reads the metastore basket, but the funder claims
	// from the utxostore inventory, and a coin only enters that inventory when
	// the monitor applies its status or proof. Those run on a timer, so a
	// freshly-started process can see a healthy balance and still have nothing
	// to spend. Asking the funder directly is the only honest readiness check.
	res, err := retryUntilFunded(ctx, func() (*sdk.CreateActionResult, error) {
		return c.Wallet.CreateAction(ctx, args, c.Config.Originator)
	})
	if err != nil {
		return nil, fmt.Errorf("chain: create genesis action: %w", err)
	}

	txid := res.Txid.String()

	// Confirm the broadcast transaction really carries our scripts where we
	// expect them, rather than trusting the ordering option.
	if err := verifyGenesisLayout(res.Tx, compiled, seed, c.Config.CellSatoshis); err != nil {
		return nil, err
	}

	// The per-cell tips are deliberately NOT recorded here. Cell c's tip at
	// generation 0 is this transaction's output c, which is derivable from the
	// three facts below plus the transaction's own bytes — so recording it would
	// create a second copy of something that can be recomputed, and a second copy
	// is a thing that can disagree. See DeriveTip.
	d := &Deployment{
		Cells:        n,
		Rule:         compiled.Rule(),
		GenesisTxID:  txid,
		SeedHex:      seed.Hex(),
		CellSatoshis: c.Config.CellSatoshis,
	}
	if err := c.saveDeployment(d); err != nil {
		return nil, err
	}
	return d, nil
}

// verifyGenesisLayout re-parses the signed transaction and checks each cell's
// output is where and what we asked for.
func verifyGenesisLayout(atomicBEEF []byte, compiled *cellscript.Compiled, seed ca.Row, sats uint64) error {
	tx, err := transaction.NewTransactionFromBEEF(atomicBEEF)
	if err != nil {
		return fmt.Errorf("chain: re-parse genesis transaction: %w", err)
	}
	for cell := range compiled.Cells() {
		if cell >= len(tx.Outputs) {
			return fmt.Errorf("chain: genesis has %d outputs, expected at least %d",
				len(tx.Outputs), compiled.Cells())
		}
		want, err := compiled.LockingScript(cell, seed)
		if err != nil {
			return err
		}
		got := tx.Outputs[cell]
		if got.LockingScript.String() != hex.EncodeToString(want) {
			return fmt.Errorf("chain: genesis output %d is not cell %d's script "+
				"(outputs were reordered?)", cell, cell)
		}
		if got.Satoshis != sats {
			return fmt.Errorf("chain: genesis output %d carries %d sat, expected %d",
				cell, got.Satoshis, sats)
		}
	}
	return nil
}

// saveDeployment writes the deployment's facts, atomically and exactly once.
//
// Unexported, and there is no exported counterpart: nothing outside genesis has
// any business rewriting this file. Making that a compile-time fact rather than
// a convention is the point — the previous version's SaveState was called from
// the engine's checkpointer, from `step`, and from the depth probe, three
// writers to a file that was also the only record of where 128 live UTXOs were.
func (c *Chain) saveDeployment(d *Deployment) error {
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("chain: encode deployment: %w", err)
	}
	path := filepath.Join(c.Config.DataDir, stateFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("chain: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("chain: install %s: %w", path, err)
	}
	return nil
}

// LoadDeployment reads the facts genesis fixed.
//
// A file written by an older build also carries `chains[]`. Those tips are
// parsed into legacyTips and go no further: they are a claim about where the
// cells were when the file was last written, on a one-second timer, by a process
// that may have been killed at any point after. Startup uses them for one
// purpose only — refusing to run when derivation lands BELOW them, which would
// mean the history store is behind and the automaton is about to re-spend
// outputs that are already gone.
func (c *Chain) LoadDeployment() (*Deployment, error) {
	return LoadDeploymentFrom(c.Config.DataDir)
}

// LoadDeploymentFrom reads the same facts straight from a data directory,
// without a wallet.
//
// The file is the whole source, so requiring a *Chain to read it was only a
// habit — and an expensive one for a read-only tool. Opening a Chain starts a
// second monitor daemon against the live deployment's storage, which is exactly
// what `rule110 audit` must not do while the engine is running. The ring size
// and the rule are all it needs, and they are right here.
func LoadDeploymentFrom(dataDir string) (*Deployment, error) {
	path := filepath.Join(dataDir, stateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("chain: read %s: %w", path, err)
	}
	var s legacyState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("chain: parse %s: %w", path, err)
	}
	d := s.Deployment
	if d.SeedHex == "" {
		// Predates the seed field: the only file shape where the global row is
		// the seed is one that never advanced past generation 0.
		if s.Generation != 0 || s.RowHex == "" {
			return nil, fmt.Errorf(
				"chain: %s records no seed and the automaton is at generation %d, so the row sequence "+
					"cannot be reproduced; this deployment cannot be resumed", path, s.Generation)
		}
		d.SeedHex = s.RowHex
	}
	d.legacyTips = s.Chains
	if err := migrateLegacyBEEF(d.legacyTips); err != nil {
		return nil, err
	}
	return &d, nil
}

// migrateLegacyBEEF converts tips still carrying a whole atomic BEEF into the
// single transaction that is all a successor actually needs.
//
// The tip transaction is the subject of its own atomic BEEF, so this is a
// lossless projection — it just discards the ancestry that should never have
// been kept. On a 128-cell automaton at generation 63 it turns a 175 MB state
// file into roughly 512 KB.
//
// It survives the move to derived tips because `import-tips` still has to read
// these bytes to backfill the store, and a deployment old enough to need the
// import is exactly the kind old enough to be carrying BEEFs.
func migrateLegacyBEEF(tips []CellChain) error {
	for i := range tips {
		c := &tips[i]
		if c.RawTxHex != "" || c.LegacyBEEFHex == "" {
			c.LegacyBEEFHex = ""
			continue
		}
		beefBytes, err := hex.DecodeString(c.LegacyBEEFHex)
		if err != nil {
			return fmt.Errorf("chain: decode legacy beef for cell %d: %w", c.Cell, err)
		}
		tx, err := transaction.NewTransactionFromBEEF(beefBytes)
		if err != nil {
			return fmt.Errorf("chain: parse legacy beef for cell %d: %w", c.Cell, err)
		}
		c.RawTxHex = hex.EncodeToString(tx.Bytes())
		c.LegacyBEEFHex = ""
	}
	return nil
}

// fundingWait bounds how long to wait for the monitor to make coins claimable.
// The CheckForProofs task runs once a minute, so this allows several cycles.
const fundingWait = 4 * time.Minute

// retryUntilFunded retries create while the funder reports insufficient funds.
func retryUntilFunded(ctx context.Context, create func() (*sdk.CreateActionResult, error)) (*sdk.CreateActionResult, error) {
	deadline := time.Now().Add(fundingWait)
	for attempt := 1; ; attempt++ {
		res, err := create()
		if err == nil {
			return res, nil
		}
		if !strings.Contains(err.Error(), "not enough funds") {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%w (waited %s over %d attempts; "+
				"is the funding transaction mined, and is the monitor running?)",
				err, fundingWait, attempt)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		// Short: a coin usually becomes claimable as soon as the monitor
		// applies its status, and a whole generation waits behind the slowest
		// cell here.
		case <-time.After(2 * time.Second):
		}
	}
}

// Row returns the row this cell's UTXO carries.
func (c CellChain) Row(cells int) (ca.Row, error) { return ca.SeedHex(cells, c.RowHex) }
