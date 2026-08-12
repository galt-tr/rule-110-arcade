package chain

import (
	"testing"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/wdk"
)

// TestKeeperRecyclesFromTheChangeBasket is the regression guard for the
// sawtooth: the pool refilling, 128 cells draining it, ModeStarved, repeat.
//
// A transition's change lands in the default basket, one sub-denomination crumb
// per transaction, hundreds a second. With RecycleBasket unset the keeper's only
// way to spend them is to aggregate a couple of hundred into one reserve chunk
// before a leaf can mint from it — serially, one chunk fan-out at a time — and
// it cannot get close to the burn rate. The pool then only refills when an
// operator deposit arrives as a single large claimable coin, which is precisely
// the oscillation that was observed.
func TestKeeperRecyclesFromTheChangeBasket(t *testing.T) {
	cfg := DefaultConfig()
	kc, err := cfg.fuelKeeperConfig()
	if err != nil {
		t.Fatalf("fuelKeeperConfig: %v", err)
	}

	if kc.RecycleBasket != string(wdk.BasketNameForChange) {
		t.Errorf("recycle basket = %q, want %q — without it the keeper can only refill "+
			"the pool from fresh deposits, never from the change its own workload produces",
			kc.RecycleBasket, wdk.BasketNameForChange)
	}
	// Direct recycle only engages when the change basket holds at least two
	// coins per concurrent leaf, so serial minting would disable it in practice
	// even with the basket named.
	if kc.MintConcurrency < 2 {
		t.Errorf("mint concurrency = %d, want the parallel mint the recycle path is sized for", kc.MintConcurrency)
	}
}

// TestChunkIsSizedToTheLeafItFunds pins the reserve chunk to what a leaf
// fan-out actually costs.
//
// Two failures sit either side of this number. Too small and a chunk cannot fund
// the leaf it exists for, so the round mints nothing. Too large and the surplus
// comes back as leaf change INTO THE RESERVE, where ensureChunks counts chunks
// as reserve-balance divided by chunk-value — satoshis rather than coins — so
// the crumbs read as whole chunks that do not exist and the round provisions for
// leaves it cannot fund. The toolbox's max(1000, 8 × denomination) heuristic
// lands on the second failure: 8000 satoshis of headroom against ~500 of real
// cost, and reserve coins on the live deployment measuring ~72,000 against a
// 108,000 chunk.
func TestChunkIsSizedToTheLeafItFunds(t *testing.T) {
	cfg := DefaultConfig()
	kc, err := cfg.fuelKeeperConfig()
	if err != nil {
		t.Fatalf("fuelKeeperConfig: %v", err)
	}

	// What one leaf fan-out really needs beyond the fuel it mints: its own fee,
	// plus enough left over for the change output WithRequiredChangeOutput
	// forces it to keep.
	cost := feeForBytes(leafFanOutBytes, cfg.FeeSatPerKB) + dustFloor(cfg.FeeSatPerKB)

	if kc.ChunkFeeHeadroom < cost {
		t.Errorf("chunk headroom = %d, but a leaf fan-out costs %d — the chunk cannot fund its own leaf",
			kc.ChunkFeeHeadroom, cost)
	}
	if kc.ChunkFeeHeadroom > 4*cost {
		t.Errorf("chunk headroom = %d against a real cost of %d; the surplus returns to the reserve "+
			"as leaf change and inflates ensureChunks' satoshi-based chunk count",
			kc.ChunkFeeHeadroom, cost)
	}
}

// TestFanOutDrawsFromTheChangeBasket is the regression guard for the cold-start
// deadlock: on a fresh wallet `rule110 fuel` reported "not enough funds" while
// the deposit sat plainly in the wallet, and the recorded workaround was to turn
// the throughput strategy off entirely.
//
// The cause is one unset field, and it is invisible from this repository alone.
// storage's fanOutSourceBasket routes a fan-out by its DESTINATION: a shape
// whose Basket is the pool draws its funding from the RESERVE basket, unless
// SourceBasket says otherwise. The reserve is the keeper's own staging area — it
// is filled by aggregating change crumbs — so on a fresh wallet it is empty,
// while the deposit internalize just credited is in change. The one command
// whose job is to create the pool therefore could not fund itself.
//
// The keeper never hit this only because its RecycleBasket setting becomes this
// same field. Naming the source here is what lets the cold start run
// fund -> fuel -> genesis with throughput left on.
func TestFanOutDrawsFromTheChangeBasket(t *testing.T) {
	cfg := DefaultConfig()
	shape, err := cfg.fuelShape(100, cfg.FuelDenomination)
	if err != nil {
		t.Fatalf("fuelShape: %v", err)
	}

	if string(shape.SourceBasket) != string(wdk.BasketNameForChange) {
		t.Errorf("source basket = %q, want %q — an unset source routes a pool-destination "+
			"fan-out to the reserve, which is empty on a fresh wallet while the deposit is in change",
			shape.SourceBasket, wdk.BasketNameForChange)
	}

	// The destination is the other half of the pairing: minting into the wrong
	// basket fails silently, because the coins exist and the balance looks
	// healthy while every transition still reports "not enough funds".
	pool, _, _, enabled := cfg.FuelPool()
	if !enabled {
		t.Fatal("the default configuration has no fuel pool")
	}
	if string(shape.Basket) != pool {
		t.Errorf("destination basket = %q, want the pool %q", shape.Basket, pool)
	}
}

// With the throughput strategy off there is no pool, so a fan-out must mint into
// change — the basket the funder claims from in that mode.
func TestFanOutWithoutAPoolMintsIntoChange(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Throughput = false

	shape, err := cfg.fuelShape(100, cfg.FuelDenomination)
	if err != nil {
		t.Fatalf("fuelShape: %v", err)
	}
	if string(shape.Basket) != string(wdk.BasketNameForChange) {
		t.Errorf("destination basket = %q, want %q with no pool to fill",
			shape.Basket, wdk.BasketNameForChange)
	}
}

// The keeper configuration is derived, not written out, so this is the guard
// that deriving it did not quietly drop one of the deployment's overrides.
func TestKeeperConfigCarriesTheDeploymentsOverrides(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FuelPoolSize = 4096
	kc, err := cfg.fuelKeeperConfig()
	if err != nil {
		t.Fatalf("fuelKeeperConfig: %v", err)
	}

	if kc.TargetPoolSize != 4096 {
		t.Errorf("target pool size = %d, want 4096 (-fuel-pool must reach the keeper)", kc.TargetPoolSize)
	}
	if kc.Denomination != cfg.FuelDenomination {
		t.Errorf("denomination = %d, want %d; a keeper minting at a different value than the funder "+
			"claims at fills the pool with coins nothing can spend", kc.Denomination, cfg.FuelDenomination)
	}
	if kc.Originator != cfg.Originator {
		t.Errorf("originator = %q, want %q", kc.Originator, cfg.Originator)
	}
	if !kc.DisableStreamYield {
		t.Error("the fair-share yield is back on; it scales with RPC duration, so it throttles the " +
			"keeper hardest exactly when the pool is draining fastest")
	}
}

// Without the throughput strategy there is no pool, no reserve and no keeper —
// so asking for its configuration must not quietly produce one shaped around
// baskets that are never used.
func TestNoKeeperConfigWithoutAPool(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Throughput = false

	if _, _, _, enabled := cfg.FuelPool(); enabled {
		t.Fatal("FuelPool reports a pool with the throughput strategy off")
	}
}
