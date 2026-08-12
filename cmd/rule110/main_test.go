package main

import (
	"testing"

	"github.com/dymurray/rule-110-arcade/internal/chain"
)

// TestFuelCoinValueAgreesWithTheDenomination is the table that catches a
// minting command and a funder that disagree about what a fuel coin is worth.
//
// `-sats` shipped defaulting to 20000 against a 1000 satoshi denomination. The
// funder's fast path claims by exact value, so those coins sat in the pool
// where nothing looking for them could see them: the balance read healthy, the
// keeper counted them as twenty times the fuel they could buy, and the
// transitions fell back to the contended walk the strategy exists to escape.
func TestFuelCoinValueAgreesWithTheDenomination(t *testing.T) {
	tests := map[string]struct {
		sats    uint64
		mutate  func(*chain.Config)
		want    uint64
		wantErr bool
	}{
		"unset mints at the configured denomination": {
			sats: 0, want: 1000,
		},
		"the default this flag used to ship with is refused": {
			sats: 20000, wantErr: true,
		},
		"an explicit value that matches is fine": {
			sats: 1000, want: 1000,
		},
		"unset follows -fuel-sats": {
			sats: 0, mutate: func(c *chain.Config) { c.FuelDenomination = 2500 }, want: 2500,
		},
		"a value that matches a changed denomination is fine": {
			sats: 2500, mutate: func(c *chain.Config) { c.FuelDenomination = 2500 }, want: 2500,
		},
		"a value that no longer matches is refused": {
			sats: 1000, mutate: func(c *chain.Config) { c.FuelDenomination = 2500 }, wantErr: true,
		},
		// Without the pool the funder claims from ordinary change, where any
		// value spends — but there is then nothing to default to either.
		"without a pool any value goes": {
			sats: 20000, mutate: func(c *chain.Config) { c.Throughput = false }, want: 20000,
		},
		"without a pool there is no denomination to default to": {
			sats: 0, mutate: func(c *chain.Config) { c.Throughput = false }, wantErr: true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := chain.DefaultConfig()
			if test.mutate != nil {
				test.mutate(&cfg)
			}

			got, err := fuelCoinValue(cfg, test.sats)
			if test.wantErr {
				if err == nil {
					t.Fatalf("fuelCoinValue(%d) = %d with no error; minting at a value the funder "+
						"does not claim at is the failure this refuses", test.sats, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("fuelCoinValue(%d): %v", test.sats, err)
			}
			if got != test.want {
				t.Errorf("fuelCoinValue(%d) = %d, want %d", test.sats, got, test.want)
			}
		})
	}
}

// TestParseRuleRefusesWhatItCannotRepresent: an elementary rule is one byte, so
// -rule 300 used to convert down to rule 44 and run a different automaton with
// no error and nothing in the output to say so.
func TestParseRuleRefusesWhatItCannotRepresent(t *testing.T) {
	for _, n := range []uint{0, 30, 110, 255} {
		got, err := parseRule(n)
		if err != nil {
			t.Errorf("parseRule(%d): %v", n, err)
		}
		if uint(got) != n {
			t.Errorf("parseRule(%d) = %d", n, got)
		}
	}
	for _, n := range []uint{256, 300, 1 << 16} {
		if got, err := parseRule(n); err == nil {
			t.Errorf("parseRule(%d) = %d with no error; that is a different automaton, silently", n, got)
		}
	}
}

// TestUIURL covers the printed "UI ready at" line, which is the first thing an
// operator copies out of the logs. The wildcard cases are the ones that matter:
// a container sets RULE110_ADDR=0.0.0.0:8110, and the old string concatenation
// rendered that as "http://localhost0.0.0.0:8110".
func TestUIURL(t *testing.T) {
	cases := []struct{ addr, want string }{
		{":8110", "http://localhost:8110"},
		{"0.0.0.0:8110", "http://localhost:8110"},
		{"[::]:8110", "http://localhost:8110"},
		{"127.0.0.1:9000", "http://127.0.0.1:9000"},
		{"rule110.internal:80", "http://rule110.internal:80"},
		{"[::1]:8110", "http://[::1]:8110"},
		// Not a host:port at all. Printing something imperfect beats failing to
		// print the line the operator is looking for.
		{"8110", "http://8110"},
	}
	for _, c := range cases {
		if got := uiURL(c.addr); got != c.want {
			t.Errorf("uiURL(%q) = %q, want %q", c.addr, got, c.want)
		}
	}
}
