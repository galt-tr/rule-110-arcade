// Package engine drives the automaton: it computes generations, advances each
// cell's UTXO chain on the network, and exposes a snapshot for the UI.
//
// The row sequence is computed locally and immediately; the chain then PROVES
// each bit of it. That ordering is deliberate — it decouples the clock from
// confirmation latency, so the pattern draws at once and confirmation status
// washes down behind it rather than the display stalling on the network.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
	"github.com/dymurray/rule-110-arcade/internal/chain"
)

// TxState is how far one cell's transaction has got.
type TxState string

const (
	// TxPending means the transaction has not been built yet.
	TxPending TxState = "pending"
	// TxBroadcast means it was accepted by arcade but has no network verdict.
	TxBroadcast TxState = "broadcast"
	// TxSeen means the network has seen it.
	TxSeen TxState = "seen"
	// TxMined means it is in a block.
	TxMined TxState = "mined"
	// TxFailed means it was rejected; that cell's chain has halted.
	TxFailed TxState = "failed"
	// TxHistoric means the generation was replayed from the seed after a
	// restart: the row is exact, but the transaction that proved it is not in
	// this process's memory.
	TxHistoric TxState = "historic"
)

// Mode is the clock's behaviour.
type Mode string

const (
	// ModePaused advances nothing.
	ModePaused Mode = "paused"
	// ModeRunning advances continuously at Rate generations per second.
	ModeRunning Mode = "running"
)

// CellTx is one cell's transaction in one generation.
type CellTx struct {
	Cell  int     `json:"cell"`
	TxID  string  `json:"txid,omitempty"`
	State TxState `json:"state"`
	Err   string  `json:"err,omitempty"`
}

// Generation is one row of the space-time diagram.
type Generation struct {
	Number uint64   `json:"number"`
	RowHex string   `json:"row"`
	Cells  []CellTx `json:"cells"`
}

// Snapshot is the UI's view of the world.
type Snapshot struct {
	Cells       int          `json:"cells"`
	Rule        int          `json:"rule"`
	Mode        Mode         `json:"mode"`
	Rate        float64      `json:"rate"`
	Generation  uint64       `json:"generation"`
	History     []Generation `json:"history"`
	Balance     uint64       `json:"balance"`
	TotalTx     int          `json:"totalTx"`
	FailedCells int          `json:"failedCells"`
	Consensus   bool         `json:"consensus"`
	ArcadeURL   string       `json:"arcadeUrl"`
	GenesisTxID string       `json:"genesisTxid"`
	LastError   string       `json:"lastError,omitempty"`
}

// maxHistory bounds how many generations the UI keeps.
const maxHistory = 2048

// TailGenerations is how many generations a streamed update carries.
//
// A snapshot holds every cell of every generation, so the full history grows
// without bound as a payload: 128 cells with txids is roughly 10 KB per
// generation, and re-sending all of it several times a second would reach
// megabytes per message well before the diagram got interesting. Streamed
// updates therefore carry only the recent tail and the client merges it into
// the history it already has; older generations are terminal and do not change.
const TailGenerations = 48

// Engine owns the automaton's state and its clock.
type Engine struct {
	chain    *chain.Chain
	compiled *cellscript.Compiled
	logger   *slog.Logger

	mu        sync.RWMutex
	state     *chain.State
	history   []Generation
	mode      Mode
	rate      float64
	balance   uint64
	totalTx   int
	lastError string

	// txIndex maps a broadcast txid back to the cell that produced it, so
	// arcade's status stream can be reflected in the diagram.
	txIndex map[string]txLoc
	// halted marks cells whose chain is broken (a rejected transaction), so we
	// stop trying to spend outputs that do not exist.
	halted map[int]bool

	// stepReq carries manual single-step requests.
	stepReq chan struct{}
	// changed is closed and replaced whenever the snapshot changes, so
	// subscribers can wait without polling.
	changed chan struct{}
}

// New creates an engine positioned at the chain's recorded state.
func New(c *chain.Chain, compiled *cellscript.Compiled, state *chain.State, logger *slog.Logger) (*Engine, error) {
	row, err := state.Row()
	if err != nil {
		return nil, err
	}
	e := &Engine{
		chain:    c,
		compiled: compiled,
		logger:   logger,
		state:    state,
		mode:     ModePaused,
		rate:     1,
		stepReq:  make(chan struct{}, 1),
		changed:  make(chan struct{}),
		txIndex:  make(map[string]txLoc),
		halted:   make(map[int]bool),
	}
	history, err := rebuildHistory(state)
	if err != nil {
		return nil, err
	}
	_ = row
	e.history = history
	return e, nil
}

// genesisGeneration records generation 0, whose cells were all created by the
// single genesis transaction.
func genesisGeneration(state *chain.State, row ca.Row) Generation {
	cells := make([]CellTx, state.Cells)
	for i := range state.Cells {
		cells[i] = CellTx{Cell: i, TxID: state.GenesisTxID, State: TxSeen}
	}
	return Generation{Number: 0, RowHex: row.Hex(), Cells: cells}
}

// rebuildHistory reproduces every generation from the seed.
//
// History is held in memory, so a restart would otherwise show a blank diagram
// beside a generation counter in the hundreds. The automaton is deterministic:
// the seed and the rule regenerate every row exactly. The transaction ids of
// those older generations are not retained, so their cells are marked historic
// rather than claiming a confirmation status we cannot evidence.
func rebuildHistory(state *chain.State) ([]Generation, error) {
	seedHex := state.SeedHex
	if seedHex == "" {
		seedHex = state.RowHex // pre-dates the seed field; show what we can
	}
	row, err := ca.SeedHex(state.Cells, seedHex)
	if err != nil {
		return nil, err
	}

	history := []Generation{genesisGeneration(state, row)}
	for gen := uint64(1); gen <= state.Generation; gen++ {
		row = state.Rule.Step(row)
		cells := make([]CellTx, state.Cells)
		for i := range state.Cells {
			cells[i] = CellTx{Cell: i, State: TxHistoric}
		}
		history = append(history, Generation{Number: gen, RowHex: row.Hex(), Cells: cells})
	}
	if len(history) > maxHistory {
		history = history[len(history)-maxHistory:]
	}
	return history, nil
}

// SnapshotTail is Snapshot with the history trimmed to the most recent
// generations, for streaming.
func (e *Engine) SnapshotTail() Snapshot {
	s := e.Snapshot()
	if len(s.History) > TailGenerations {
		s.History = s.History[len(s.History)-TailGenerations:]
	}
	return s
}

// Snapshot returns the current view. Safe for concurrent use.
func (e *Engine) Snapshot() Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()

	history := make([]Generation, len(e.history))
	copy(history, e.history)

	failed := 0
	if n := len(history); n > 0 {
		for _, c := range history[n-1].Cells {
			if c.State == TxFailed {
				failed++
			}
		}
	}

	return Snapshot{
		Cells:       e.state.Cells,
		Rule:        int(e.state.Rule),
		Mode:        e.mode,
		Rate:        e.rate,
		Generation:  e.state.Generation,
		History:     history,
		Balance:     e.balance,
		TotalTx:     e.totalTx,
		FailedCells: failed,
		Consensus:   failed == 0,
		ArcadeURL:   e.chain.Config.ArcadeURL,
		GenesisTxID: e.state.GenesisTxID,
		LastError:   e.lastError,
	}
}

// Changed returns a channel closed the next time the snapshot changes.
func (e *Engine) Changed() <-chan struct{} {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.changed
}

// notify wakes every subscriber. Callers must hold the write lock.
func (e *Engine) notify() {
	close(e.changed)
	e.changed = make(chan struct{})
}

// SetMode starts or stops the clock.
func (e *Engine) SetMode(m Mode) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mode = m
	e.notify()
}

// SetRate sets generations per second, clamped to something a chain can serve.
func (e *Engine) SetRate(r float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rate = min(max(r, 0.05), 20)
	e.notify()
}

// Step requests exactly one generation. Non-blocking: a request already queued
// is left as-is rather than stacking up behind a slow generation.
func (e *Engine) Step() {
	select {
	case e.stepReq <- struct{}{}:
	default:
	}
}

// Run drives the clock until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	go e.trackBalance(ctx)
	go e.watchStatus(ctx)
	go e.reconcile(ctx)

	for {
		e.mu.RLock()
		mode, rate := e.mode, e.rate
		e.mu.RUnlock()

		if mode == ModeRunning {
			e.advance(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(float64(time.Second) / rate)):
			}
			continue
		}

		// Paused: wait for a manual step, a mode change, or cancellation.
		select {
		case <-ctx.Done():
			return
		case <-e.stepReq:
			e.advance(ctx)
		case <-e.Changed():
		}
	}
}

// advance moves every cell forward one generation.
//
// Cells are independent, so they go out concurrently; the generation is
// recorded as soon as the rows are known, and each cell's transaction status is
// filled in as it completes.
func (e *Engine) advance(ctx context.Context) {
	e.mu.Lock()
	state := e.state
	row, err := state.Row()
	if err != nil {
		e.lastError = err.Error()
		e.mu.Unlock()
		return
	}
	next := state.Rule.Step(row)
	gen := Generation{
		Number: state.Generation + 1,
		RowHex: next.Hex(),
		Cells:  make([]CellTx, state.Cells),
	}
	for i := range state.Cells {
		if e.halted[i] {
			gen.Cells[i] = CellTx{Cell: i, State: TxFailed, Err: "chain halted by an earlier rejection"}
			continue
		}
		gen.Cells[i] = CellTx{Cell: i, State: TxPending}
	}
	e.history = append(e.history, gen)
	if len(e.history) > maxHistory {
		e.history = e.history[len(e.history)-maxHistory:]
	}
	genIdx := len(e.history) - 1
	e.notify()
	e.mu.Unlock()

	// Bound the fan-out. Releasing all N cells at once does not make a
	// single-writer store faster — the writers just queue on a lock, and the
	// whole generation stalls behind the resulting contention.
	limit := e.chain.Config.Concurrency
	if limit <= 0 {
		limit = state.Cells
	}
	sem := make(chan struct{}, limit)

	var wg sync.WaitGroup
	for cell := range state.Cells {
		e.mu.RLock()
		skip := e.halted[cell]
		e.mu.RUnlock()
		if skip {
			continue
		}
		wg.Add(1)
		go func(cell int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := e.chain.AdvanceCell(ctx, e.compiled, state, cell, next)
			e.recordCell(genIdx, cell, res, err)
		}(cell)
	}
	wg.Wait()

	// The row only advances once every cell has proved its bit. A cell that
	// failed leaves the automaton where it was, so the failure is visible
	// rather than silently skipped.
	e.mu.Lock()
	defer e.mu.Unlock()
	failed := 0
	for _, c := range e.history[genIdx].Cells {
		if c.State == TxFailed {
			failed++
		}
	}
	if failed == 0 {
		e.state.Generation = gen.Number
		e.state.RowHex = gen.RowHex
		if err := e.chain.SaveState(e.state); err != nil {
			e.lastError = err.Error()
		}
	} else {
		e.lastError = fmt.Sprintf("generation %d: %d/%d cells failed", gen.Number, failed, state.Cells)
	}
	e.notify()
}

// recordCell stores one cell's outcome and updates its chain tip.
func (e *Engine) recordCell(genIdx, cell int, res *chain.StepResult, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if genIdx >= len(e.history) {
		return // history was trimmed underneath us
	}
	if err != nil {
		e.history[genIdx].Cells[cell] = CellTx{Cell: cell, State: TxFailed, Err: err.Error()}
		e.notify()
		return
	}

	e.history[genIdx].Cells[cell] = CellTx{Cell: cell, TxID: res.TxID, State: TxBroadcast}
	e.indexTx(res.TxID, genIdx, cell)
	e.state.Chains[cell] = chain.CellChain{
		Cell:       cell,
		TxID:       res.TxID,
		Vout:       0,
		Satoshis:   e.state.Chains[cell].Satoshis,
		Generation: res.Generation,
		RowHex:     res.RowHex,
		BEEFHex:    res.BEEFHex,
	}
	e.totalTx++
	e.notify()
}

// trackBalance refreshes the reported balance periodically.
func (e *Engine) trackBalance(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		if b, err := e.chain.Balance(ctx); err == nil {
			e.mu.Lock()
			e.balance = b
			e.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
