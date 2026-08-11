// Command rule110 runs a Rule 110 cellular automaton as a covenant on BSV.
//
// Each cell is a UTXO carrying the whole current row; spending it proves, in
// native Bitcoin Script, that one bit of the next row is correct. A generation
// of N cells is therefore N independent transactions.
//
// Subcommands:
//
//	address   print the address that funds this deployment
//	fund      internalize a mined funding transaction
//	run       start the automaton and its web UI
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/defs"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/chain"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "rule110: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("a subcommand is required")
	}
	switch args[0] {
	case "address":
		return cmdAddress(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `rule110 — Rule 110 as a Bitcoin Script covenant

  rule110 address [flags]   print the funding address for this deployment
  rule110 help              show this message

Common flags:
  -arcade-url string   arcade instance to broadcast through
  -network string      main | test | ttn | tstn  (default tstn)
  -data-dir string     wallet database and key file (this IS the wallet)
  -cells int           ring size, a multiple of 8
`)
}

// bindCommon registers the flags every subcommand shares.
func bindCommon(fs *flag.FlagSet, cfg *chain.Config) *string {
	fs.StringVar(&cfg.ArcadeURL, "arcade-url", envOr("RULE110_ARCADE_URL", ""),
		"arcade instance to broadcast through (required)")
	fs.StringVar(&cfg.ChainTracksURL, "chaintracks-url", envOr("RULE110_CHAINTRACKS_URL", ""),
		"headers service; empty derives it from the arcade URL")
	fs.StringVar(&cfg.DataDir, "data-dir", envOr("RULE110_DATA_DIR", cfg.DataDir),
		"wallet database and key file — losing this loses the coins")
	fs.StringVar(&cfg.Originator, "originator", cfg.Originator, "BRC-100 originator (FQDN-shaped)")
	fs.IntVar(&cfg.Cells, "cells", cfg.Cells, "ring size (multiple of 8)")
	fs.Uint64Var(&cfg.CellSatoshis, "cell-sats", cfg.CellSatoshis, "satoshis each cell UTXO carries")
	fs.BoolVar(&cfg.Chronicle, "chronicle", cfg.Chronicle,
		"verify with Chronicle-era script rules (required for Rúnar covenants)")

	network := fs.String("network", envOr("RULE110_NETWORK", string(defs.NetworkTSTN)),
		"main | test | ttn (Teranode test net) | tstn (private scaling test net)")
	return network
}

func cmdAddress(args []string) error {
	cfg := chain.DefaultConfig()
	fs := flag.NewFlagSet("address", flag.ContinueOnError)
	network := bindCommon(fs, &cfg)
	rule := fs.Uint("rule", uint(cfg.Rule), "Wolfram rule number")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.Rule = ca.Rule(*rule)

	net, err := parseNetwork(*network)
	if err != nil {
		return err
	}
	cfg.Network = net

	// Deriving an address is offline on purpose: you can hand it out before any
	// service is reachable, so the arcade URL is not required here.
	id, err := chain.LoadOrCreateIdentity(cfg.DataDir)
	if err != nil {
		return err
	}
	target, err := chain.FundingAddress(id, cfg.Network)
	if err != nil {
		return err
	}

	fmt.Printf("network:         %s\n", cfg.Network)
	fmt.Printf("data dir:        %s\n", cfg.DataDir)
	fmt.Println()
	fmt.Printf("FUNDING ADDRESS: %s\n", target.Address)
	fmt.Println()
	fmt.Printf("locking script:  %s\n", target.LockingScriptHex)
	fmt.Printf("sender pubkey:   %s\n", target.SenderPublicKeyHex)
	fmt.Printf("derivation:      %s / %s\n", target.DerivationPrefix, target.DerivationSuffix)
	fmt.Println()
	fmt.Println("Send a payment to the address above, wait for it to be mined, then run:")
	fmt.Printf("  rule110 fund -tx <raw-tx-hex> -data-dir %s\n", cfg.DataDir)
	fmt.Println()
	fmt.Println("The funding transaction must be MINED before it can be internalized:")
	fmt.Println("the wallet verifies its merkle proof against the headers service.")

	return nil
}

// parseNetwork delegates to the toolbox, which is the authority on which
// networks exist and how they are spelled ("main"/"test"/"ttn"/"tstn" — not
// "mainnet"/"testnet").
func parseNetwork(s string) (defs.BSVNetwork, error) {
	n, err := defs.ParseBSVNetworkStr(strings.TrimSpace(s))
	if err != nil {
		return "", fmt.Errorf("chain: %w (valid: main, test, ttn, tstn)", err)
	}
	return n, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
