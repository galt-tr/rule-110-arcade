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
	"os"
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
	// ModeStarved means there is not enough coin to buy another transaction.
	//
	// This is a normal state, not a failure. Cells stop at their next loop top,
	// every chain is left on a signed and broadcast tip, and the status stream,
	// reconciler and UI keep running so in-flight work still settles. The clock
	// resumes on its own once the balance poller sees funds again.
	ModeStarved Mode = "starved"
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
	Cells      int          `json:"cells"`
	Rule       int          `json:"rule"`
	Mode       Mode         `json:"mode"`
	Rate       float64      `json:"rate"`
	Generation uint64       `json:"generation"`
	History    []Generation `json:"history"`
	// Balance is what the funder can actually claim — NOT the wallet total.
	// Reserve is what remains to mint more spendable coin from, and PoolCoins
	// how many claimable coins back the balance. PoolCoins is the one that
	// predicts starvation: one coin funds one transition.
	Balance   uint64 `json:"balance"`
	Reserve   uint64 `json:"reserve"`
	PoolCoins int    `json:"poolCoins"`
	TotalTx   int    `json:"totalTx"`

	// ProvedCells and FailedCells describe the NEWEST generation only: how many
	// of its cells have a transition the network has accepted, and how many were
	// rejected. The remainder are still in flight.
	//
	// Neither says the cells AGREE. Nothing in Script compares one cell's row to
	// another's — each cell verifies only its own bit of the next row, which
	// cellscript.TestOtherCellsBitsAreNotChecked asserts deliberately — so
	// cross-cell agreement is an auditable invariant, not a proved one. This
	// used to be a single Consensus bool set to `failed == 0` and rendered as
	// "128/128 agree", which named the one property the covenant does not
	// establish.
	ProvedCells int `json:"provedCells"`
	FailedCells int `json:"failedCells"`

	ArcadeURL   string `json:"arcadeUrl"`
	GenesisTxID string `json:"genesisTxid"`
	LastError   string `json:"lastError,omitempty"`

	// Leader reports whether this instance holds the single-writer lease. Only
	// the leader advances cells; everyone else serves the UI read-only.
	Leader bool `json:"leader"`

	// HaltedCells is how many cells can never advance again.
	//
	// Distinct from FailedCells, which counts failures in the newest row only
	// and therefore reads zero the moment the window moves on. A halted cell is
	// permanent until its tip is recovered, so this is the number that says
	// whether the ring is being eroded.
	HaltedCells int `json:"haltedCells"`

	// Starved reports that the automaton has stopped for want of funding, with
	// the address to send coin to. It resumes unattended once coin arrives.
	Starved        bool   `json:"starved"`
	FundingAddress string `json:"fundingAddress,omitempty"`

	// Lag is how far the clock has run ahead of the slowest cell, and Depth the
	// deepest unconfirmed chain. Both are backpressure made visible: lag means
	// the cells cannot keep up with the requested rate, depth means mining
	// cannot keep up with the cells.
	Lag   uint64 `json:"lag"`
	Depth uint64 `json:"depth"`

	// WaitingOnCoin is how many cells are currently retrying a funding
	// shortfall.
	//
	// A transient shortfall is not an error and is not worth a log line, but it
	// must not therefore be invisible: a contended coin pool looks exactly like
	// a slow network from the outside, and we lost real time to that. If this
	// sits high, the fuel pool is the bottleneck, not the chain.
	WaitingOnCoin int `json:"waitingOnCoin"`
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
	funds     chain.Funds
	totalTx   int
	lastError string

	// target is the generation every cell is being asked to reach.
	//
	// This is what replaces the per-generation barrier. The clock raises it and
	// each cell chases it independently, so a slow cell falls behind instead of
	// holding all the others up, and the automaton's rate stops being set by its
	// worst cell. The clock refuses to run more than maxLag ahead of the slowest
	// cell, so "cannot keep up" shows as a bounded lag rather than a runaway.
	target uint64

	// lastMined[cell] is the newest generation of that cell known to be in a
	// block. tip − lastMined is that cell's unconfirmed chain depth, which the
	// worker bounds: a cell is an unbroken chain of unconfirmed transactions,
	// and past the node's mempool ancestor limit the deepest one is rejected and
	// the rejection cascades to every descendant.
	lastMined map[int]uint64

	// starvedSince is when funding ran out, or the zero time. lastProbe paces
	// the single retry that resumes the automaton once coin arrives.
	starvedSince time.Time
	lastProbe    time.Time
	// fundingAddress is shown to the operator while starved.
	fundingAddress string

	// dirty marks tip state that the checkpointer has not written yet.
	dirty bool

	// owner identifies this instance in the single-writer election, and leader
	// records whether it currently holds it. See holdLease.
	owner  string
	leader bool

	// waitingOnCoin holds the cells currently retrying a funding shortfall, so
	// coin contention is visible rather than looking like a slow network.
	waitingOnCoin map[int]bool

	// txIndex maps a broadcast txid back to the cell that produced it, so
	// arcade's status stream can be reflected in the diagram.
	txIndex map[string]txLoc
	// halted marks cells whose chain is broken (a rejected transaction), so we
	// stop trying to spend outputs that do not exist.
	halted map[int]bool

	// store is the durable record. Memory holds a window for the UI; the store
	// holds everything.
	store *history.Store

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
		chain:         c,
		compiled:      compiled,
		logger:        logger,
		state:         state,
		mode:          ModePaused,
		rate:          1,
		target:        state.Generation,
		lastMined:     make(map[int]uint64, state.Cells),
		changed:       make(chan struct{}),
		txIndex:       make(map[string]txLoc),
		halted:        make(map[int]bool),
		waitingOnCoin: make(map[int]bool),
		store:         store,
		owner:         instanceOwner(),
	}
	// The address is what an operator needs the moment funding runs out, so
	// resolve it now rather than when the automaton is already stopped.
	if target, err := chain.FundingAddress(c.Identity, c.Config.Network); err == nil {
		e.fundingAddress = target.Address
	} else {
		logger.Warn("could not derive the funding address", "err", err)
	}
	// Nothing is known to be mined until the status stream says so; starting at
	// the recorded generation means the depth gate opens rather than clamping
	// every cell shut on a fresh start.
	for i := range state.Cells {
		e.lastMined[i] = state.Chains[i].Generation
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

	// A write-ahead record left unresolved means the process died between
	// broadcasting a transition and recording it. That cell's real tip is
	// UNKNOWN — advancing it from the last tip we did record would re-spend an
	// output the network may already have consumed, and the resulting rejection
	// is indistinguishable from a genuine failure. It is exactly how cells 34
	// and 51 were lost.
	//
	// So stop the cell instead of guessing. This is deliberately conservative:
	// one stalled cell an operator can see beats a silent double spend, and
	// recovering the real tip from chain data is a separate job.
	uncertain, err := store.UnresolvedAttempts(ctx)
	if err != nil {
		return nil, err
	}
	for cell, generation := range uncertain {
		if cell < 0 || cell >= state.Cells {
			continue
		}
		e.halted[cell] = true
		e.logger.Warn("cell tip is unknown after an unclean shutdown; refusing to advance it",
			"cell", cell, "generation", generation,
			"detail", "a transition may have been broadcast without being recorded")
	}
	if len(uncertain) > 0 {
		e.lastError = fmt.Sprintf(
			"%d cell(s) stopped: a transition may have been broadcast but not recorded before shutdown",
			len(uncertain))
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

	// Count the newest row only. "Proved" means a node accepted the transition,
	// which is where script validation happens; TxBroadcast is arcade's receipt,
	// not the network's verdict, so it does not count yet.
	failed, proved := 0, 0
	if n := len(history); n > 0 {
		for _, c := range history[n-1].Cells {
			switch c.State {
			case TxFailed:
				failed++
			case TxSeen, TxMined:
				proved++
			}
		}
	}

	frontier := e.frontierLocked()
	return Snapshot{
		Cells:       e.state.Cells,
		Rule:        int(e.state.Rule),
		Mode:        e.mode,
		Rate:        e.rate,
		Generation:  frontier,
		History:     history,
		Balance:     e.funds.Spendable,
		Reserve:     e.funds.Reserve,
		PoolCoins:   e.funds.PoolCoins,
		TotalTx:     e.totalTx,
		ProvedCells: proved,
		FailedCells: failed,
		ArcadeURL:   e.chain.Config.ArcadeURL,
		GenesisTxID: e.state.GenesisTxID,
		LastError:   e.lastError,

		Starved:        e.mode == ModeStarved,
		FundingAddress: e.fundingAddress,
		Leader:         e.leader,
		HaltedCells:    len(e.halted),
		Lag:            e.target - min(e.target, frontier),
		Depth:          e.deepestLocked(),
		WaitingOnCoin:  len(e.waitingOnCoin),
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
//
// Resuming from starved clears the shortfall timer too, so an operator who has
// just sent coin does not have to wait out the grace period.
func (e *Engine) SetMode(m Mode) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if m == ModeRunning {
		e.starvedSince = time.Time{}
		if e.mode == ModeStarved {
			e.lastError = ""
		}
		// Never leave the clock behind the cells: a target stale from a previous
		// pause would make every worker idle until it caught up.
		if f := e.frontierLocked(); e.target < f {
			e.target = f
		}
	}
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

// Step asks every cell to advance exactly one generation.
//
// With no barrier there is nothing to trigger, only a target to raise: each cell
// chases it at its own pace and the step completes when the slowest one lands.
func (e *Engine) Step() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.target < e.frontierLocked()+1 {
		e.target = e.frontierLocked() + 1
	} else {
		e.target++
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

// trackBalance refreshes the reported balance periodically.
func (e *Engine) trackFunds(ctx context.Context) {
	// Five seconds, not ten: this is the number an operator watches after
	// sending a payment to a starved deployment, and it is also how they see the
	// pool draining before it empties. A slow reading here reads as "nothing is
	// happening".
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		funds, err := e.chain.Funds(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.logger.Warn("read funds", "err", err)
		} else {
			e.mu.Lock()
			changed := e.funds != funds
			e.funds = funds
			if changed {
				e.notify()
			}
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

// instanceOwner is this process's identity in the single-writer election.
//
// The hostname is the pod name under Kubernetes, which makes the lease row
// legible to an operator; the suffix keeps two processes on one host distinct.
func instanceOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}
