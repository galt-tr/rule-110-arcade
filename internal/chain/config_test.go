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
		"defaults": {
			mutate:      func(*Config) {},
			wantEnabled: true, wantBasket: "fuel", wantDenom: 1000, wantTarget: 20000,
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
