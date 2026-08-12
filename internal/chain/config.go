// Package chain wires the arcade toolbox wallet for the automaton: it holds
// the cell UTXOs, funds their transitions from a denominated fuel pool, and
// broadcasts through arcade.
package chain

import (
	"fmt"
	"math"
	"net/url"
	"strings"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/defs"

	"github.com/dymurray/rule-110-arcade/internal/ca"
)

// Config describes everything the automaton needs to reach a chain.
//
// ArcadeURL is deliberately the only endpoint that must be set: arcade is
// network-agnostic and the toolbox derives its companion services from it.
type Config struct {
	// ArcadeURL is the arcade instance to broadcast through.
	ArcadeURL string

	// ChainTracksURL overrides the headers service. Empty means "derive it from
	// ArcadeURL", which is what a standard arcade deployment serves.
	ChainTracksURL string

	// EventsURL overrides the status stream. Empty means "the same host as
	// ArcadeURL", which is what a standard arcade deployment serves.
	//
	// Worth overriding for two reasons. Arcade serves its SSE from a separate
	// process behind the same hostname, so an ingress that does not route
	// /events to it answers 404 and this deployment then learns nothing about
	// its own transactions — they sit at "broadcast" in the diagram forever
	// while the once-a-minute proof poll quietly does the work instead. And
	// inside a cluster the stream is better taken from the sse Service
	// directly: it is the highest-volume connection this process holds, and
	// routing it through an ingress and a CDN adds two hops and a buffering
	// layer to a stream whose whole value is being prompt.
	EventsURL string

	// Network is the BSV network the arcade instance is attached to.
	Network defs.BSVNetwork

	// DataDir holds the wallet database and the key file. There is no
	// restore-from-seed in an arcade-only wallet, so this directory IS the
	// wallet: losing it loses the coins.
	DataDir string

	// Originator identifies this application to the wallet. Must be FQDN-shaped.
	Originator string

	// Cells is the ring size; Rule is the automaton rule to enforce.
	Cells int
	Rule  ca.Rule

	// CellSatoshis is the value each cell UTXO carries. It stays constant
	// across generations: fees come from the fuel pool, not from the cells.
	CellSatoshis uint64

	// PostgresDSN switches storage from SQLite to PostgreSQL.
	//
	// This is the single biggest throughput lever. SQLite serialises every
	// write, so a whole generation advancing at once thrashes a single writer
	// lock; the toolbox's own benchmarks put SQLite at ~57-108 TPS against ~575
	// for PostgreSQL. Empty keeps SQLite, which is fine for a small ring and is
	// not fine for the 256-cell default.
	PostgresDSN string

	// MaxDBConns bounds the storage connection pool. The benchmarks pair it
	// with worker count (conns ~ workers + margin).
	//
	// It sizes ONE pool, not two: the toolbox shares a single connection pool
	// between the metastore and the utxostore, so this is the whole of the
	// wallet's claim on the server.
	MaxDBConns int

	// HistoryDBConns bounds the history store's pool, separately from the
	// wallet's. 0 takes the history package's own default.
	//
	// Separate because they are separate pools against the same server and the
	// sum is what matters: exceeding max_connections does not slow this
	// application down, it halts cells, because a cell whose write-ahead record
	// cannot be written must not advance. Budget both against the server's
	// limit and leave room for whatever else lives there.
	HistoryDBConns int

	// MaxUnconfirmedDepth bounds how far ahead of its newest mined transaction
	// a cell may run. It DEFAULTS TO 0, which means no bound, and that is the
	// correct setting for teranode.
	//
	// A cell is an unbroken chain of unconfirmed transactions, so its depth
	// grows at the generation rate and only falls when a block lands. This
	// field once defaulted to a finite number on the theory of a mempool
	// ancestor limit: past it the deepest transaction is rejected and the
	// rejection cascades to every descendant, destroying a cell rather than
	// costing a step.
	//
	// THAT LIMIT DOES NOT EXIST HERE. Teranode does not enforce ancestor
	// tracking; arcade's LimitAncestorCount is a dead value with no setter; and
	// `rule110 depth-probe` built 600 consecutive transactions on a single chain
	// with zero rejections. The cascade recorded in the toolbox benchmarks had
	// another cause. Keeping a finite default was caution about a boundary
	// nobody could find, and it had a real price: the gate in engine.turnReady
	// caps the sustained generation rate at depth ÷ block interval, so a
	// value of 200 against this network's ~755 s blocks silently capped the
	// automaton at ~0.27 gen/s no matter what rate was configured.
	//
	// What unbounded depth actually costs is the size of the in-flight set —
	// cells × rate × block interval transactions that the partial index
	// cell_txs_inflight, the status pipeline and Unsettled() at startup all
	// have to carry. That is a sizing question, not a rejection risk. Set a
	// bound only for a network you have measured and found one on.
	MaxUnconfirmedDepth uint64

	// MaxUnseenDepth bounds how far a cell may run ahead of its newest
	// transaction the network has ACCEPTED. It defaults to 1, which means a cell
	// may not build generation N+1 until generation N is seen. 0 disables it.
	//
	// This is a different question from MaxUnconfirmedDepth, and confusing the
	// two is what cost a whole ring. That one asks whether a transaction has been
	// CONFIRMED — mined into a block — and is a margin against a mempool ancestor
	// limit teranode does not enforce. This one asks whether the network has so
	// much as HEARD of it.
	//
	// The distinction is that arcade's 202 means "accepted for processing", not
	// "validated" and not "in a mempool". Advancing on the 202 submits a child
	// that spends an output the network has not yet learned about, and it is
	// refused for an input that does not exist — not because anything is wrong
	// with it, but because it arrived first. Measured on the live deployment:
	// children rejected 0.9 to 21.7 seconds BEFORE their parents were accepted,
	// every one of those parents valid and seen today. All 256 cells were lost
	// that way in under two minutes.
	//
	// The toolbox already enforces exactly this rule for every coin it manages —
	// change is held at TierSending until arcade reports SEEN, see storage's
	// process.go — and the cell's continuation output is the one output in this
	// system that is not a wallet-managed coin, so it needs the rule applied by
	// hand.
	//
	// The cost is that the configured rate becomes advisory: when the network is
	// slow to accept, cells wait and the reported lag climbs. That is the
	// backpressure whose absence was the whole problem.
	MaxUnseenDepth uint64

	// MaxLag bounds how far the clock may run ahead of the slowest cell before
	// it stops asking for new generations. Without it, a rate the chain cannot
	// serve would queue without limit and the reported rate would be fiction.
	MaxLag uint64

	// FeeSatPerKB prices transactions, above arcade's 100 sat/kB floor.
	//
	// Not because the floor is applied to a larger size than the toolbox prices
	// — it is not. The extended format carries each prevout inline, but the
	// validator takes that as separate spent-coin data and does not bill it, and
	// the node TRUNCATES the required fee where the toolbox rounds up. Pricing
	// at exactly 100 is therefore safe on its own terms. (An earlier version of
	// this comment claimed the opposite; it was wrong.)
	//
	// The margin is for a different hazard: the fee is committed from a SIZE
	// ESTIMATE made before any unlocking script exists, and every script that
	// comes out longer than estimated eats into it. Ours are ~2.6 kB of
	// covenant, so a small error is a large number of bytes. MinBroadcastFeeRate
	// is the check that catches the estimate being wrong; this is the headroom
	// that stops it triggering.
	FeeSatPerKB int64

	// MinBroadcastFeeRate is the floor a finished transaction must clear locally
	// before it is allowed to exist, or 0 to skip the check.
	//
	// This measures the real transaction rather than the plan — fee from inputs
	// minus outputs, size from the bytes that will actually be broadcast — so it
	// catches a fee committed against a bad estimate, which is the one way a
	// correctly configured wallet still emits an underpriced transaction. Set it
	// to the floor the receiving arcade enforces, not to your own target rate:
	// it is the network's policy, not ours.
	MinBroadcastFeeRate int64

	// FuelDenomination is the value of one fuel coin, and FuelPoolSize how many
	// the keeper maintains.
	//
	// One coin funds exactly one cell transition, which is what makes every coin
	// in the pool interchangeable and lets the funder's ClaimExact claim without
	// ever colliding. It must comfortably clear the fee plus the dust floor, or
	// the change output vanishes and the covenant cannot spend the result.
	FuelDenomination uint64
	FuelPoolSize     uint64

	// Throughput selects the denominated fuel pool over the default privacy
	// strategy. Off means every cell competes for the same change set.
	Throughput bool

	// FullStatusUpdates asks arcade for every status transition rather than
	// terminal ones only. Good for a diagram, roughly 4x the event volume, and
	// arcade's SSE fan-out is measured at ~1,500-1,700 events/s.
	FullStatusUpdates bool

	// ApplyConcurrency is how many workers the monitor uses to apply arcade
	// status batches. The toolbox default of 8 is documented as too low for a
	// sustained high rate: when the appliers cannot drain the hand-off queue the
	// SSE reader blocks and arcade drops events, which is how transactions end
	// up with no status at all.
	ApplyConcurrency int

	// Chronicle selects Chronicle-era script rules for local pre-broadcast
	// verification. Rúnar covenants contain OP_2MUL and cannot verify without
	// it, so it defaults on; turn it off only against a Genesis-rules network,
	// where these transactions will be rejected anyway.
	Chronicle bool

	// LockControls refuses every play/pause/step/rate request, fixing the
	// automaton at the configured rate for the life of the process.
	//
	// This exists because POST /api/control has no authentication and a public
	// deployment puts it one HTTP request from anyone. Hiding the buttons in the
	// browser is not a lock — the endpoint is still reachable — so the refusal
	// lives in the handler and the UI merely reflects it. There is deliberately
	// no override header or token: an escape hatch on an unauthenticated
	// endpoint is just the endpoint again, and changing the rate of a public
	// exhibit is worth a redeploy.
	LockControls bool

	// PublicFunding exposes /api/funding and /api/fund so anyone with a BRC-100
	// wallet can pay this deployment's costs.
	//
	// Off by default: it is the only endpoint that makes an anonymous caller
	// commission wallet and network work, and a private deployment should not
	// carry that surface for nothing.
	PublicFunding bool

	// MinPaymentSatoshis is the smallest public payment worth accepting.
	//
	// Below one fuel coin plus its fees a payment cannot buy a single cell
	// transition, so accepting it costs a database row and a broadcast to move
	// the automaton not at all. Filled in from FuelDenomination when unset.
	MinPaymentSatoshis uint64

	// FirstFuelCoins is how many fuel coins the cold-start bootstrap mints
	// before creating generation 0.
	//
	// Enough that the automaton visibly RUNS the moment genesis lands, rather
	// than coming up correct and immediately starved while the keeper catches
	// up. Filled in from Cells when unset.
	FirstFuelCoins uint64
}

// DefaultConfig returns a configuration with everything but ArcadeURL set to a
// sensible default.
func DefaultConfig() Config {
	return Config{
		// Default to the private scaling test net, not mainnet: this is a
		// stress-test tool and defaulting to real money would be wrong.
		Network:      defs.NetworkTSTN,
		DataDir:      "./data",
		Originator:   "rule110.arcade.local",
		Cells:        256,
		Rule:         ca.Rule110,
		CellSatoshis: 1,
		Chronicle:    true,
		// One shared pool serves both the metastore and the utxostore, and the
		// toolbox sizes it as workers plus margin. There is one worker per cell
		// and they burst-sign together at the start of a generation.
		//
		// It is also the pool the monitor's status appliers draw from, with no
		// priority and no deadline, so starving it does not merely slow the
		// automaton down — it stops the diagram tracking the chain. Measured:
		// raising this from 72 to 200 improved status lag eightfold at
		// unchanged throughput.
		MaxDBConns: 144,
		// Unbounded. See MaxUnconfirmedDepth: teranode enforces no ancestor
		// limit, and a finite value here silently caps the generation rate at
		// depth ÷ block interval.
		MaxUnconfirmedDepth: 0,
		// A cell waits for its own parent to be accepted before building on it.
		MaxUnseenDepth:      1,
		MaxLag:              32,
		FeeSatPerKB:         125,
		MinBroadcastFeeRate: 100,
		// A cell transition is ~4.1 kB, so ~512 satoshis of fee at 125 sat/kB.
		// 1000 leaves comfortable change above the 48 satoshi dust floor (see
		// dustFloor) while stranding little value per coin.
		FuelDenomination: 1000,
		// The keeper refills from 60% of target, so the usable swing is 40% of
		// this number and it has to cover the keeper's own measure-and-mint
		// latency. At 256 cells and 0.5 gen/s the ring burns 128 coins a second,
		// so 20,000 bought 62 seconds of swing and 60,000 buys 187.
		FuelPoolSize:      60000,
		Throughput:        true,
		FullStatusUpdates: true,
		// ~512 status events a second at 128 tx/s with full updates. The toolbox
		// default of 8 is documented as too low; 32 is adequate only if every
		// apply is fast, and an applier that cannot drain the hand-off queue
		// blocks the SSE reader and makes arcade drop events for a slow client.
		ApplyConcurrency: 48,
	}
}

// Validate checks the configuration and normalises ArcadeURL.
func (c *Config) Validate() error {
	if c.ArcadeURL == "" {
		return fmt.Errorf("chain: arcade URL is required")
	}
	u, err := url.Parse(c.ArcadeURL)
	if err != nil {
		return fmt.Errorf("chain: parse arcade URL %q: %w", c.ArcadeURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("chain: arcade URL must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("chain: arcade URL %q has no host", c.ArcadeURL)
	}
	// The toolbox builds paths as "{URL}/tx", so a trailing slash would produce
	// a double slash.
	c.ArcadeURL = strings.TrimRight(c.ArcadeURL, "/")
	c.ChainTracksURL = strings.TrimRight(c.ChainTracksURL, "/")
	c.EventsURL = strings.TrimRight(c.EventsURL, "/")

	if c.Cells <= 0 || c.Cells%8 != 0 {
		return fmt.Errorf("chain: cells must be a positive multiple of 8, got %d", c.Cells)
	}
	if c.CellSatoshis == 0 {
		return fmt.Errorf("chain: cell satoshis must be positive")
	}
	// A zero MaxLag would stop the clock outright, so fill it in rather than
	// leaving an unset field to look like a hang. MaxUnconfirmedDepth is left
	// alone: zero legitimately means "no depth limit".
	if c.MaxLag == 0 {
		c.MaxLag = DefaultConfig().MaxLag
	}
	// Both of these are caught here because both fail a long way from their
	// cause. A zero fee rate prices every transaction at nothing, and nothing
	// local objects: utxoManagement's denomination check compares against a
	// marginal input fee that is also zero, so the configuration validates and
	// the first rejection arrives from arcade, per transaction, as an
	// insufficient-fee error nobody would trace back to a flag.
	if c.FeeSatPerKB <= 0 {
		return fmt.Errorf("chain: fee rate must be positive, got %d sat/kB "+
			"(arcade's floor is 100; see Config.FeeSatPerKB for why we price above it)", c.FeeSatPerKB)
	}
	// Apply concurrency fails the other way — silently. monitor's
	// WithApplyConcurrency IGNORES a non-positive value, so asking for zero
	// leaves the toolbox default of 8 running: the very setting this field
	// exists to raise, and whose failure is arcade dropping status events for a
	// slow client rather than anything that looks like a misconfiguration.
	if c.ApplyConcurrency <= 0 {
		return fmt.Errorf("chain: apply concurrency must be positive, got %d "+
			"(a non-positive value is ignored and silently leaves the toolbox default of 8)",
			c.ApplyConcurrency)
	}
	// Both of these are derived from numbers that are themselves configurable,
	// so they are filled in here rather than in DefaultConfig: a caller who
	// raises -fuel-sats or -cells should not also have to restate these.
	if c.MinPaymentSatoshis == 0 {
		// Ten fuel coins. Below roughly this a payment cannot buy even one cell
		// transition once fees are paid, so accepting it costs a broadcast and a
		// database row and moves the automaton not at all.
		c.MinPaymentSatoshis = 10 * c.FuelDenomination
	}
	if c.FirstFuelCoins == 0 {
		// Two full generations, so the automaton is visibly running the moment
		// genesis lands while the keeper fills the pool to target behind it.
		c.FirstFuelCoins = 2 * uint64(c.Cells)
	}
	if c.Throughput && c.MinPaymentSatoshis <= c.FuelDenomination {
		return fmt.Errorf("chain: minimum payment %d must exceed one fuel coin of %d satoshis, "+
			"or an accepted payment cannot fund a single transition",
			c.MinPaymentSatoshis, c.FuelDenomination)
	}
	if c.Originator == "" {
		return fmt.Errorf("chain: originator is required")
	}
	if c.DataDir == "" {
		return fmt.Errorf("chain: data directory is required")
	}
	if err := c.Network.Validate(); err != nil {
		return fmt.Errorf("chain: %w", err)
	}
	return nil
}

// utxoManagement builds the funding strategy.
//
// The privacy default has every cell competing for the same unreserved change
// set, which is why 128 concurrent cells collapsed to 4 transactions a second
// with 981 coins in the wallet and one of them claimable. The throughput
// strategy replaces that with a pool of identical coins: because any coin will
// do, the funder's ClaimExact issues a single SKIP LOCKED claim that cannot
// collide with another cell's.
//
// RecycleChangeToPool stays OFF deliberately. It would route each transition's
// change straight back into the pool, chaining every payment onto the previous
// one's change — and this workload already runs 128 unbroken unconfirmed chains.
// Adding a 129th, shared by every cell, is how the toolbox's own benchmarks
// blew past the mempool ancestor limit and took 22,853 rejections.
func (c *Config) utxoManagement() (defs.UTXOManagement, error) {
	um := defs.DefaultUTXOManagement()
	if !c.Throughput {
		return um, nil
	}
	um.Strategy = defs.StrategyThroughput
	um.Throughput.DenominationSatoshis = c.FuelDenomination
	um.Throughput.TargetPoolSize = c.FuelPoolSize
	// A cell transition spends a fuel coin whether or not it is mined yet, and
	// on a test network the pool lingers unproven, so mined-only would find the
	// pool empty and fall back to the contended walk we are trying to escape.
	um.Throughput.SpendPolicy = defs.SpendPolicyPreferMined

	fee := defs.FeeModel{Type: defs.SatPerKB, Value: c.FeeSatPerKB}
	if err := um.Validate(fee, defs.Commission{}); err != nil {
		return um, fmt.Errorf("chain: fuel pool configuration: %w", err)
	}
	return um, nil
}

// FuelPool reports the basket and denomination the fuel keeper maintains, and
// whether the throughput strategy is on at all.
func (c *Config) FuelPool() (basket string, denomination, target uint64, enabled bool) {
	um, err := c.utxoManagement()
	if err != nil || !um.Enabled() {
		return "", 0, 0, false
	}
	return um.Throughput.PoolBasket, c.FuelDenomination, c.FuelPoolSize, true
}

// minSpendBytes is the smallest transaction that can spend one P2PKH output:
// one P2PKH input and one P2PKH output. 8 envelope + 1 input count + 148 input
// + 1 output count + 34 output = 192, the figure the toolbox's own size test
// pins.
const minSpendBytes = 192

// dustFloor is the smallest change output the funder will keep, at feeSatPerKB.
//
// The toolbox does not use a constant. It prices the cheapest possible future
// spend of an output — minSpendBytes — and requires the output to be worth
// TWICE that fee; below it the change is dropped and donated to the miner.
// So the floor moves with the fee rate, and quoting it as a bare number is how
// this repository ended up asserting 40 in one file and 48 in another. Both
// were the same formula: 40 is arcade's 100 sat/kB floor, 48 is the 125 sat/kB
// we actually configure. 48 is the one that applies here.
//
// It matters because WithRequiredChangeOutput turns "the change vanished" from
// a lost rounding error into a transaction the covenant cannot spend.
func dustFloor(feeSatPerKB int64) uint64 {
	return 2 * feeForBytes(minSpendBytes, feeSatPerKB)
}

// feeForBytes prices sizeBytes at feeSatPerKB, rounding up the way the toolbox
// does. A non-positive rate is priced at zero rather than panicking; Validate
// is what refuses it.
func feeForBytes(sizeBytes uint64, feeSatPerKB int64) uint64 {
	if feeSatPerKB <= 0 {
		return 0
	}
	return uint64(math.Ceil(float64(sizeBytes) / 1000 * float64(feeSatPerKB)))
}

// eventsURL is where the status stream lives: the override if there is one,
// otherwise the arcade host itself.
func (c *Config) eventsURL() string {
	if c.EventsURL != "" {
		return c.EventsURL
	}
	return c.ArcadeURL
}
