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

	// Mint where the funder will look. Under the throughput strategy that is the
	// denominated pool basket; otherwise the funder claims from ordinary change.
	// Getting this wrong is silent: the coins exist, the balance looks healthy,
	// and every transition still reports "not enough funds" forever.
	basket := wdk.BasketNameForChange
	if pool, _, _, enabled := c.Config.FuelPool(); enabled {
		basket = pool
	}

	var results []FuelResult
	for minted := uint64(0); minted < count; {
		batch := min(count-minted, fanoutPerTx)

		shape := wdk.ShapedChange{
			Count:    batch,
			Satoshis: primitives.SatoshiValue(satoshis),
			Basket:   primitives.StringUnder300(basket),
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
	um, err := c.Config.utxoManagement()
	if err != nil {
		return err
	}

	cfg := fuelkeeper.FromThroughput(um.Throughput, denom)
	cfg.Originator = c.Config.Originator
	cfg.TargetPoolSize = target
	// Minting serially cannot match the burn rate of 128 cells; the reference
	// deployment that sustained ~1,900 TPS used 64.
	cfg.MintConcurrency = 64
	// The keeper normally pauses for a multiple of its own RPC duration to
	// fair-share the wallet. Under load that duration inflates, so the pause
	// inflates with it and throttles the keeper exactly when the pool is
	// draining fastest.
	cfg.DisableStreamYield = true
	cfg.StreamLeafCap = 2000

	keeper, err := fuelkeeper.New(c.Wallet, cfg, c.logger)
	if err != nil {
		return fmt.Errorf("chain: fuel keeper: %w", err)
	}
	c.logger.InfoContext(ctx, "fuel keeper started",
		"denomination", denom, "target", target, "basket", um.Throughput.PoolBasket)
	keeper.Run(ctx)
	return nil
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
