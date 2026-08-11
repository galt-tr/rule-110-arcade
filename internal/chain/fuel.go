package chain

import (
	"context"
	"fmt"

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

		shape := wdk.ShapedChange{
			Count:    batch,
			Satoshis: primitives.SatoshiValue(satoshis),
			// Mint into the change basket. The dedicated pool basket is not
			// wired end to end yet (storage does not honour Options.FuelShape),
			// so a fan-out lands as ordinary change — which is exactly what the
			// funder claims from, and what we need here.
			Basket: primitives.StringUnder300(wdk.BasketNameForChange),
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

// ClaimableCoins reports how many coins the change basket currently holds.
func (c *Chain) ClaimableCoins(ctx context.Context) (int, error) {
	n, err := c.Wallet.BasketClaimableCount(ctx, string(wdk.BasketNameForChange))
	if err != nil {
		return 0, fmt.Errorf("chain: count claimable coins: %w", err)
	}
	return n, nil
}
