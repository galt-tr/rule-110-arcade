// Package engine drives the automaton: it computes generations, advances each
// cell's UTXO chain on the network, and exposes a snapshot for the UI.
//
// The row sequence is computed locally and immediately; the chain then PROVES
// each bit of it. That ordering is deliberate — it decouples the clock from
// confirmation latency, so the pattern draws at once and confirmation status
// washes down behind it rather than the display stalling on the network.
package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"sync/atomic"
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

	// UnseenDepth is how far the furthest cell has run ahead of what the network
	// has ACCEPTED. Non-zero means cells are waiting on the acceptance gate,
	// which is correct behaviour and looks exactly like a stall from outside —
	// hence the number. See Config.MaxUnseenDepth.
	UnseenDepth uint64 `json:"unseenDepth"`

	// WaitingOnCoin is how many cells are currently retrying a funding
	// shortfall.
	//
	// A transient shortfall is not an error and is not worth a log line, but it
	// must not therefore be invisible: a contended coin pool looks exactly like
	// a slow network from the outside, and we lost real time to that. If this
	// sits high, the fuel pool is the bottleneck, not the chain.
	WaitingOnCoin int `json:"waitingOnCoin"`

	// Bootstrap is present ONLY while a cold deployment is being brought up,
	// and absent once generation 0 exists. The engine never sets it — see
	// package boot, which serves this shape before there is an engine at all.
	//
	// It is a field on the snapshot rather than its own endpoint so the browser
	// keeps one payload, one fetch and one stream across the handover.
	Bootstrap *Bootstrap `json:"bootstrap,omitempty"`

	// Locked reports that the clock cannot be driven from outside: no play,
	// pause, step or rate change, for the life of the process.
	//
	// It is here so the browser can render the truth rather than offering
	// controls that will be refused. It is NOT how the refusal happens — the
	// engine's own setters and the HTTP handler each refuse independently,
	// because a control surface that is only hidden is not locked at all.
	Locked bool `json:"locked"`
}

// Bootstrap is how far a cold deployment has got towards existing.
//
// It carries the funding address and the shortfall because that is the only
// thing a visitor can act on: an automaton with no coin has nothing to show and
// exactly one thing to ask for.
type Bootstrap struct {
	// Phase is one of waiting, funding, fuel, genesis.
	Phase string `json:"phase"`
	// Address is where to send coin, and Network which chain it is on.
	Address string `json:"address"`
	Network string `json:"network"`
	// MinSatoshis is what must arrive before anything is spent; Have is what
	// has arrived.
	MinSatoshis uint64 `json:"minSatoshis"`
	Have        uint64 `json:"have"`
	// Err is the last failure, if the machine is retrying.
	Err string `json:"err,omitempty"`
}

// maxHistory bounds how many generations the UI keeps.
//
// Every generation holds one entry per cell, so this is a per-cell multiple:
// 1024 generations of a 256-cell ring is ~262,000 CellTx values held resident
// and copied by every full Snapshot. 2048 was chosen against a 128-cell ring and
// doubling the ring doubled the cost of the choice as well as the choice itself.
const maxHistory = 1024

// PublishInterval is the floor between tail publishes — the rate ceiling for
// pushed UI updates. A generation produces one notification per cell and the UI
// only needs the resulting state.
//
// 500ms against a clock locked at 0.5 gen/s gives roughly four frames per
// generation, which is enough to watch a row fill in without paying to
// re-marshal state that has not changed. It is a floor and not a delay, so an
// isolated change still appears at once — see PublishTails.
const PublishInterval = 500 * time.Millisecond

// TailGenerations is how many generations a streamed update carries.
//
// A snapshot holds every cell of every generation, so the full history grows
// without bound as a payload: a 256-cell generation carrying txids is ~27 KB,
// and re-sending all of it several times a second would reach megabytes per
// message well before the diagram got interesting. Streamed updates therefore
// carry only the recent tail and the client merges it into the history it
// already has; older generations are terminal and do not change.
//
// 8, not the 48 this held against a 128-cell ring, and the reason is not only
// the payload. A longer tail is often reached for to deliver late status
// changes, and it cannot: a mined verdict lands roughly rate × block-interval
// generations behind the frontier — hundreds, on this network — so no tail
// anyone would stream reaches it. The fix for that is pushing the changed set
// rather than a window. Raising this number buys bandwidth cost and no
// correctness.
const TailGenerations = 8

// Options are the choices a deployment makes about how the engine behaves that
// are not properties of the chain.
type Options struct {
	// AutoRecover lets the engine resolve a cell whose tip is unknown — or whose
	// tip a lost transition of ours already spent — by itself, on every
	// acquisition of the writer lease.
	//
	// It defaults OFF, and that is a considered position rather than caution for
	// its own sake. The asymmetry decides it: what recovery prevents is a halted
	// cell — visible, non-destructive, fixable by hand — while a bug in it runs
	// once per cell unattended immediately after a crash, which is precisely
	// when the deployment is least well understood. `rule110 recover` shows
	// exactly what it would do, against the real wallet, before anything is
	// allowed to do it on its own.
	AutoRecover bool

	// LockControls makes SetMode, SetRate and Step no-ops, fixing the clock at
	// whatever the process started with.
	//
	// The refusal is here, in the engine, and not only in the HTTP handler that
	// is the reason for wanting it. POST /api/control has no authentication, so
	// on a public deployment the handler must refuse — but a policy enforced
	// only at the edge is one refactor away from being enforced nowhere, and the
	// thing being protected is a live ring of UTXO chains. Both layers refuse;
	// neither relies on the other.
	//
	// Starvation is deliberately NOT covered. clearStarvation and the worker's
	// own transition into ModeStarved write the mode field directly, under the
	// lock, because those are the engine describing what happened to it rather
	// than an operator driving it.
	LockControls bool

	// Rate is the clock's starting speed in generations per second, and Start
	// runs it immediately instead of coming up paused.
	//
	// These are Options rather than a SetRate/SetMode pair at the call site
	// because LockControls turns those two into no-ops. Configuring a locked
	// engine by calling the mutators the lock exists to disable is a
	// contradiction that resolves silently and in the worst direction — a
	// public deployment that comes up permanently paused and cannot be started.
	// A zero Rate means the default of one generation per second.
	Rate  float64
	Start bool

	// PerfLog writes one line per completed generation with the current phase
	// quantiles, at most once a second. Nil is off, which is the default.
	//
	// Its own logger rather than a bool, because the process logger runs at Warn
	// and the obvious way to make this line visible — lowering that level —
	// turns on the toolbox's info logging too, which at several hundred
	// transactions a second is a second workload rather than an aid to
	// measuring the first.
	//
	// Off by default and worth keeping that way: the numbers are all on
	// /metrics, which is where a long run should be read from. This is for
	// watching a short experiment in a terminal without standing up a scraper.
	PerfLog *slog.Logger
}

// startMode is the clock's initial state.
//
// A locked engine always starts running: with the mutators disabled there is no
// later opportunity to start it, so coming up paused would be permanent.
func startMode(o Options) Mode {
	if o.Start || o.LockControls {
		return ModeRunning
	}
	return ModePaused
}

// startRate applies the same clamp SetRate does, so a rate that arrives through
// Options cannot reach somewhere a rate that arrives through the API could not.
func startRate(o Options) float64 {
	if o.Rate == 0 {
		return 1
	}
	return clampRate(o.Rate)
}

// Engine owns the automaton's state and its clock.
type Engine struct {
	chain    *chain.Chain
	compiled *cellscript.Compiled
	logger   *slog.Logger
	opts     Options

	// ledger and oracle are what recovery reads: the wallet's own record of what
	// it signed, and the network's verdict on a transaction. Both are the chain,
	// held as their interfaces rather than reached through it so a repair can be
	// tested against a ledger that lies — which is the only way to test that
	// recovery believes evidence rather than assumptions. See chain.Ledger.
	ledger chain.Ledger
	oracle chain.TxStatus

	mu sync.RWMutex

	// deployment is what genesis fixed: ring size, rule, seed, genesis txid.
	// Immutable, so it needs no lock discipline of its own — it is here rather
	// than passed around only because everything else in the engine wants it.
	deployment *chain.Deployment

	// tips[cell] is the output that cell will spend next.
	//
	// Derived at startup and on every re-acquisition of the writer lease, never
	// loaded from a file: the durable record of where a cell is is the history
	// store, and these are rebuilt from it plus the bytes of the transactions it
	// names. See DeriveTips.
	tips []chain.CellChain

	// generation is the frontier: the newest row every unhalted cell has proved.
	// Display state, derived from tips, and the position the automaton holds when
	// every cell is halted and there is no frontier left to compute.
	generation uint64

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

	// lastSeen[cell] is the newest generation of that cell the NETWORK HAS
	// ACCEPTED — seen, or mined, which implies seen. tip − lastSeen is how far
	// the cell has run ahead of what arcade will admit exists, which is the
	// gate that stops a child being submitted before its parent lands. See
	// Config.MaxUnseenDepth.
	lastSeen map[int]uint64

	// starvedSince is when funding ran out, or the zero time. lastProbe paces
	// the single retry that resumes the automaton once coin arrives.
	starvedSince time.Time
	lastProbe    time.Time
	// fundingAddress is shown to the operator while starved.
	fundingAddress string

	// owner identifies this instance in the single-writer election, and leader
	// records whether it currently holds it. See holdLease.
	owner  string
	leader bool

	// needsRederive gates every cell until the tips have been rebuilt under the
	// lease we currently hold.
	//
	// holdLease can flip leader back to true after a store error, and it cannot
	// tell "the database blinked" from "the lease genuinely expired and someone
	// else advanced all 128 chains". Resuming from in-memory tips in the second
	// case double-spends every cell at once. So a fresh acquisition is treated
	// as the second case unconditionally: nothing advances until the tips have
	// been re-derived from the store, which is cheap and bounded. See rederive.
	needsRederive bool

	// waitingOnCoin holds the cells currently retrying a funding shortfall, so
	// coin contention is visible rather than looking like a slow network.
	waitingOnCoin map[int]bool

	// txIndex maps a broadcast txid back to the cell that produced it, so
	// arcade's status stream can be reflected in the diagram.
	txIndex map[string]txLoc
	// halted marks cells whose chain is broken, and haltReason says why.
	//
	// Reconstructed from the store at startup, not merely accumulated at
	// runtime. That is bug 9a: a rejection used to set this in memory only,
	// while the tip had already been advanced to the rejected transaction's
	// output, so a restart resumed the cell against an output that never
	// existed and it spent a phantom forever.
	halted     map[int]bool
	haltReason map[int]string

	// needsRepair marks cells whose worker must repair them before its next
	// turn. A rejection sets it; repairCell clears it.
	//
	// The repair runs on the cell's own worker rather than where the rejection
	// arrives, for two reasons. The rejection arrives on the monitor's applier
	// goroutine, which must never block. And the worker is the only goroutine
	// that can guarantee no transition for this cell is in flight — repairing
	// while advanceCell holds a half-built generation would rewind the tip under
	// it.
	needsRepair map[int]bool

	// retries counts consecutive refusals of the SAME generation, per cell, so a
	// transition that cannot be made to work halts the cell instead of rebuilding
	// for ever. Cleared when the cell advances past the generation it counts.
	retries map[int]retryState

	// store is the durable record. Memory holds a window for the UI; the store
	// holds everything.
	store *history.Store

	// changed is closed and replaced whenever the snapshot changes, so
	// subscribers can wait without polling.
	changed chan struct{}

	// statusWrites carries applied status updates to the batching writer. It
	// exists so that applying a status never waits on the database: the apply
	// runs on the monitor's applier goroutine, and a database round trip there
	// stalls the SSE reader for the whole process. Drained by writeStatuses.
	statusWrites chan history.StatusUpdate

	// persistQueue carries durable cell writes to the single committer, and
	// persistStopped is closed when that committer exits so a caller can tell
	// "shutting down" from "the write failed".
	//
	// Unlike statusWrites, every sender here is BLOCKED on the result: this is a
	// group commit, not a hand-off. See Engine.persist.
	persistQueue   chan persistRequest
	persistStopped chan struct{}

	// published is the most recent tail snapshot, already marshalled. See
	// PublishTail.
	published atomic.Pointer[[]byte]

	// perf holds every latency instrument. Its own instruments are atomic, so
	// only raisedAt below needs the engine lock.
	perf *perf

	// raisedAt[g] is when generation g was first asked for, which is where the
	// generation-duration histogram measures from. Guarded by mu; entries are
	// deleted as the frontier passes them. See noteTargetRaisedLocked.
	raisedAt map[uint64]time.Time

	// perfLogMu paces the per-generation log line. Its own lock rather than mu,
	// because it is taken after the engine lock is released and adding a second
	// reason to hold mu on this path would defeat the point of measuring it.
	perfLogMu   sync.Mutex
	lastPerfLog time.Time
}

// PublishedTail returns the most recently published tail snapshot as JSON, and
// whether there is one yet.
//
// The bytes are shared and must not be modified. Every SSE subscriber writes the
// same slice, so the cost of a push is one marshal for the process rather than
// one per client — and the marshal happens on the publisher's goroutine, off
// every request path. This is the one idea worth taking from the reference
// application, which feels instant for exactly this reason: what it serves is
// always a value computed somewhere else.
//
// A POINTER, so a caller can tell "the same tail I already have" from "a new
// one" without comparing bytes. PublishTail stores a fresh pointer on each
// rebuild and never mutates a slice it has published, which makes pointer
// identity exact. Subscribers need this because they are woken by notify(),
// which fires far more often than the tail is rebuilt.
func (e *Engine) PublishedTail() (*[]byte, bool) {
	p := e.published.Load()
	if p == nil {
		return nil, false
	}
	return p, true
}

// PublishTail rebuilds and marshals the tail snapshot, making it the value
// PublishedTail returns. It reports whether the snapshot could be marshalled.
func (e *Engine) PublishTail() bool {
	data, err := json.Marshal(e.SnapshotTail())
	if err != nil {
		e.logger.Error("marshal tail snapshot", "err", err)
		return false
	}
	e.published.Store(&data)
	return true
}

// PublishTails keeps the published tail snapshot current until ctx is done.
//
// Coalescing lives here rather than in each subscriber: one publisher doing the
// work once beats N connections each rebuilding and re-marshalling the same
// state.
//
// minInterval is a floor between publishes, not a delay before one. The
// difference is the whole point: a generation of 128 cells settling produces 128
// notifications and the UI needs only the result, but an isolated change — a
// pause, a rate change, one cell mined on a quiet ring — should appear at once.
// Sleeping first would tax every update to coalesce the few that need it.
func (e *Engine) PublishTails(ctx context.Context, minInterval time.Duration) {
	var last time.Time
	for {
		// Subscribe BEFORE publishing, so a change landing during the publish
		// wakes the next iteration instead of being stranded until some later
		// one. Held across the throttle below for the same reason.
		changed := e.Changed()

		if wait := minInterval - time.Since(last); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}

		e.PublishTail()
		last = time.Now()

		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
	}
}

// New creates an engine positioned where the history store and the chain say
// the cells actually are.
//
// Nothing here is loaded from a mutable file. Every tip is derived and verified
// against the covenant script the transaction really carries, and the halted set
// is reconstructed from the same records — so a cell halted by a rejection stays
// halted across a restart, which it did not before. See DeriveTips.
func New(ctx context.Context, c *chain.Chain, compiled *cellscript.Compiled, d *chain.Deployment,
	store *history.Store, opts Options, logger *slog.Logger) (*Engine, error) {

	seed, err := d.Seed()
	if err != nil {
		return nil, err
	}
	e := &Engine{
		chain:          c,
		ledger:         c,
		oracle:         c.Oracle,
		compiled:       compiled,
		logger:         logger,
		opts:           opts,
		deployment:     d,
		tips:           make([]chain.CellChain, d.Cells),
		mode:           startMode(opts),
		rate:           startRate(opts),
		lastMined:      make(map[int]uint64, d.Cells),
		lastSeen:       make(map[int]uint64, d.Cells),
		changed:        make(chan struct{}),
		txIndex:        make(map[string]txLoc),
		halted:         make(map[int]bool),
		haltReason:     make(map[int]string),
		waitingOnCoin:  make(map[int]bool),
		needsRepair:    make(map[int]bool),
		retries:        make(map[int]retryState),
		statusWrites:   make(chan history.StatusUpdate, statusWriteQueue),
		persistQueue:   make(chan persistRequest, persistQueueSize),
		persistStopped: make(chan struct{}),
		store:          store,
		owner:          InstanceOwner(),
		perf:           newPerf(),
		raisedAt:       make(map[uint64]time.Time),
		// Nothing may advance until the tips have been re-derived while this
		// instance actually holds the writer lease. They are derived here too,
		// so the UI has something true to show immediately, but between here and
		// the first acquisition another writer may still own these chains.
		needsRederive: true,
	}
	// The address is what an operator needs the moment funding runs out, so
	// resolve it now rather than when the automaton is already stopped.
	if target, err := chain.FundingAddress(c.Identity, c.Config.Network); err == nil {
		e.fundingAddress = target.Address
	} else {
		logger.Warn("could not derive the funding address", "err", err)
	}

	// Seed generation 0 before deriving, so a brand-new deployment has the row
	// the derivation cross-check compares against.
	if err := seedGenesisRecord(ctx, store, d, seed); err != nil {
		return nil, err
	}

	positions, err := DeriveTips(ctx, c, compiled, d, store)
	if err != nil {
		return nil, err
	}
	if err := CheckMigrationFloor(ctx, store, c.Oracle, positions, d.LegacyTips()); err != nil {
		return nil, err
	}
	e.applyPositions(positions)

	loaded, err := loadHistory(ctx, store, d.Cells, e.generation)
	if err != nil {
		return nil, err
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
			// Zero time: broadcast by an earlier process, so the status lag for
			// these is unknowable and deliberately not measured.
			e.indexTx(c.TxID, c.Generation, c.Cell, time.Time{})
		}
	}

	e.logger.Info("tips derived", "generations", len(e.history), "unsettled", len(unsettled),
		"frontier", e.generation, "halted", len(e.halted))
	for cell, why := range e.haltReason {
		e.logger.Warn("cell halted", "cell", cell, "reason", why)
	}
	return e, nil
}

// applyPositions installs a freshly derived set of tips. Callers must not hold
// the lock; New runs before anything else can, and rederive takes it here.
func (e *Engine) applyPositions(positions []CellPosition) {
	e.mu.Lock()
	defer e.mu.Unlock()

	unknown := 0
	e.halted = make(map[int]bool, len(positions))
	e.haltReason = make(map[int]string, len(positions))
	for cell, p := range positions {
		e.tips[cell] = p.Tip
		if p.Halted {
			e.halted[cell] = true
			e.haltReason[cell] = p.HaltReason
		}
		if p.Unknown {
			unknown++
		}
		// Nothing is known to be mined until the status stream says so; starting
		// at the derived generation means the depth gate opens rather than
		// clamping every cell shut on a fresh start.
		e.lastMined[cell] = p.Tip.Generation
		// A DERIVED tip is a transaction that exists on chain, so it is at least
		// seen. Seeding this is what stops every cell stalling on the seen gate
		// at startup, waiting for a status the stream will never resend.
		e.lastSeen[cell] = p.Tip.Generation
	}

	e.generation = e.frontierLocked()
	// Never leave the clock behind the cells, or every worker idles until it
	// catches up.
	if e.target < e.generation {
		e.target = e.generation
	}

	// Set unconditionally, including the empty case: this is a fresh assessment
	// of every cell, so a message left over from the previous one would report a
	// problem that recovery has just resolved.
	switch {
	case unknown > 0:
		e.lastError = fmt.Sprintf(
			"%d cell(s) stopped: a transition may have been broadcast but not recorded before shutdown "+
				"(run \"rule110 recover\")", unknown)
	case len(e.halted) > 0:
		e.lastError = fmt.Sprintf("%d cell(s) halted", len(e.halted))
	default:
		e.lastError = ""
	}
	e.notify()
}

// seedGenesisRecord writes generation 0 if the store has never recorded it.
//
// Generation 0's transactions are not something the engine ever broadcasts —
// they were all created by the single genesis transaction — so nothing else
// would ever put them in the store, and the diagram would start blank.
func seedGenesisRecord(ctx context.Context, store *history.Store, d *chain.Deployment, seed ca.Row) error {
	if _, ok, err := store.RowAt(ctx, 0); err != nil || ok {
		return err
	}
	if err := store.RecordGeneration(ctx, 0, seed.Hex()); err != nil {
		return err
	}
	// One statement rather than one per cell. This runs before the HTTP server
	// comes up, so its cost is startup latency an operator waits through.
	rows := make([]history.CellTx, d.Cells)
	for cell := range d.Cells {
		rows[cell] = history.CellTx{
			Generation: 0, Cell: cell, TxID: d.GenesisTxID, Status: history.StatusSeen,
		}
	}
	return store.RecordTxBatch(ctx, rows)
}

// loadHistory restores the recorded history: real rows with the real
// transactions that proved them.
//
// Rows could be recomputed from the seed, but the transaction ids could not —
// they exist only in the store, and they are the evidence the automaton ran on
// chain at all. Recomputing rows and discarding txids was the wrong trade.
func loadHistory(ctx context.Context, store *history.Store, cellCount int, frontier uint64) ([]Generation, error) {
	from := uint64(0)
	if frontier > maxHistory {
		from = frontier - maxHistory
	}
	recorded, err := store.Load(ctx, from, maxHistory+1)
	if err != nil {
		return nil, err
	}

	out := make([]Generation, 0, len(recorded))
	for _, g := range recorded {
		cells := make([]CellTx, cellCount)
		for i := range cells {
			cells[i] = CellTx{Cell: i, State: TxPending}
		}
		for _, c := range g.Cells {
			if c.Cell < 0 || c.Cell >= len(cells) {
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
//
// It builds only the tail. This used to call Snapshot and then slice the result,
// which copied all of maxHistory — 2048 generations of 128 cells, a quarter of a
// million structs under the read lock — to emit 48 of them, on every push, for
// every connected client. A pending writer blocks behind that copy.
func (e *Engine) SnapshotTail() Snapshot {
	return e.snapshot(TailGenerations)
}

// Snapshot returns the current view, with the full in-memory history. Safe for
// concurrent use.
func (e *Engine) Snapshot() Snapshot { return e.snapshot(0) }

// Stats returns the scalar view with no history at all.
//
// For callers that never look at the diagram — /metrics is scraped on a timer
// whether or not a browser is connected, and paying a full history copy for a
// dozen gauges is pure waste.
func (e *Engine) Stats() Snapshot { return e.snapshot(-1) }

// snapshot builds the view, copying at most limit trailing generations: 0 means
// all of them, negative means none.
func (e *Engine) snapshot(limit int) Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()

	from := 0
	if limit > 0 && len(e.history) > limit {
		from = len(e.history) - limit
	} else if limit < 0 {
		from = len(e.history)
	}

	// Copy the CELLS, not just the generation headers. Generation.Cells is a
	// slice, so copying the outer slice alone leaves every backing array shared
	// with the live engine — and the caller marshals it after this lock is
	// released, while recordCell and applyStatusBatch are writing into those
	// same arrays. That is a genuine data race on a string header, not a stale
	// read.
	history := make([]Generation, 0, len(e.history)-from)
	for _, g := range e.history[from:] {
		cells := make([]CellTx, len(g.Cells))
		copy(cells, g.Cells)
		g.Cells = cells
		history = append(history, g)
	}

	// Count the newest row only. "Proved" means a node accepted the transition,
	// which is where script validation happens; TxBroadcast is arcade's receipt,
	// not the network's verdict, so it does not count yet.
	//
	// Read from e.history rather than the copy, so the counts are right even
	// when no history was copied at all.
	failed, proved := 0, 0
	if n := len(e.history); n > 0 {
		for _, c := range e.history[n-1].Cells {
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
		Cells:       e.deployment.Cells,
		Rule:        int(e.deployment.Rule),
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
		GenesisTxID: e.deployment.GenesisTxID,
		LastError:   e.lastError,

		Starved:        e.mode == ModeStarved,
		FundingAddress: e.fundingAddress,
		Leader:         e.leader,
		Locked:         e.opts.LockControls,
		HaltedCells:    len(e.halted),
		Lag:            e.target - min(e.target, frontier),
		Depth:          e.deepestLocked(),
		UnseenDepth:    e.deepestUnseenLocked(),
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
	if e.opts.LockControls {
		return
	}
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

// clampRate bounds a requested rate to something a chain can serve. Shared by
// SetRate and the Options path so there is one answer to what a rate may be.
func clampRate(r float64) float64 { return min(max(r, 0.05), 20) }

// arcadeEventBudget is roughly how many status events a second arcade's SSE
// fan-out has been measured to sustain, and fullStatusMultiplier is how many
// events a transaction produces when every transition is subscribed to rather
// than only the terminal one.
//
// Both are observations of a particular deployment rather than anything arcade
// promises, which is why exceeding them warns instead of clamping. A number
// this soft has no business silently overriding what an operator asked for.
const (
	arcadeEventBudget    = 1500
	fullStatusMultiplier = 4
	terminalStatusPerTx  = 1
)

// SetRate sets generations per second, clamped to something a chain can serve.
func (e *Engine) SetRate(r float64) {
	if e.opts.LockControls {
		return
	}
	e.mu.Lock()
	rate := clampRate(r)
	e.rate = rate
	cells := e.deployment.Cells
	full := e.chain.Config.FullStatusUpdates
	e.notify()
	e.mu.Unlock()

	// Warn about the event budget rather than the transaction rate, because the
	// two fail differently and only this one is invisible. Outrunning the chain
	// shows up as lag, which the UI already reports; outrunning arcade's SSE
	// fan-out makes it drop events for a slow client, and a dropped event is a
	// status that never arrives — the diagram simply stops being true, with
	// nothing anywhere saying so until the reconciler catches it 90 seconds
	// later.
	perTx := terminalStatusPerTx
	if full {
		perTx = fullStatusMultiplier
	}
	if events := rate * float64(cells*perTx); events > arcadeEventBudget && full {
		e.logger.Warn("rate exceeds the measured arcade event budget; statuses will lag or be dropped",
			"rate", rate, "events_per_second", int(events), "budget", arcadeEventBudget,
			"remedy", "restart with -full-status=false, which drops this to one event per transaction")
	}
}

// Step asks every cell to advance exactly one generation.
//
// With no barrier there is nothing to trigger, only a target to raise: each cell
// chases it at its own pace and the step completes when the slowest one lands.
func (e *Engine) Step() {
	if e.opts.LockControls {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.target < e.frontierLocked()+1 {
		e.target = e.frontierLocked() + 1
	} else {
		e.target++
	}
	e.noteTargetRaisedLocked(e.target)
	e.notify()
}

// indexOfGeneration finds a generation by number, or -1.
//
// A binary search rather than the backward scan this used to be. The scan was
// written for the caller that looks for the NEWEST generation, where it stops
// after one comparison — but applyStatusBatch calls this once per status record
// while holding the write lock, and a status for an old generation walks the
// whole window. With up to maxHistory entries and every late or re-delivered
// status paying for it, that put an unbounded scan in the hottest critical
// section in the program to save nothing at all: log2(2048) is eleven
// comparisons, and the newest-generation case is only one of them slower.
//
// The slice is kept ascending by ensureGeneration, and may have gaps where a
// generation was never created; BinarySearchFunc handles both.
func indexOfGeneration(gens []Generation, number uint64) int {
	i, found := slices.BinarySearchFunc(gens, number, func(g Generation, n uint64) int {
		return cmp.Compare(g.Number, n)
	})
	if !found {
		return -1
	}
	return i
}

// trackFunds refreshes the reported balance periodically.
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

// persistRetries is how many times a durable write is attempted before the
// cells in it are halted, persistBackoff the pause between attempts, and
// persistTimeout the bound on one attempt. See commitBatch.
const (
	persistRetries = 3
	persistBackoff = 50 * time.Millisecond

	// persistTimeout replaces the context.Background() this used to run under.
	// A store that accepts the connection and then never answers would
	// otherwise block a cell worker for ever, holding a slot in the batch and
	// never halting the cell that is stuck — the one failure this whole path is
	// built to make visible.
	persistTimeout = 15 * time.Second
)

// haltCell stops one cell and says why, once.
func (e *Engine) haltCell(cell int, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cell < 0 || cell >= len(e.tips) || e.halted[cell] {
		return
	}
	e.halted[cell] = true
	e.haltReason[cell] = reason
	e.lastError = fmt.Sprintf("cell %d halted: %s", cell, reason)
	e.notify()
}

// InstanceOwner is this process's identity in the single-writer election.
//
// The hostname is the pod name under Kubernetes, which makes the lease row
// legible to an operator; the suffix keeps two processes on one host distinct.
//
// Exported because the cold start takes the SAME lease before the engine
// exists — see package boot. It has to be the same string: a bootstrapper that
// claimed under a different owner would hold the lease against the engine that
// follows it in the same process, and the engine would sit out the full TTL
// waiting for itself.
func InstanceOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}
