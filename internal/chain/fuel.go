package chain

import (
	"context"
	"fmt"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/wallet/fuelkeeper"

	sdk "github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/wdk"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/wdk/primitives"
)

// fanoutPerTx bounds how many outputs one fan-out transaction mints. It mirrors
// the toolbox's own FanoutOutputsPerTx default.
const fanoutPerTx = 100

// FuelResult reports one fan-out round.
type FuelResult struct {
	TxID  string
	Coins uint64
	Value uint64
}

// FanOutFuel mints `count` equal-value coins so concurrent cell transitions
// each have their own coin to spend.
//
// This is the fix for the failure mode a full generation exposes: the covenant
// forces exactly one change output per transaction, so the change pool grows by
// one coin per transaction while a generation wants N of them at once. With 128
// cells and a handful of coins, most cells starve with "not enough funds" — not
// because the wallet is poor, but because there is nothing left to claim.
//
// Minting is split across rounds because a single fan-out transaction is capped
// at fanoutPerTx outputs.
func (c *Chain) FanOutFuel(ctx context.Context, count, satoshis uint64) ([]FuelResult, error) {
	if count == 0 {
		return nil, fmt.Errorf("chain: coin count must be positive")
	}
	if satoshis == 0 {
		return nil, fmt.Errorf("chain: coin value must be positive")
	}

	var results []FuelResult
	for minted := uint64(0); minted < count; {
		batch := min(count-minted, fanoutPerTx)

		shape, err := c.Config.fuelShape(batch, satoshis)
		if err != nil {
			return results, err
		}

		// Each round spends the previous round's change, and that coin is not
		// claimable until the monitor applies its status. Without this retry a
		// multi-round fan-out reliably dies on the second batch.
		res, err := retryUntilFunded(ctx, func() (*sdk.CreateActionResult, error) {
			return c.Wallet.FanOutFuel(ctx, shape, c.Config.Originator)
		})
		if err != nil {
			return results, fmt.Errorf("chain: fan out fuel (minted %d of %d): %w", minted, count, err)
		}

		results = append(results, FuelResult{
			TxID:  res.Txid.String(),
			Coins: batch,
			Value: satoshis,
		})
		minted += batch
	}
	return results, nil
}

// fuelShape describes one fan-out round: how many coins, of what value, into
// which basket — and, crucially, out of which basket they are FUNDED.
//
// Both baskets have a silent failure behind them, which is why this is one
// function with one comment rather than two literals at the call site.
//
// The DESTINATION is where the funder will look for the coins. Under the
// throughput strategy that is the denominated pool; otherwise the funder claims
// from ordinary change. Minting into the wrong one is invisible: the coins
// exist, the balance reads healthy, and every transition still reports "not
// enough funds" forever.
//
// The SOURCE is the fix for the cold-start deadlock recorded against this
// project — `rule110 fuel` failing with "not enough funds" on a fresh wallet
// that plainly held a deposit, worked around by turning throughput off. storage's
// fanOutSourceBasket routes a fan-out by its destination: a pool-destination
// shape draws from the RESERVE basket unless SourceBasket overrides it. The
// reserve is the keeper's staging area, filled by aggregating change crumbs, so
// on a fresh wallet it is empty while the deposit internalize just credited sits
// in change. The one command whose job is to create the pool could not fund
// itself. The keeper never had the bug only because its RecycleBasket setting
// becomes this same field.
//
// Sourcing from change unconditionally is deliberate rather than conditional on
// the reserve being empty. This path exists to turn DEPOSITS into fuel, and a
// deposit always lands in change — InternalizeAction credits the change basket.
// The reserve belongs to the keeper, which drains it through its own leaf path.
func (c *Config) fuelShape(count, satoshis uint64) (wdk.ShapedChange, error) {
	if count == 0 {
		return wdk.ShapedChange{}, fmt.Errorf("chain: coin count must be positive")
	}
	if satoshis == 0 {
		return wdk.ShapedChange{}, fmt.Errorf("chain: coin value must be positive")
	}

	basket := string(wdk.BasketNameForChange)
	if pool, _, _, enabled := c.FuelPool(); enabled {
		basket = pool
	}

	return wdk.ShapedChange{
		Count:        count,
		Satoshis:     primitives.SatoshiValue(satoshis),
		Basket:       primitives.StringUnder300(basket),
		SourceBasket: primitives.StringUnder300(wdk.BasketNameForChange),
	}, nil
}

// RunFuelKeeper keeps the denominated pool topped up for as long as ctx lives.
//
// Without it the pool only drains: throughput change deliberately does NOT
// recycle back into the pool (that would chain every transition onto the
// previous one's change, and this workload already runs one unbroken
// unconfirmed chain per cell). So something has to mint replacements, and this
// is it — the keeper is client-side because it needs the operator's keys.
//
// A no-op when the throughput strategy is off, since there is no pool to keep.
func (c *Chain) RunFuelKeeper(ctx context.Context) error {
	_, denom, target, enabled := c.Config.FuelPool()
	if !enabled {
		return nil
	}
	cfg, err := c.Config.fuelKeeperConfig()
	if err != nil {
		return err
	}

	keeper, err := fuelkeeper.New(c.Wallet, cfg, c.logger)
	if err != nil {
		return fmt.Errorf("chain: fuel keeper: %w", err)
	}
	c.logger.InfoContext(ctx, "fuel keeper started",
		"denomination", denom, "target", target, "basket", cfg.PoolBasket,
		"recycleBasket", cfg.RecycleBasket, "chunkFeeHeadroom", cfg.ChunkFeeHeadroom)
	keeper.Run(ctx)
	return nil
}

// fuelKeeperConfig derives the keeper's tuning from ours.
//
// Split out from RunFuelKeeper so the numbers below can be asserted without a
// wallet, an arcade, or money. Every one of them is a deployment-shaped
// override of a toolbox default, and the last two were the difference between
// a pool that sustains 128 cells and one that sawtooths into ModeStarved.
func (c *Config) fuelKeeperConfig() (fuelkeeper.Config, error) {
	um, err := c.utxoManagement()
	if err != nil {
		return fuelkeeper.Config{}, err
	}

	cfg := fuelkeeper.FromThroughput(um.Throughput, c.FuelDenomination)
	cfg.Originator = c.Originator
	cfg.TargetPoolSize = c.FuelPoolSize
	// Minting serially cannot match the burn rate of 128 cells; the reference
	// deployment that sustained ~1,900 TPS used 64.
	cfg.MintConcurrency = 64
	// The keeper normally pauses for a multiple of its own RPC duration to
	// fair-share the wallet. Under load that duration inflates, so the pause
	// inflates with it and throttles the keeper exactly when the pool is
	// draining fastest.
	cfg.DisableStreamYield = true
	cfg.StreamLeafCap = 2000

	// Mint fuel straight out of the change basket, not only out of reserve
	// chunks. This is the fix for the observed oscillation, and it is worth
	// spelling out because the mechanism is not visible from any one file.
	//
	// A cell transition spends one 1000 satoshi fuel coin, pays ~512 of it in
	// fee, and returns the ~488 satoshi remainder as change. With
	// RecycleChangeToPool off — correctly off, see utxoManagement — that change
	// lands in the DEFAULT basket. So the wallet accrues roughly one 488
	// satoshi crumb per transition, hundreds a second, and the keeper's chunk
	// path is the only thing that can spend them: it must aggregate ~220 of
	// them into a single 100,996 satoshi reserve chunk, one chunk fan-out at a
	// time, before a leaf can mint 100 fuel coins from it. It cannot get close.
	// The pool empties, the automaton starves, an operator deposit arrives as
	// one big claimable coin, chunking runs briefly at full speed, the pool
	// refills, and the whole thing repeats.
	//
	// The direct-recycle path funds each leaf from the change basket itself —
	// RecycleCount coins per leaf, MintConcurrency leaves at once — so ~18
	// crumbs become 8 fuel coins in one transaction and 64 of those run in
	// parallel. The keeper falls back to the chunk path on its own whenever the
	// change basket holds too few distinct coins to parallelize (bootstrap, or
	// a fresh deposit), so the reserve is still drained and nothing strands.
	//
	// The cost is ancestry. A recycle leaf spends crumbs belonging to ~18
	// DIFFERENT cells, so it merges 18 unconfirmed chains and every fuel coin
	// it mints carries that merged history into whichever cell spends it. The
	// chunk path merges the same ancestry when it consumes crumbs, but in
	// practice it rarely does — the funder prefers one sufficient coin to two
	// hundred insufficient ones — so this genuinely makes the unconfirmed
	// ancestor set larger, not merely differently shaped. We are doing it
	// anyway because `rule110 depth-probe` found no ancestor limit on this
	// network to violate, and because the alternative measured on the live
	// deployment is a pool that cannot be refilled at all. Re-probe before
	// trusting it on a network we have not measured.
	cfg.RecycleBasket = string(wdk.BasketNameForChange)

	// Size a reserve chunk to what a leaf fan-out actually costs, rather than
	// FromThroughput's max(1000, 8 × denomination) heuristic — which the
	// toolbox itself invites callers to override, and which comes out at 8000
	// here, sixteen times the real figure.
	//
	// The excess does not evaporate. A fan-out's change returns to the basket
	// that funded it, so a leaf funded from the reserve leaves its unspent
	// headroom BACK in the reserve, as a coin far too small to fund another
	// leaf. ensureChunks then counts chunks as reserve-balance ÷ chunk-value —
	// satoshis, not coins — so fourteen of those crumbs read as one whole chunk
	// that does not exist, the round provisions for leaves it cannot fund, and
	// those leaves fail. Reserve coins measured at ~72,000 satoshis on the live
	// deployment against a 108,000 chunk value is that arithmetic showing.
	cfg.ChunkFeeHeadroom = c.chunkFeeHeadroom()

	return cfg, nil
}

// leafFanOutBytes is the serialized size of one leaf fan-out: a single P2PKH
// input spending a reserve chunk, fanoutPerTx fuel outputs, and the one change
// output WithRequiredChangeOutput insists on. 8 envelope + 1 input count + 148
// input + 3 output count + 101 × 34 outputs = 3,594.
//
// Every script in it is P2PKH, so unlike a cell transition this estimate is not
// a guess — there is no covenant to come out longer than planned.
const leafFanOutBytes = 8 + 1 + 148 + 3 + (fanoutPerTx+1)*34

// chunkFeeHeadroom is what a reserve chunk carries beyond the fuel it mints:
// the leaf fan-out's fee, plus the dust floor its change has to clear, doubled
// so a chunk still funds a leaf if the rate or the shape moves a little.
//
// At the default 125 sat/kB that is 2 × (450 + 48) = 996, against the toolbox
// heuristic's 8000. See fuelKeeperConfig for why the difference matters.
func (c *Config) chunkFeeHeadroom() uint64 {
	return 2 * (feeForBytes(leafFanOutBytes, c.FeeSatPerKB) + dustFloor(c.FeeSatPerKB))
}

// ClaimableCoins reports how many coins the funder can currently claim, from
// whichever basket it is actually claiming from.
func (c *Chain) ClaimableCoins(ctx context.Context) (int, error) {
	basket := wdk.BasketNameForChange
	if pool, _, _, enabled := c.Config.FuelPool(); enabled {
		basket = pool
	}
	n, err := c.Wallet.BasketClaimableCount(ctx, string(basket))
	if err != nil {
		return 0, fmt.Errorf("chain: count claimable coins: %w", err)
	}
	return n, nil
}
