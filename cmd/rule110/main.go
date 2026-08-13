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
//
// `run` brings an empty deployment up by itself: it serves the UI, shows a
// funding address, and creates generation 0 once somebody pays it. The other
// subcommands are the same sequence done by hand, and remain the way to
// internalize a payment that is already on chain, to audit, and to recover.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/defs"

	"github.com/dymurray/rule-110-arcade/internal/boot"
	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/engine"
	"github.com/dymurray/rule-110-arcade/internal/history"
	"github.com/dymurray/rule-110-arcade/internal/web"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		// `rule110 run -h` asked for the flag list and the flag package has
		// already printed it. Reporting that as an error, and exiting non-zero,
		// made the one command that documents a subcommand look broken.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
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
	case "audit":
		return cmdAudit(args[1:])
	case "recover":
		return cmdRecover(args[1:])
	case "import-tips":
		return cmdImportTips(args[1:])
	case "depth-probe":
		return cmdDepthProbe(args[1:])
	case "prune":
		return cmdPrune(args[1:])
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

Usage: rule110 <subcommand> [flags]

Starting a new deployment takes one command:

  rule110 run -arcade-url <url> -public-funding

An empty data directory is not an error. run serves the UI, shows its funding
address, and mints fuel and creates generation 0 by itself once a payment lands.
With -public-funding the page carries a Fund button any BRC-100 wallet can use.

The same sequence by hand, which is still what to reach for when a payment is
already on chain:

  1  rule110 address                 print the funding address (offline; needs no arcade)
  2  send coin to that address       from any wallet
  3  wait for that payment to be MINED  (this path only: a raw transaction
                                     carries no ancestry, so nothing but a
                                     merkle proof shows it is real)
  4  rule110 fund -tx <hex> -bump <hex>
                                     internalize it; BOTH flags are required
  5  rule110 fuel                    mint the coins one generation fans out across
  6  rule110 genesis                 create generation 0: one UTXO per cell
  7  rule110 run                     start the automaton and its web UI

Order matters at one place in particular: fuel BEFORE genesis. Under the
throughput strategy genesis is funded from the pool, so creating it first finds
an empty pool and fails against a wallet that plainly holds money.

Also:
  rule110 audit                      re-derive the recorded history and check it against
                                     the transactions on chain — including the cross-cell
                                     agreement Script does NOT enforce (read-only; safe to
                                     run while the automaton is running)
  rule110 step -cell N               advance a single cell by hand
  rule110 depth-probe                measure how deep an unconfirmed chain this
                                     network accepts (destroys the cell it probes)
  rule110 recover                    resolve cells whose tip is unknown after an
                                     unclean shutdown, whose tip was spent by a
                                     transition we lost the record of, or that a
                                     local failure halted without anything ever
                                     reaching the network (dry run unless -apply)
  rule110 import-tips                backfill the history store from a legacy
                                     state.json (dry run unless -apply)
  rule110 prune                      reclaim wallet payload that can no longer be
                                     spent (dry run unless -apply)
  rule110 help                       show this message

Run "rule110 <subcommand> -h" for that subcommand's flags with their defaults.

Environment. Each is only the default for the matching flag, which wins:
  RULE110_ARCADE_URL       -arcade-url        arcade instance to broadcast through (required)
  RULE110_CHAINTRACKS_URL  -chaintracks-url   headers service; empty derives it from -arcade-url
  RULE110_EVENTS_URL       -events-url        arcade status stream; empty uses -arcade-url. Point it
                                              at the sse service directly inside a cluster
  RULE110_LOCK_CONTROLS    -lock-controls     refuse play/pause/step/rate; /api/control is
                                              unauthenticated, so this is the lock
  RULE110_PUBLIC_FUNDING   -public-funding    expose /api/funding and /api/fund
  RULE110_NETWORK          -network           main | test | ttn | tstn  (default tstn)
  RULE110_DATA_DIR         -data-dir          wallet database and key file — this IS the wallet
  RULE110_POSTGRES_DSN     -postgres-dsn      storage DSN; empty uses SQLite, which is much
                                              slower under a full generation's fan-out
  RULE110_ADDR             -addr              web UI listen address (run only)

Flags every subcommand accepts, beyond the five above:
  -originator string       BRC-100 originator, FQDN-shaped
  -cell-sats uint          satoshis each cell UTXO carries
  -max-db-conns int        storage connection pool size
  -apply-concurrency int   monitor workers applying arcade status batches
  -full-status             subscribe to every status transition (~4x the events)
  -chronicle               verify with Chronicle-era script rules; required, because the
                           covenant contains OP_2MUL (default true)
  -fee-sat-per-kb int      fee rate, above arcade's 100 sat/kB floor
  -min-broadcast-fee-rate int
                           refuse a finished transaction below this sat/kB (0 skips)
  -throughput              fund from a denominated fuel pool instead of contending for change
  -fuel-sats uint          value of one fuel coin
  -fuel-pool uint          how many fuel coins the keeper maintains
  -max-depth uint          how far a cell may run ahead of its newest MINED transaction
                           (0 = unbounded); measure it with depth-probe, do not assume it
  -max-unseen uint         how far a cell may run ahead of its newest ACCEPTED transaction
                           (0 = unbounded). 1, the default, means a cell waits for its
                           parent to land before building on it — arcade's 202 is
                           "accepted for processing", not "in a mempool"
  -max-lag uint            how far the clock may run ahead of the slowest cell

Subcommand flags:
  fund         -tx, -bump (both required), -vout, -description
  fuel         -count, -sats
  genesis      -cells, -rule, -seed
  step         -cell
  run          -addr, -rate, -start, -auto-recover
  depth-probe  -cell, -max
  audit        -from, -to, -last, -max-failures, -gaps

The ring size is fixed at genesis, so -cells is a genesis flag only: run, step and
depth-probe read it back out of the recorded state.
`)
}

// bindCommon registers the flags every subcommand shares.
func bindCommon(fs *flag.FlagSet, cfg *chain.Config) *string {
	fs.StringVar(&cfg.ArcadeURL, "arcade-url", envOr("RULE110_ARCADE_URL", ""),
		"arcade instance to broadcast through (required)")
	fs.StringVar(&cfg.ChainTracksURL, "chaintracks-url", envOr("RULE110_CHAINTRACKS_URL", ""),
		"headers service; empty derives it from the arcade URL")
	fs.StringVar(&cfg.EventsURL, "events-url", envOr("RULE110_EVENTS_URL", ""),
		"arcade status stream; empty uses the arcade URL. Point it at the sse service directly in-cluster")
	fs.StringVar(&cfg.DataDir, "data-dir", envOr("RULE110_DATA_DIR", cfg.DataDir),
		"wallet database and key file — losing this loses the coins")
	fs.StringVar(&cfg.Originator, "originator", cfg.Originator, "BRC-100 originator (FQDN-shaped)")
	// -cells is deliberately NOT here. The ring size is fixed when genesis
	// creates the UTXOs, and run/step/depth-probe read it back from the recorded
	// state; offering the flag on those made it look adjustable, which it is not.
	// cmdGenesis binds it.
	fs.Uint64Var(&cfg.CellSatoshis, "cell-sats", cfg.CellSatoshis, "satoshis each cell UTXO carries")
	fs.StringVar(&cfg.PostgresDSN, "postgres-dsn", envOr("RULE110_POSTGRES_DSN", ""),
		"PostgreSQL DSN; empty uses SQLite (much slower under a wide fan-out)")
	fs.IntVar(&cfg.MaxDBConns, "max-db-conns", cfg.MaxDBConns, "storage connection pool size")
	fs.BoolVar(&cfg.Chronicle, "chronicle", cfg.Chronicle,
		"verify with Chronicle-era script rules (required for Rúnar covenants)")
	// The old help text said the 100 sat/kB floor is applied to the
	// extended-format size. Config.FeeSatPerKB retracts that in as many words —
	// the validator treats the inline prevouts as spent-coin data and does not
	// bill them. The margin is headroom for a fee committed from a size estimate
	// made before the ~2.6 kB unlocking scripts exist, which is a different
	// hazard, and the help text should not keep asserting the withdrawn one.
	fs.Int64Var(&cfg.FeeSatPerKB, "fee-sat-per-kb", cfg.FeeSatPerKB,
		"fee rate, above arcade's 100 sat/kB floor; the margin is headroom for the pre-signing size estimate")
	fs.Int64Var(&cfg.MinBroadcastFeeRate, "min-broadcast-fee-rate", cfg.MinBroadcastFeeRate,
		"reject a finished transaction below this sat/kB (0 skips the check); set it to the floor the receiving arcade enforces")
	fs.BoolVar(&cfg.Throughput, "throughput", cfg.Throughput,
		"fund from a denominated fuel pool instead of contending for change")
	fs.Uint64Var(&cfg.FuelDenomination, "fuel-sats", cfg.FuelDenomination,
		"value of one fuel coin; must clear one transition's fee plus the dust floor")
	fs.Uint64Var(&cfg.FuelPoolSize, "fuel-pool", cfg.FuelPoolSize,
		"how many fuel coins the keeper maintains")
	fs.Uint64Var(&cfg.MaxUnconfirmedDepth, "max-depth", cfg.MaxUnconfirmedDepth,
		"how far a cell may run ahead of its newest mined transaction (0 = unbounded)")
	fs.Uint64Var(&cfg.MaxUnseenDepth, "max-unseen", cfg.MaxUnseenDepth,
		"how far a cell may run ahead of its newest ACCEPTED transaction (0 = unbounded); 1 means a cell waits for its parent to land")
	fs.Uint64Var(&cfg.MaxLag, "max-lag", cfg.MaxLag,
		"how far the clock may run ahead of the slowest cell")
	fs.IntVar(&cfg.ApplyConcurrency, "apply-concurrency", cfg.ApplyConcurrency,
		"monitor workers applying arcade status batches")
	fs.BoolVar(&cfg.FullStatusUpdates, "full-status", cfg.FullStatusUpdates,
		"subscribe to every status transition (~4x the events; turn off above ~3 gen/s)")
	fs.BoolVar(&cfg.LockControls, "lock-controls", envBool("RULE110_LOCK_CONTROLS", cfg.LockControls),
		"refuse every play/pause/step/rate request; /api/control is unauthenticated, so this is the lock")
	fs.BoolVar(&cfg.PublicFunding, "public-funding", envBool("RULE110_PUBLIC_FUNDING", cfg.PublicFunding),
		"expose /api/funding and /api/fund so any BRC-100 wallet can pay this deployment's costs")
	fs.Uint64Var(&cfg.MinPaymentSatoshis, "min-payment", cfg.MinPaymentSatoshis,
		"smallest public payment worth accepting; 0 derives it from -fuel-sats")
	fs.Uint64Var(&cfg.FirstFuelCoins, "first-fuel", cfg.FirstFuelCoins,
		"fuel coins the cold-start bootstrap mints before genesis; 0 derives it from the ring size")

	network := fs.String("network", envOr("RULE110_NETWORK", string(defs.NetworkTSTN)),
		"main | test | ttn (Teranode test net) | tstn (private scaling test net)")
	return network
}

func cmdAddress(args []string) error {
	cfg := chain.DefaultConfig()
	fs := flag.NewFlagSet("address", flag.ContinueOnError)
	network := bindCommon(fs, &cfg)
	// No -rule here: the address is derived from the identity key and the
	// network, and nothing else. It used to accept -rule and discard it.
	if err := fs.Parse(args); err != nil {
		return err
	}

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
	fmt.Printf("  rule110 fund -tx <tx-hex> -bump <bump-hex> -data-dir %s\n", cfg.DataDir)
	fmt.Println()
	fmt.Println("The funding transaction must be MINED before it can be internalized:")
	fmt.Println("the wallet verifies the merkle proof in -bump against the headers")
	fmt.Println("service, so -bump is required and a mempool transaction will not do.")

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
	// Only genesis can choose the ring size: it is what creates the UTXOs, and
	// every later subcommand reads the size back out of the recorded state.
	fs.IntVar(&cfg.Cells, "cells", cfg.Cells, "ring size (multiple of 8); fixed here for the life of the deployment")
	rule := fs.Uint("rule", uint(cfg.Rule), "Wolfram rule number (0-255)")
	seedHex := fs.String("seed", "", "initial row as hex; empty seeds a single live cell at index 0")
	if err := fs.Parse(args); err != nil {
		return err
	}
	parsedRule, err := parseRule(*rule)
	if err != nil {
		return err
	}
	cfg.Rule = parsedRule

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
	if err := engine.CheckMigrationFloor(ctx, d.store, d.chain.Oracle, positions, d.facts.LegacyTips()); err != nil {
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

	// Take the VALIDATED configuration back.
	//
	// Open receives a Config by value and validates its own copy, and Validate
	// is not only a check — it fills in the settings derived from others
	// (MinPaymentSatoshis from the fuel denomination, FirstFuelCoins from the
	// ring size) and normalises the arcade URL. Reading those off the local
	// copy gets zeroes, and a zero here is silent in the worst way: the cold
	// start would ask the fan-out for no coins and advertise a minimum payment
	// of nothing.
	cfg = c.Config

	store, err := history.Open(ctx, cfg.PostgresDSN, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// The ring's shape comes from the deployment when there is one, and from the
	// configuration when there is not. Only the first case is a fact; the second
	// is a proposal, which the cold start is about to turn into one.
	cells, rule := cfg.Cells, cfg.Rule
	if existing, lerr := c.LoadDeployment(); lerr == nil {
		cells, rule = existing.Cells, existing.Rule
	}
	compiled, err := cellscript.Compile(cells, rule)
	if err != nil {
		return err
	}
	seed, err := seedFor(cells)
	if err != nil {
		return err
	}
	genesisSize, err := chain.GenesisBytes(compiled, seed)
	if err != nil {
		return err
	}

	// Serve the UI BEFORE there is anything to show.
	//
	// This is the whole point of the cold start being in-process: a fresh volume
	// has no coin, and the only way to get some is to ask — which needs a page,
	// which needs a server. `run` used to fail here telling the operator to go
	// run three other subcommands, which a container simply restarts and says
	// again.
	boots := boot.New(c, compiled, boot.Options{
		Seed:             seed,
		Throughput:       cfg.Throughput,
		FuelDenomination: cfg.FuelDenomination,
		FirstFuelCoins:   cfg.FirstFuelCoins,
		MinBootstrap:     cfg.BootstrapMinimum(genesisSize),
		Network:          string(cfg.Network),
		LockControls:     cfg.LockControls,
		Leader:           leaseHolder(ctx, store, logger),
	}, logger)

	var webOpts []web.Option
	if cfg.PublicFunding {
		webOpts = append(webOpts, web.WithFunder(newFunder(c, cfg.BootstrapMinimum(genesisSize))))
	}
	srv := web.New(boots, logger, webOpts...)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, *addr) }()

	printBanner(cfg, compiled, *addr)

	d, err := boots.Run(ctx)
	if err != nil {
		// A cancelled context on the way up is a clean shutdown, not a failure.
		if ctx.Err() != nil {
			return <-serveErr
		}
		return err
	}

	// The cold start may have created a ring of a different shape than the one
	// compiled above — it cannot, today, but the deployment is the authority and
	// compiling against anything else would bind cells to the wrong script.
	if d.Cells != cells || d.Rule != rule {
		if compiled, err = cellscript.Compile(d.Cells, d.Rule); err != nil {
			return err
		}
	}

	// The clock's starting state is configuration, not a command: -lock-controls
	// turns SetRate and SetMode into no-ops, so setting the rate by calling the
	// mutator the lock disables would leave a locked deployment stuck at the
	// default, paused, with no way to start it.
	eng, err := engine.New(ctx, c, compiled, d, store, engine.Options{
		AutoRecover:  *autoRecover,
		LockControls: cfg.LockControls,
		Rate:         *rate,
		Start:        *start,
	}, logger)
	if err != nil {
		return err
	}
	// Hand every reader over to the engine. Anyone streaming during the cold
	// start is holding the bootstrapper's change channel, so Adopt notifies once
	// — without it they would block on a channel nothing closes again.
	boots.Adopt(eng)

	if st, err := store.Stats(ctx); err == nil {
		fmt.Printf("history: %d generations, %d transactions recorded (%d still settling)\n",
			st.Generations, st.Txs, st.Unsettled)
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

	fmt.Printf("running · %d cells · generation %d\n", d.Cells, eng.Snapshot().Generation)
	fmt.Printf("genesis: %s\n", d.GenesisTxID)

	// The server is already up; block on it, as this function used to.
	return <-serveErr
}

func cmdFuel(args []string) error {
	cfg := chain.DefaultConfig()
	fs := flag.NewFlagSet("fuel", flag.ContinueOnError)
	network := bindCommon(fs, &cfg)
	// Defaults track the pool configuration rather than literals. They used to be
	// 20000 and 300 against a denomination of 1000, so plain `rule110 fuel`
	// minted coins the funder could never claim — see fuelCoinValue.
	count := fs.Uint64("count", cfg.FuelPoolSize, "how many coins to mint")
	sats := fs.Uint64("sats", 0, "value of each coin; 0 uses the configured fuel denomination")
	if err := fs.Parse(args); err != nil {
		return err
	}
	net, err := parseNetwork(*network)
	if err != nil {
		return err
	}
	cfg.Network = net

	// A coin whose value is not the pool denomination is invisible to the funder.
	// Under the throughput strategy claiming is `... AND satoshis = <denomination>`
	// (utxostore ClaimExact) — an exact match, not a minimum — so minting at any
	// other value produces coins that sit in the pool basket forever. The symptom
	// is the worst kind: the mint succeeds, the balance looks healthy, and every
	// transition reports "not enough funds" until someone works out why.
	value, err := fuelCoinValue(cfg, *sats)
	if err != nil {
		return err
	}

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
	fmt.Printf("minting %d coins of %d sat (%d sat total)...\n", *count, value, *count*value)

	results, err := c.FanOutFuel(ctx, *count, value)
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

// uiURL renders a listen address as a URL that can actually be opened.
//
// The previous version printed "http://localhost" concatenated with the raw
// address, which is right for the ":8110" default and wrong for every other
// value: RULE110_ADDR=0.0.0.0:8110 — the setting a container uses — came out as
// "http://localhost0.0.0.0:8110".
//
// A wildcard host is not reachable as written, so it becomes localhost; a host
// that was named explicitly is kept, since that is where the operator asked the
// server to be.
func uiURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func seedRow(cells int, hexSeed string) (ca.Row, error) {
	if hexSeed == "" {
		return ca.SeedSingle(cells)
	}
	return ca.SeedHex(cells, hexSeed)
}

// fuelCoinValue resolves what `rule110 fuel` mints. sats is the -sats flag,
// with 0 meaning "whatever the pool is denominated in".
//
// This flag used to default to 20000 against a 1000 satoshi denomination, and
// the two are not interchangeable. Under the throughput strategy the funder's
// fast path is utxostore's ClaimExact, which matches the denomination EXACTLY:
// a 20000 satoshi coin sitting in the fuel basket is invisible to it. Such a
// coin is not quite dead — the bounded tiered walk that backs up a short pool
// will eventually pick it — but that walk is the contended selection the whole
// strategy exists to escape, and it then spends twenty transitions' worth of
// fuel on one transition and returns the change to the CHANGE basket, not the
// pool. Meanwhile the keeper measures inventory as pool-balance ÷ denomination,
// so it reads those coins as twenty times the fuel they can actually buy and
// idles while the automaton crawls.
//
// fuel.go's FanOutFuel already documents this failure ("the coins exist, the
// balance looks healthy, and every transition still reports not enough funds").
// A flag default put it back, so a mismatch is refused here rather than
// documented a third time.
func fuelCoinValue(cfg chain.Config, sats uint64) (uint64, error) {
	_, denomination, _, throughput := cfg.FuelPool()
	if !throughput {
		// No pool, so nothing to agree with: the funder claims from ordinary
		// change and any value will do. There is no default to fall back on.
		if sats == 0 {
			return 0, fmt.Errorf("-sats is required with -throughput=false: " +
				"there is no fuel denomination to take the value from")
		}
		return sats, nil
	}
	if sats == 0 || sats == denomination {
		return denomination, nil
	}
	return 0, fmt.Errorf("-sats %d does not match the fuel denomination %d\n"+
		"the throughput funder claims fuel by exact value, so coins of any other size "+
		"never reach its fast path; pass -fuel-sats %d to mint at that denomination, "+
		"or drop -sats to use the configured one", sats, denomination, sats)
}

// parseRule bounds a Wolfram rule number.
//
// ca.Rule is a uint8 because that is precisely what an elementary rule is: a
// truth table over eight neighbourhoods, one bit each. But the flag is parsed
// as a uint, so an unchecked conversion silently took -rule 300 down to rule 44
// and ran a different automaton without a word.
func parseRule(n uint) (ca.Rule, error) {
	if n > 255 {
		return 0, fmt.Errorf("rule must be 0-255 — an elementary rule is one byte — got %d "+
			"(which would silently become rule %d)", n, ca.Rule(n))
	}
	return ca.Rule(n), nil
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

// envBool is envOr for a flag.
//
// An unparseable value falls back rather than failing, because these come from
// a Kubernetes ConfigMap where the difference between "true" and "True" should
// not crash-loop a pod — strconv.ParseBool accepts both, along with 1/0/t/f.
func envBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}
