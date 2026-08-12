package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dymurray/rule-110-arcade/internal/chain"
)

// cmdPrune reclaims the wallet payload that a months-long run would otherwise
// accumulate forever.
//
// It is a separate process rather than another goroutine inside `run` for two
// reasons. It deletes data from a wallet with no backup, so it must be startable
// and stoppable without touching the automaton — nobody should have to restart
// the engine to turn pruning off. And it is the only part of this program whose
// blast radius is measured in gigabytes, so it should not share a lifetime with
// anything that might be restarted casually.
//
// It does NOT take the engine's writer lease. The lease guards the cell UTXOs
// against two writers double-spending them; this touches no UTXO, spends
// nothing, and is designed to run alongside a live engine.
//
// Dry run is the default and -apply is the only way past it. Run the dry run
// first, read the numbers, and only then apply.
func cmdPrune(args []string) error {
	cfg := chain.DefaultConfig()
	opts := chain.DefaultPruneOptions()

	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	network := bindCommon(fs, &cfg)
	apply := fs.Bool("apply", false,
		"actually write; without this the sweep only reports what it would drop")
	watch := fs.Bool("watch", false,
		"keep sweeping until interrupted, instead of making one pass")
	fs.Uint64Var(&opts.RetainGenerations, "retain-generations", opts.RetainGenerations,
		"never touch transactions from the newest N generations")
	fs.Uint64Var(&opts.MinConfirmations, "min-confirmations", opts.MinConfirmations,
		"how deep the spending transaction must be buried before its parent is pruned")
	fs.IntVar(&opts.BatchSize, "batch", opts.BatchSize, "rows per statement")
	fs.DurationVar(&opts.Pause, "pause", opts.Pause, "gap between batches")
	fs.DurationVar(&opts.Interval, "interval", opts.Interval, "gap between passes under -watch")
	fs.IntVar(&opts.MaxBatches, "max-batches", opts.MaxBatches, "stop a pass after N batches (0 = no limit)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	net, err := parseNetwork(*network)
	if err != nil {
		return err
	}
	cfg.Network = net
	opts.DryRun = !*apply

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	p, err := chain.OpenPruner(ctx, cfg, opts, logger)
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()

	if *watch {
		if opts.DryRun {
			fmt.Println("mode:           DRY RUN (nothing will be written; pass -apply to prune)")
		}
		p.Run(ctx)
		return nil
	}

	rep, err := p.Sweep(ctx)
	if err != nil {
		return err
	}

	mode := "DRY RUN — nothing was written"
	if !opts.DryRun {
		mode = "APPLIED"
	}
	fmt.Printf("mode:            %s\n", mode)
	fmt.Printf("tip height:      %d\n", rep.TipHeight)
	if rep.FloorTxID == 0 {
		fmt.Printf("retention floor: not established — the automaton has not run past "+
			"%d generations, so nothing is eligible\n", opts.RetainGenerations)
	} else {
		fmt.Printf("retention floor: transaction_id %d (generation %d, retaining %d)\n",
			rep.FloorTxID, rep.FloorGeneration, opts.RetainGenerations)
	}
	fmt.Printf("scanned:         %d mined transactions\n", rep.Scanned)
	fmt.Printf("prunable:        %d\n", rep.Prunable)
	if !opts.DryRun {
		fmt.Printf("pruned:          %d\n", rep.Pruned)
	}
	// Reported as the uncompressed length, which is what LENGTH() returns.
	// PostgreSQL stores these ~3.7x smaller after TOAST compression, so the
	// disk saving on the live deployment is roughly a quarter of this.
	fmt.Printf("raw_tx bytes:    %d (uncompressed; ~%d on disk after TOAST)\n",
		rep.Bytes, rep.Bytes*1115/4082)
	return nil
}
