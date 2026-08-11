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
// state file below tracks them itself.
const CellBasket = "rule110cells"

// stateFile records the automaton's position on chain between runs.
const stateFile = "state.json"

// State is the durable record of where each cell chain has got to.
type State struct {
	// Cells is the ring size and Rule the automaton rule, both fixed at genesis
	// because they are compiled into every cell's locking script.
	Cells int     `json:"cells"`
	Rule  ca.Rule `json:"rule"`

	// GenesisTxID is the transaction that created generation 0.
	GenesisTxID string `json:"genesisTxid"`

	// Generation is the generation number Row describes.
	Generation uint64 `json:"generation"`

	// RowHex is the current row, hex-encoded.
	RowHex string `json:"row"`

	// SeedHex is generation 0's row. The automaton is deterministic, so the
	// seed plus the rule reproduces every row that has ever existed — which is
	// what lets the diagram be rebuilt after a restart instead of starting
	// blank at whatever generation the chain had reached.
	SeedHex string `json:"seed"`

	// Chains is the per-cell tip, indexed by cell.
	Chains []CellChain `json:"chains"`
}

// CellChain is one cell's position: the UTXO to spend next, and the BEEF that
// proves where it came from.
type CellChain struct {
	Cell       int    `json:"cell"`
	TxID       string `json:"txid"`
	Vout       uint32 `json:"vout"`
	Satoshis   uint64 `json:"satoshis"`
	Generation uint64 `json:"generation"`

	// RowHex is the row THIS cell's UTXO carries. Cells can sit at different
	// generations, so the global State.RowHex is not a safe substitute: the
	// locking script and the sighash preimage are both derived from the row the
	// UTXO actually holds, and using the wrong one fails with a bare
	// OP_CHECKSIGVERIFY that gives no hint why.
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
	// LoadState converts it and drops it. Transitional: delete this along with
	// the state file itself.
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

// Row returns the state's current row.
func (s *State) Row() (ca.Row, error) { return ca.SeedHex(s.Cells, s.RowHex) }

// Clone returns a deep copy, so the caller can write it to disk without
// holding a lock over the encode.
func (s *State) Clone() *State {
	out := *s
	out.Chains = make([]CellChain, len(s.Chains))
	copy(out.Chains, s.Chains)
	return &out
}

// Genesis creates generation 0: one output per cell, all carrying the same row.
//
// Every cell is created in a SINGLE transaction, which is what establishes the
// invariant the whole design rests on — all N chains start from an identical
// row, so any later divergence is visible.
func (c *Chain) Genesis(ctx context.Context, compiled *cellscript.Compiled, seed ca.Row) (*State, error) {
	n := compiled.Cells()
	if seed.Cells() != n {
		return nil, fmt.Errorf("chain: seed has %d cells, contract expects %d", seed.Cells(), n)
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

	genesisTx, err := transaction.NewTransactionFromBEEF(res.Tx)
	if err != nil {
		return nil, fmt.Errorf("chain: re-parse genesis transaction: %w", err)
	}
	rawHex := hex.EncodeToString(genesisTx.Bytes())

	state := &State{
		Cells:       n,
		Rule:        compiled.Rule(),
		GenesisTxID: txid,
		Generation:  0,
		RowHex:      seed.Hex(),
		SeedHex:     seed.Hex(),
		Chains:      make([]CellChain, n),
	}
	for cell := range n {
		state.Chains[cell] = CellChain{
			Cell:       cell,
			TxID:       txid,
			Vout:       uint32(cell),
			Satoshis:   c.Config.CellSatoshis,
			Generation: 0,
			RowHex:     seed.Hex(),
			RawTxHex:   rawHex,
		}
	}
	if err := c.SaveState(state); err != nil {
		return nil, err
	}
	return state, nil
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

// SaveState writes the automaton's position, atomically.
func (c *Chain) SaveState(s *State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("chain: encode state: %w", err)
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

// LoadState reads the automaton's position.
func (c *Chain) LoadState() (*State, error) {
	path := filepath.Join(c.Config.DataDir, stateFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("chain: read %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("chain: parse %s: %w", path, err)
	}
	// Backfill per-cell rows for state written before CellChain carried one.
	// Only cells still at the global generation can be assumed to hold the
	// global row; a cell that ran ahead has an unknown row and must be
	// reported rather than guessed at.
	for i := range s.Chains {
		if s.Chains[i].RowHex != "" {
			continue
		}
		if s.Chains[i].Generation != s.Generation {
			return nil, fmt.Errorf(
				"chain: cell %d is at generation %d but the automaton is at %d, and the "+
					"state file predates per-cell rows so its row cannot be recovered; "+
					"re-run genesis", i, s.Chains[i].Generation, s.Generation)
		}
		s.Chains[i].RowHex = s.RowHex
	}
	if err := s.migrateLegacyBEEF(); err != nil {
		return nil, err
	}
	return &s, nil
}

// migrateLegacyBEEF converts tips still carrying a whole atomic BEEF into the
// single transaction that is all a successor actually needs.
//
// The tip transaction is the subject of its own atomic BEEF, so this is a
// lossless projection — it just discards the ancestry that should never have
// been kept. On a 128-cell automaton at generation 63 it turns a 175 MB state
// file into roughly 512 KB.
func (s *State) migrateLegacyBEEF() error {
	for i := range s.Chains {
		c := &s.Chains[i]
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
