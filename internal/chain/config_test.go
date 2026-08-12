package chain

import "testing"

// TestFuelPoolReportsWhatTheFunderWillClaim is the table that would have caught
// `rule110 fuel -sats 20000` minting into a pool the funder claims at 1000.
//
// FuelPool is the single answer to "what value coin does the funder actually
// claim, and out of which basket". Everything that mints fuel has to agree with
// it exactly, because ClaimExact matches on value — so the answer must be the
// denomination this configuration asked for, not whatever the toolbox would
// derive from its own expected action shape, and it must be absent entirely
// when there is no pool.
func TestFuelPoolReportsWhatTheFunderWillClaim(t *testing.T) {
	tests := map[string]struct {
		mutate      func(*Config)
		wantEnabled bool
		wantBasket  string
		wantDenom   uint64
		wantTarget  uint64
	}{
		// Derived rather than pinned: the assertion that matters here is that
		// FuelPool hands back the configuration's own numbers instead of the
		// toolbox's derivation from its expected action shape. Hard-coding them
		// would make this case fail every time a default is retuned, which
		// tests the changelog rather than the contract.
		"defaults": {
			mutate:      func(*Config) {},
			wantEnabled: true, wantBasket: "fuel",
			wantDenom: DefaultConfig().FuelDenomination, wantTarget: DefaultConfig().FuelPoolSize,
		},
		"the denomination and pool size are ours, not the toolbox's derivation": {
			mutate:      func(c *Config) { c.FuelDenomination = 2500; c.FuelPoolSize = 4096 },
			wantEnabled: true, wantBasket: "fuel", wantDenom: 2500, wantTarget: 4096,
		},
		"throughput off means there is no pool to agree with": {
			mutate: func(c *Config) { c.Throughput = false },
		},
		// The funder would spend more on the input than the coin holds, so the
		// toolbox refuses the strategy rather than minting unspendable fuel.
		"a denomination under the marginal input fee is not a pool": {
			mutate: func(c *Config) { c.FuelDenomination = 1 },
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			test.mutate(&cfg)

			basket, denom, target, enabled := cfg.FuelPool()
			if enabled != test.wantEnabled {
				t.Fatalf("enabled = %v, want %v", enabled, test.wantEnabled)
			}
			if !enabled {
				return
			}
			if basket != test.wantBasket {
				t.Errorf("basket = %q, want %q", basket, test.wantBasket)
			}
			if denom != test.wantDenom {
				t.Errorf("denomination = %d, want %d — anything minting fuel must use this exact value", denom, test.wantDenom)
			}
			if target != test.wantTarget {
				t.Errorf("target = %d, want %d", target, test.wantTarget)
			}
		})
	}
}

// TestDustFloorTracksTheFeeRate settles a disagreement between two comments in
// this package, one quoting a 40 satoshi dust floor and the other 48. Both were
// the same formula — twice the fee for the cheapest possible spend of the
// output — at different rates, which is exactly why quoting it as a bare number
// went wrong. 48 is the one that applies at the rate we configure.
func TestDustFloorTracksTheFeeRate(t *testing.T) {
	if got := dustFloor(100); got != 40 {
		t.Errorf("dustFloor(100) = %d, want 40 (arcade's floor rate)", got)
	}
	if got := dustFloor(DefaultConfig().FeeSatPerKB); got != 48 {
		t.Errorf("dustFloor at the configured rate = %d, want 48", got)
	}
}

// TestValidateRejectsSettingsThatFailFarFromTheirCause covers the two fields
// whose zero value is accepted here and then goes wrong somewhere unrecognisable
// — one at arcade, per transaction; the other not at all, because the monitor
// silently ignores it and runs a default the comment above the field calls too
// low.
func TestValidateRejectsSettingsThatFailFarFromTheirCause(t *testing.T) {
	tests := map[string]struct {
		mutate  func(*Config)
		wantErr bool
	}{
		"a valid configuration":       {mutate: func(*Config) {}},
		"no fee rate":                 {mutate: func(c *Config) { c.FeeSatPerKB = 0 }, wantErr: true},
		"a negative fee rate":         {mutate: func(c *Config) { c.FeeSatPerKB = -1 }, wantErr: true},
		"no status appliers":          {mutate: func(c *Config) { c.ApplyConcurrency = 0 }, wantErr: true},
		"negative status appliers":    {mutate: func(c *Config) { c.ApplyConcurrency = -8 }, wantErr: true},
		"no local fee floor is fine":  {mutate: func(c *Config) { c.MinBroadcastFeeRate = 0 }},
		"no depth limit is still ok":  {mutate: func(c *Config) { c.MaxUnconfirmedDepth = 0 }},
		"a ring that is not a nibble": {mutate: func(c *Config) { c.Cells = 12 }, wantErr: true},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.ArcadeURL = "https://arcade.example/"
			test.mutate(&cfg)

			err := cfg.Validate()
			if test.wantErr && err == nil {
				t.Fatal("Validate accepted a configuration that cannot work, deferring the failure to somewhere unrecognisable")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

// TestValidateFillsInTheDerivedAmounts pins the two settings a deployment is
// not expected to state.
//
// Both are derived from numbers that are themselves configurable, so an
// operator who raises -fuel-sats or the ring size should not have to restate
// them — and the failure of leaving them at zero is quiet in both directions: a
// zero minimum accepts dust that cannot buy a single transition, and zero first
// fuel mints nothing, so genesis lands on an empty pool and the automaton comes
// up correct and immediately starved.
func TestValidateFillsInTheDerivedAmounts(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ArcadeURL = "https://arcade.example"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	if cfg.MinPaymentSatoshis <= cfg.FuelDenomination {
		t.Errorf("minimum payment = %d, not above one fuel coin of %d",
			cfg.MinPaymentSatoshis, cfg.FuelDenomination)
	}
	if cfg.FirstFuelCoins < uint64(cfg.Cells) {
		t.Errorf("first fuel = %d coins for a ring of %d; genesis would land on a pool "+
			"that cannot fund even one generation", cfg.FirstFuelCoins, cfg.Cells)
	}
}

// An explicitly configured amount must survive validation untouched — the
// fill-in is for zero, not a correction of the operator.
func TestValidateKeepsExplicitAmounts(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ArcadeURL = "https://arcade.example"
	cfg.MinPaymentSatoshis = 777_000
	cfg.FirstFuelCoins = 9

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.MinPaymentSatoshis != 777_000 {
		t.Errorf("minimum payment = %d, want the configured 777000", cfg.MinPaymentSatoshis)
	}
	if cfg.FirstFuelCoins != 9 {
		t.Errorf("first fuel = %d, want the configured 9", cfg.FirstFuelCoins)
	}
}

// A minimum payment at or below one fuel coin cannot buy a transition once fees
// are paid, so accepting it costs a broadcast and a row and moves the automaton
// not at all. Caught here rather than discovered as a pool that never grows.
func TestValidateRejectsAMinimumBelowOneFuelCoin(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ArcadeURL = "https://arcade.example"
	cfg.MinPaymentSatoshis = cfg.FuelDenomination

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted a minimum payment that cannot fund a single transition")
	}
}

// The depth gate caps the sustained rate at depth / block interval, so a finite
// default would silently cap the automaton no matter what rate is configured.
// Teranode enforces no ancestor limit; this pins that the shipped default does
// not reintroduce one.
func TestDefaultDepthIsUnbounded(t *testing.T) {
	if got := DefaultConfig().MaxUnconfirmedDepth; got != 0 {
		t.Errorf("default max unconfirmed depth = %d, want 0 (unbounded) — a finite value caps "+
			"the sustained rate at depth / block interval, and teranode enforces no ancestor limit", got)
	}
}
