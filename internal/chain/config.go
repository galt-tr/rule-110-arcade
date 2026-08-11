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
