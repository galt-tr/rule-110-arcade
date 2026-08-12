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
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/defs"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/engine"
	"github.com/dymurray/rule-110-arcade/internal/history"
	"github.com/dymurray/rule-110-arcade/internal/web"
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
	case "fund":
		return cmdFund(args[1:])
	case "genesis":
		return cmdGenesis(args[1:])
	case "step":
		return cmdStep(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "fuel":
		return cmdFuel(args[1:])
	case "recover":
		return cmdRecover(args[1:])
	case "import-tips":
		return cmdImportTips(args[1:])
	case "depth-probe":
		return cmdDepthProbe(args[1:])
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
  rule110 fund [flags]      internalize a mined funding transaction
  rule110 genesis [flags]   create generation 0: one UTXO per cell
  rule110 step [flags]      advance one cell by one generation
  rule110 run [flags]       start the automaton and its web UI
  rule110 fuel [flags]      mint coins so a whole generation can fan out
  rule110 recover [flags]   resolve cells whose tip is unknown (dry run unless -apply)
  rule110 import-tips       backfill the history store from a legacy state.json (dry run unless -apply)
  rule110 depth-probe       measure how deep an unconfirmed chain this network accepts
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
	fs.StringVar(&cfg.PostgresDSN, "postgres-dsn", envOr("RULE110_POSTGRES_DSN", ""),
		"PostgreSQL DSN; empty uses SQLite (much slower under a wide fan-out)")
	fs.IntVar(&cfg.MaxDBConns, "max-db-conns", cfg.MaxDBConns, "storage connection pool size")
	fs.IntVar(&cfg.Concurrency, "concurrency", cfg.Concurrency, "how many cells advance at once")
	fs.BoolVar(&cfg.Chronicle, "chronicle", cfg.Chronicle,
		"verify with Chronicle-era script rules (required for Rúnar covenants)")
	fs.Int64Var(&cfg.FeeSatPerKB, "fee-sat-per-kb", cfg.FeeSatPerKB,
		"fee rate; must exceed arcade's 100 sat/kB floor, which it applies to the extended-format size")
	fs.BoolVar(&cfg.Throughput, "throughput", cfg.Throughput,
		"fund from a denominated fuel pool instead of contending for change")
	fs.Uint64Var(&cfg.FuelDenomination, "fuel-sats", cfg.FuelDenomination,
		"value of one fuel coin; must clear one transition's fee plus the dust floor")
	fs.Uint64Var(&cfg.FuelPoolSize, "fuel-pool", cfg.FuelPoolSize,
		"how many fuel coins the keeper maintains")
	fs.Uint64Var(&cfg.MaxUnconfirmedDepth, "max-depth", cfg.MaxUnconfirmedDepth,
		"how far a cell may run ahead of its newest mined transaction (0 = unbounded)")
	fs.Uint64Var(&cfg.MaxLag, "max-lag", cfg.MaxLag,
		"how far the clock may run ahead of the slowest cell")
	fs.IntVar(&cfg.ApplyConcurrency, "apply-concurrency", cfg.ApplyConcurrency,
		"monitor workers applying arcade status batches")
	fs.BoolVar(&cfg.FullStatusUpdates, "full-status", cfg.FullStatusUpdates,
		"subscribe to every status transition (~4x the events; turn off above ~3 gen/s)")

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

func cmdFund(args []string) error {
	cfg := chain.DefaultConfig()
	fs := flag.NewFlagSet("fund", flag.ContinueOnError)
	network := bindCommon(fs, &cfg)
	txHex := fs.String("tx", "", "funding transaction hex (raw or Extended Format)")
	bumpHex := fs.String("bump", "", "BUMP merkle path hex proving the transaction was mined")
	vout := fs.Uint("vout", 0, "index of the output paying this wallet")
	desc := fs.String("description", "rule110 deployment funding", "description recorded on the action")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *txHex == "" || *bumpHex == "" {
		return fmt.Errorf("both -tx and -bump are required (the payment must be mined to be internalized)")
	}

	net, err := parseNetwork(*network)
	if err != nil {
		return err
	}
	cfg.Network = net

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	c, err := chain.Open(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()

	fmt.Printf("wallet identity: %s\n", c.IdentityKey)

	before, err := c.Balance(ctx)
	if err != nil {
		return fmt.Errorf("read balance: %w", err)
	}

	if err := c.Internalize(ctx, *txHex, *bumpHex, uint32(*vout), *desc); err != nil {
		return err
	}

	after, err := c.Balance(ctx)
	if err != nil {
		return fmt.Errorf("read balance: %w", err)
	}
	fmt.Printf("balance: %d -> %d sat (+%d)\n", before, after, after-before)
	return nil
}

func cmdGenesis(args []string) error {
	cfg := chain.DefaultConfig()
	fs := flag.NewFlagSet("genesis", flag.ContinueOnError)
	network := bindCommon(fs, &cfg)
	rule := fs.Uint("rule", uint(cfg.Rule), "Wolfram rule number")
	seedHex := fs.String("seed", "", "initial row as hex; empty seeds a single live cell at index 0")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg.Rule = ca.Rule(*rule)

	net, err := parseNetwork(*network)
	if err != nil {
		return err
	}
	cfg.Network = net

	seed, err := seedRow(cfg.Cells, *seedHex)
	if err != nil {
		return err
	}

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	compiled, err := cellscript.Compile(cfg.Cells, cfg.Rule)
	if err != nil {
		return err
	}

	c, err := chain.Open(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()

	fmt.Println("waiting for claimable funds (monitor applies statuses on startup)...")
	if err := c.WaitForClaimableFunds(ctx, 3*time.Minute); err != nil {
		return err
	}

	balance, err := c.Balance(ctx)
	if err != nil {
		return fmt.Errorf("read balance: %w", err)
	}
	fmt.Printf("balance before: %d sat\n", balance)
	fmt.Printf("creating %d cells, rule %d, seed %s\n", cfg.Cells, cfg.Rule, seed.Hex())

	d, err := c.Genesis(ctx, compiled, seed)
	if err != nil {
		return err
	}

	after, err := c.Balance(ctx)
	if err != nil {
		return fmt.Errorf("read balance: %w", err)
	}

	fmt.Println()
	fmt.Printf("GENESIS TXID: %s\n", d.GenesisTxID)
	fmt.Println()
	fmt.Printf("cells:          %d (vout 0..%d)\n", d.Cells, d.Cells-1)
	fmt.Printf("seed:           %s\n", d.SeedHex)
	fmt.Printf("balance after:  %d sat (spent %d)\n", after, balance-after)
	return nil
}

func cmdStep(args []string) error {
	cfg := chain.DefaultConfig()
	fs := flag.NewFlagSet("step", flag.ContinueOnError)
	network := bindCommon(fs, &cfg)
	cell := fs.Int("cell", 0, "which cell to advance")
	if err := fs.Parse(args); err != nil {
		return err
	}
	net, err := parseNetwork(*network)
	if err != nil {
		return err
	}
	cfg.Network = net

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Takes the writer lease: this spends a real cell UTXO, so it must not run
	// beside the engine.
	d, err := openDeployment(ctx, cfg, "step", logger)
	if err != nil {
		return err
	}
	defer d.release()

	positions, err := engine.DeriveTips(ctx, d.chain, d.compiled, d.facts, d.store)
	if err != nil {
		return err
	}
	if err := engine.CheckMigrationFloor(positions, d.facts.LegacyTips()); err != nil {
		return err
	}
	if *cell < 0 || *cell >= d.facts.Cells {
		return fmt.Errorf("cell %d is outside the ring of %d", *cell, d.facts.Cells)
	}
	if p := positions[*cell]; p.Halted {
		return fmt.Errorf("cell %d is halted and must not be advanced: %s", *cell, p.HaltReason)
	}

	// Report from THIS cell's tip, not the global row: cells can sit at
	// different generations and the global row would describe the wrong one.
	tip := positions[*cell].Tip
	row, err := tip.Row(d.facts.Cells)
	if err != nil {
		return err
	}
	next := d.facts.Rule.Step(row)

	fmt.Printf("cell %d, generation %d -> %d\n", *cell, tip.Generation, tip.Generation+1)
	fmt.Printf("row  %s -> %s\n", row.Hex(), next.Hex())
	fmt.Printf("bit  %v -> %v\n", row.Get(*cell), next.Get(*cell))

	// The write-ahead record, for the same reason the engine writes one: signing
	// broadcasts, so a process killed after this point has spent the output and
	// does not know it. Without the record the next start would resume from a
	// tip that is already spent. See history.StatusAttempting.
	if err := d.store.RecordTx(ctx, history.CellTx{
		Generation: tip.Generation + 1, Cell: *cell, Status: history.StatusAttempting,
	}); err != nil {
		return err
	}

	res, err := d.chain.AdvanceCell(ctx, d.compiled, tip, d.facts.Cells, d.facts.Rule)
	if err != nil {
		if errors.Is(err, chain.ErrNotBroadcast) {
			// Certainly unspent, so the record is a false alarm and must go.
			if derr := d.store.DeleteAttempt(ctx, tip.Generation+1, *cell); derr != nil {
				logger.Error("retract write-ahead record", "err", derr)
			}
		}
		return err
	}

	// The store is the only record of where this cell now is; there is no state
	// file behind it.
	if err := d.store.RecordGeneration(ctx, res.Generation, res.RowHex); err != nil {
		return err
	}
	if err := d.store.RecordTx(ctx, history.CellTx{
		Generation: res.Generation, Cell: *cell,
		TxID: res.TxID, Status: history.StatusBroadcast,
	}); err != nil {
		return err
	}

	fmt.Println()
	fmt.Printf("STEP TXID: %s\n", res.TxID)
	fmt.Printf("size:      %d bytes\n", res.SizeBytes)
	return nil
}

func cmdRun(args []string) error {
	cfg := chain.DefaultConfig()
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	network := bindCommon(fs, &cfg)
	addr := fs.String("addr", envOr("RULE110_ADDR", ":8110"), "web UI listen address")
	rate := fs.Float64("rate", 1, "generations per second when running")
	start := fs.Bool("start", false, "begin advancing immediately instead of waiting for the UI")
	autoRecover := fs.Bool("auto-recover", false,
		"resolve cells whose tip is unknown automatically; off by default — run \"rule110 recover\" first")
	if err := fs.Parse(args); err != nil {
		return err
	}
	net, err := parseNetwork(*network)
	if err != nil {
		return err
	}
	cfg.Network = net

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	c, err := chain.Open(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.Background()) }()

	d, err := c.LoadDeployment()
	if err != nil {
		return fmt.Errorf("%w\n(run \"rule110 genesis\" first to create generation 0)", err)
	}
	compiled, err := cellscript.Compile(d.Cells, d.Rule)
	if err != nil {
		return err
	}

	store, err := history.Open(ctx, cfg.PostgresDSN, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	eng, err := engine.New(ctx, c, compiled, d, store, engine.Options{AutoRecover: *autoRecover}, logger)
	if err != nil {
		return err
	}

	if st, err := store.Stats(ctx); err == nil {
		fmt.Printf("history: %d generations, %d transactions recorded (%d still settling)\n",
			st.Generations, st.Txs, st.Unsettled)
	}
	eng.SetRate(*rate)
	if *start {
		eng.SetMode(engine.ModeRunning)
	}
	// Wait for the engine to drain on the way out. Run returns once every cell
	// worker has stopped at its loop top and the tips have been checkpointed;
	// exiting before that would abandon in-flight transitions in exactly the
	// window that costs a cell — see history.StatusAttempting.
	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		eng.Run(ctx)
	}()
	defer func() {
		select {
		case <-engineDone:
		case <-time.After(20 * time.Second):
			logger.Warn("engine did not drain in time; exiting anyway")
		}
	}()

	// The pool only drains on its own: throughput change deliberately does not
	// recycle back into it, so without the keeper the automaton runs until the
	// pool is empty and then starves.
	go func() {
		if err := c.RunFuelKeeper(ctx); err != nil {
			logger.Error("fuel keeper stopped", "err", err)
		}
	}()

	fmt.Printf("rule 110 · %d cells · generation %d\n", d.Cells, eng.Snapshot().Generation)
	fmt.Printf("arcade:  %s\n", cfg.ArcadeURL)
	fmt.Printf("genesis: %s\n", d.GenesisTxID)
	if basket, denom, target, on := cfg.FuelPool(); on {
		fmt.Printf("fuel:    %d x %d sat in %q, kept topped up\n", target, denom, basket)
	}
	fmt.Printf("\n  UI ready at http://localhost%s\n\n", *addr)

	return web.New(eng, logger).Serve(ctx, *addr)
}

func cmdFuel(args []string) error {
	cfg := chain.DefaultConfig()
	fs := flag.NewFlagSet("fuel", flag.ContinueOnError)
	network := bindCommon(fs, &cfg)
	count := fs.Uint64("count", 300, "how many coins to mint")
	sats := fs.Uint64("sats", 20000, "value of each coin (must cover one cell transition's fee)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	net, err := parseNetwork(*network)
	if err != nil {
		return err
	}
	cfg.Network = net

	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	c, err := chain.Open(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()

	before, err := c.ClaimableCoins(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("claimable coins before: %d\n", before)
	fmt.Printf("minting %d coins of %d sat (%d sat total)...\n", *count, *sats, *count**sats)

	results, err := c.FanOutFuel(ctx, *count, *sats)
	for _, r := range results {
		fmt.Printf("  %s  %d coins\n", r.TxID, r.Coins)
	}
	if err != nil {
		return err
	}

	after, err := c.ClaimableCoins(ctx)
	if err != nil {
		return err
	}
	balance, _ := c.Balance(ctx)
	fmt.Printf("\nclaimable coins after: %d\n", after)
	fmt.Printf("balance: %d sat\n", balance)
	return nil
}

func seedRow(cells int, hexSeed string) (ca.Row, error) {
	if hexSeed == "" {
		return ca.SeedSingle(cells)
	}
	return ca.SeedHex(cells, hexSeed)
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
