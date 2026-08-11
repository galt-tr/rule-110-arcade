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
	"github.com/dymurray/rule-110-arcade/internal/history"
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
	// TxFailed means it was rejected, or never got far enough to broadcast;
	// that cell's chain has halted.
	TxFailed TxState = "failed"
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

	// store is the durable record. Memory holds a window for the UI; the store
	// holds everything.
	store *history.Store

	// stepReq carries manual single-step requests.
	stepReq chan struct{}
	// changed is closed and replaced whenever the snapshot changes, so
	// subscribers can wait without polling.
	changed chan struct{}
}

// New creates an engine positioned at the chain's recorded state.
func New(ctx context.Context, c *chain.Chain, compiled *cellscript.Compiled, state *chain.State,
	store *history.Store, logger *slog.Logger) (*Engine, error) {

	seedHex := state.SeedHex
	if seedHex == "" {
		seedHex = state.RowHex
	}
	seed, err := ca.SeedHex(state.Cells, seedHex)
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
		store:    store,
	}
	loaded, err := loadHistory(ctx, store, state)
	if err != nil {
		return nil, err
	}
	if len(loaded) == 0 {
		// Nothing recorded yet: seed generation 0 from the genesis transaction
		// and persist it, so the very first row is durable too.
		g := genesisGeneration(state, seed)
		if err := store.RecordGeneration(ctx, 0, g.RowHex); err != nil {
			return nil, err
		}
		for _, c := range g.Cells {
			if err := store.RecordTx(ctx, history.CellTx{
				Generation: 0, Cell: c.Cell, TxID: c.TxID, Status: history.StatusSeen,
			}); err != nil {
				return nil, err
			}
		}
		loaded = []Generation{g}
	}
	e.history = loaded

	// Re-index whatever is still in flight so the status stream can find it.
	unsettled, err := store.Unsettled(ctx)
	if err != nil {
		return nil, err
	}
	// Indexing by generation number means no search: the previous version
	// scanned the whole 2048-entry window per unsettled transaction, which is
	// ~26 seconds of startup at 12.8M unsettled rows, single-threaded, before
	// the HTTP server comes up.
	for _, c := range unsettled {
		if c.TxID != "" {
			e.indexTx(c.TxID, c.Generation, c.Cell)
		}
	}
	e.logger.Info("history loaded", "generations", len(e.history), "unsettled", len(unsettled))

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

// loadHistory restores the recorded history: real rows with the real
// transactions that proved them.
//
// Rows could be recomputed from the seed, but the transaction ids could not —
// they exist only in the store, and they are the evidence the automaton ran on
// chain at all. Recomputing rows and discarding txids was the wrong trade.
func loadHistory(ctx context.Context, store *history.Store, state *chain.State) ([]Generation, error) {
	from := uint64(0)
	if state.Generation > maxHistory {
		from = state.Generation - maxHistory
	}
	recorded, err := store.Load(ctx, from, maxHistory+1)
	if err != nil {
		return nil, err
	}

	out := make([]Generation, 0, len(recorded))
	for _, g := range recorded {
		cells := make([]CellTx, state.Cells)
		for i := range state.Cells {
			cells[i] = CellTx{Cell: i, State: TxPending}
		}
		for _, c := range g.Cells {
			if c.Cell < 0 || c.Cell >= state.Cells {
				continue
			}
			cells[c.Cell] = CellTx{
				Cell: c.Cell, TxID: c.TxID, State: TxState(c.Status), Err: c.Err,
			}
		}
		out = append(out, Generation{Number: g.Number, RowHex: g.RowHex, Cells: cells})
	}
	return out, nil
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

	// Copy the CELLS, not just the generation headers. Generation.Cells is a
	// slice, so copying the outer slice alone leaves every backing array shared
	// with the live engine — and the caller marshals it after this lock is
	// released, while recordCell and applyStatus are writing into those same
	// arrays. That is a genuine data race on a string header, not a stale read.
	history := make([]Generation, len(e.history))
	for i, g := range e.history {
		cells := make([]CellTx, len(g.Cells))
		copy(cells, g.Cells)
		g.Cells = cells
		history[i] = g
	}

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

	// A generation number repeats whenever a previous attempt left a cell
	// unproved, because the row only advances once every cell has proved its
	// bit. Carry that attempt's cells forward: the ones that succeeded already
	// spent their UTXO and must not be advanced a second time, or they run
	// ahead of the automaton and the diagram stops describing the chain.
	genIdx := indexOfGeneration(e.history, gen.Number)
	if genIdx >= 0 {
		copy(gen.Cells, e.history[genIdx].Cells)
	}

	todo := make([]int, 0, state.Cells)
	for i := range state.Cells {
		switch {
		case e.halted[i]:
			gen.Cells[i] = CellTx{Cell: i, State: TxFailed, Err: "chain halted by an earlier rejection"}
		case state.Chains[i].Generation >= gen.Number:
			// Already proved this generation: its UTXO is spent, so advancing
			// it again would run the cell ahead of the automaton. The copy
			// above carried the transaction that proved it.
			//
			// Anything else the copy carried is stale and must not stand. In
			// particular a recorded failure cannot survive here: the chain
			// moved past this generation, so whatever went wrong was made good,
			// and leaving the cell failed would block the row forever — every
			// later attempt skips the cell, so nothing would ever clear it.
			if gen.Cells[i].State == "" || gen.Cells[i].State == TxFailed {
				gen.Cells[i] = CellTx{Cell: i, State: TxPending}
			}
		default:
			gen.Cells[i] = CellTx{Cell: i, State: TxPending}
			todo = append(todo, i)
		}
	}

	if genIdx >= 0 {
		e.history[genIdx] = gen
	} else {
		e.history = append(e.history, gen)
		if len(e.history) > maxHistory {
			e.history = e.history[len(e.history)-maxHistory:]
		}
		genIdx = len(e.history) - 1
	}
	e.notify()
	e.mu.Unlock()

	if err := e.store.RecordGeneration(ctx, gen.Number, gen.RowHex); err != nil {
		e.logger.ErrorContext(ctx, "persist generation", "generation", gen.Number, "err", err)
	}

	// Bound the fan-out. Releasing all N cells at once does not make a
	// single-writer store faster — the writers just queue on a lock, and the
	// whole generation stalls behind the resulting contention.
	limit := e.chain.Config.Concurrency
	if limit <= 0 {
		limit = state.Cells
	}
	sem := make(chan struct{}, limit)

	var wg sync.WaitGroup
	for _, cell := range todo {
		wg.Add(1)
		go func(cell int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := e.chain.AdvanceCell(ctx, e.compiled, state, cell, next)
			e.recordCell(gen.Number, cell, res, err)
		}(cell)
	}
	wg.Wait()

	// The row only advances once every cell has proved its bit. A cell that
	// failed leaves the automaton where it was, so the failure is visible
	// rather than silently skipped.
	e.mu.Lock()
	defer e.mu.Unlock()
	failed := 0
	if idx := indexOfGeneration(e.history, gen.Number); idx >= 0 {
		for _, c := range e.history[idx].Cells {
			if c.State == TxFailed {
				failed++
			}
		}
	}
	if failed == 0 {
		// Clear the banner: whatever went wrong on an earlier attempt at this
		// generation has been made good, and leaving the message up reports a
		// failure that no longer exists.
		e.lastError = ""
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

// indexOfGeneration finds a generation by number, or -1. It scans from the end
// because the only caller is looking for the newest generation.
func indexOfGeneration(gens []Generation, number uint64) int {
	for i := len(gens) - 1; i >= 0; i-- {
		if gens[i].Number == number {
			return i
		}
		if gens[i].Number < number {
			break // ordered by number: no earlier entry can match
		}
	}
	return -1
}

// recordCell stores one cell's outcome and updates its chain tip.
//
// generation is the generation NUMBER, resolved to a slice index here under the
// lock. A cell can finish long after its generation was appended, and the
// history trim shifts every index, so an index captured at dispatch time would
// address the wrong row by the time the result lands.
func (e *Engine) recordCell(generation uint64, cell int, res *chain.StepResult, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// The store is the durable record and does not depend on the window, so it
	// is written whether or not the generation is still displayable.
	genIdx := indexOfGeneration(e.history, generation)

	if err != nil {
		if genIdx >= 0 {
			e.history[genIdx].Cells[cell] = CellTx{Cell: cell, State: TxFailed, Err: err.Error()}
		}
		e.persist(history.CellTx{
			Generation: generation, Cell: cell,
			Status: history.StatusFailed, Err: err.Error(),
		})
		e.notify()
		return
	}

	if genIdx >= 0 {
		e.history[genIdx].Cells[cell] = CellTx{Cell: cell, TxID: res.TxID, State: TxBroadcast}
	}
	e.indexTx(res.TxID, generation, cell)
	e.persist(history.CellTx{
		Generation: generation, Cell: cell,
		TxID: res.TxID, Status: history.StatusBroadcast,
	})
	e.state.Chains[cell] = chain.CellChain{
		Cell:       cell,
		TxID:       res.TxID,
		Vout:       0,
		Satoshis:   e.state.Chains[cell].Satoshis,
		Generation: res.Generation,
		RowHex:     res.RowHex,
		RawTxHex:   res.RawTxHex,
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

// persist writes a cell transaction to the durable store.
//
// Failures are logged rather than propagated: losing a history row must not
// stop the automaton, and the reconciler will re-record the status later.
func (e *Engine) persist(c history.CellTx) {
	if err := e.store.RecordTx(context.Background(), c); err != nil {
		e.logger.Error("persist cell transaction",
			"generation", c.Generation, "cell", c.Cell, "err", err)
	}
}
