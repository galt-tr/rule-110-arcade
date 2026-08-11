// Package chain wires the arcade toolbox wallet for the automaton: it holds
// the cell UTXOs, funds their transitions from a denominated fuel pool, and
// broadcasts through arcade.
package chain

import (
	"fmt"
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
	// write, so 128 cells advancing at once thrash a single writer lock; the
	// toolbox's own benchmarks put SQLite at ~57-108 TPS against ~575 for
	// PostgreSQL. Empty keeps SQLite, which is fine for a small ring.
	PostgresDSN string

	// MaxDBConns bounds the storage connection pool. The benchmarks pair it
	// with worker count (conns ~ workers + margin).
	MaxDBConns int

	// Concurrency bounds how many cells advance at once. Unbounded fan-out
	// makes SQLite worse, not better: the writers queue on a lock and the
	// latency shows up as a stalled generation.
	Concurrency int

	// MaxUnconfirmedDepth bounds how far ahead of its newest mined transaction
	// a cell may run, or 0 for no bound.
	//
	// A cell is an unbroken chain of unconfirmed transactions, so its depth
	// grows at the generation rate and only falls when a block lands. The
	// original justification was a mempool ancestor limit: past it the deepest
	// transaction is rejected and the rejection cascades to every descendant,
	// which destroys a cell rather than costing a step.
	//
	// WE COULD NOT FIND THAT LIMIT. `rule110 depth-probe` built 600 consecutive
	// transactions on a single chain against the dev-ovh-1 scale network,
	// reaching at least 250 unconfirmed ancestors, with zero rejections. That
	// matches arcade enforcing no ancestor limit — its LimitAncestorCount is a
	// dead value with no setter — and teranode's documentation saying ancestor
	// tracking is not enforced. The cascade recorded in the toolbox benchmarks
	// may well have had another cause.
	//
	// So this is kept as a deliberate margin, not a proven boundary: the
	// benchmarks did observe SOME cascade, other networks may differ, and the
	// bound also usefully caps the in-flight set the status pipeline and
	// reconciler have to carry. Set it from a probe of the network you are
	// actually running against rather than trusting this default.
	MaxUnconfirmedDepth uint64

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
		Cells:        128,
		Rule:         ca.Rule110,
		CellSatoshis: 1,
		Chronicle:    true,
		MaxDBConns:   72,
		Concurrency:  32,
		// Below the 250 we measured without a rejection, well above the 64 this
		// was set to when the limit was still assumed rather than tested.
		MaxUnconfirmedDepth: 200,
		MaxLag:              32,
		FeeSatPerKB:         125,
		MinBroadcastFeeRate: 100,
		// A cell transition is ~4.1 kB, so ~512 satoshis of fee at 125 sat/kB.
		// 1000 leaves comfortable change above the ~48 satoshi dust floor while
		// stranding little value per coin.
		FuelDenomination:  1000,
		FuelPoolSize:      20000,
		Throughput:        true,
		FullStatusUpdates: true,
		ApplyConcurrency:  32,
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
