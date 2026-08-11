package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"

	"github.com/dymurray/rule-110-arcade/internal/cellscript"
	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// cmdDepthProbe measures how deep an unbroken chain of unconfirmed transactions
// this network will actually accept.
//
// Every cell is such a chain, so this number is the automaton's real speed
// limit: depth grows at the generation rate and only falls when a block lands.
// Nodes have historically capped mempool ancestors, and past the cap the
// deepest transaction is rejected and the rejection cascades to every
// descendant — which for us destroys a cell rather than costing a step.
//
// The trouble is that nobody could tell us the number. The toolbox benchmarks
// record a cascade attributed to an ancestor limit; arcade's own
// LimitAncestorCount is a dead value with no setter. So this measures it
// instead of assuming, on ONE cell, because if the limit is real the probe
// destroys the chain it is probing and one cell is a survivable price.
//
// Run it against any new network before choosing -max-depth.
func cmdDepthProbe(args []string) error {
	cfg := chain.DefaultConfig()
	fs := flag.NewFlagSet("depth-probe", flag.ContinueOnError)
	network := bindCommon(fs, &cfg)
	cell := fs.Int("cell", 0, "which cell to build the chain on")
	maxDepth := fs.Int("max", 1000, "give up (declaring no limit found) at this depth")
	if err := fs.Parse(args); err != nil {
		return err
	}
	net, err := parseNetwork(*network)
	if err != nil {
		return err
	}
	cfg.Network = net
	// The whole point is to find the limit, so the governor must be off.
	cfg.MaxUnconfirmedDepth = 0

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	c, err := chain.Open(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(ctx) }()

	store, err := history.Open(ctx, cfg.PostgresDSN, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// Refuse to run against a live engine. Two writers would double-spend this
	// cell, and the resulting rejection would look exactly like the limit we are
	// trying to measure — the worst possible way to be wrong.
	owner := fmt.Sprintf("depth-probe-%d", os.Getpid())
	held, err := store.AcquireLease(ctx, "rule110-engine", owner, 10*time.Minute)
	if err != nil {
		return err
	}
	if !held {
		return fmt.Errorf("the engine holds the writer lease; stop it before probing")
	}
	defer func() { _ = store.ReleaseLease(context.WithoutCancel(ctx), "rule110-engine", owner) }()

	state, err := c.LoadState()
	if err != nil {
		return err
	}
	compiled, err := cellscript.Compile(state.Cells, state.Rule)
	if err != nil {
		return err
	}

	tip := state.Chains[*cell]
	fmt.Printf("probing cell %d from generation %d, up to %d deep\n\n",
		*cell, tip.Generation, *maxDepth)

	// Persisting each tip as we go is not optional: abandoning a broadcast
	// transition without recording it is precisely the crash that costs a cell.
	save := func() {
		state.Chains[*cell] = tip
		if err := c.SaveState(state); err != nil {
			logger.Error("save state", "err", err)
		}
	}
	defer save()

	var built []string
	start := time.Now()

	for depth := 1; depth <= *maxDepth; depth++ {
		if ctx.Err() != nil {
			break
		}
		if err := store.RecordTx(ctx, history.CellTx{
			Generation: tip.Generation + 1, Cell: *cell, Status: history.StatusAttempting,
		}); err != nil {
			return err
		}

		res, err := c.AdvanceCell(ctx, compiled, tip, state.Cells, state.Rule)
		if err != nil {
			fmt.Printf("\nstopped at depth %d: %v\n", depth, err)
			break
		}
		tip = chain.CellChain{
			Cell: *cell, TxID: res.TxID, Vout: 0, Satoshis: tip.Satoshis,
			Generation: res.Generation, RowHex: res.RowHex, RawTxHex: res.RawTxHex,
		}
		built = append(built, res.TxID)
		if err := store.RecordTx(ctx, history.CellTx{
			Generation: res.Generation, Cell: *cell,
			TxID: res.TxID, Status: history.StatusBroadcast,
		}); err != nil {
			return err
		}

		// Checkpoint periodically rather than every step: an interrupt must not
		// leave the cell's tip further ahead than the file says.
		if depth%25 == 0 {
			save()
			unconfirmed, rejectedAt, reason := probeStatuses(ctx, c.Oracle, built)
			fmt.Printf("depth %4d  unconfirmed %4d  %.1f tx/s\n",
				depth, unconfirmed, float64(depth)/time.Since(start).Seconds())
			if rejectedAt >= 0 {
				fmt.Printf("\nREJECTED at chain position %d: %s\n", rejectedAt+1, reason)
				fmt.Printf("deepest unconfirmed run reached: %d\n", unconfirmed)
				return nil
			}
		}
	}

	save()
	// Give the last broadcasts a moment to be judged before the verdict.
	fmt.Print("\nsettling")
	for range 6 {
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
		fmt.Print(".")
	}
	fmt.Println()

	if len(built) == 0 {
		// Say nothing about limits. A probe that built nothing has measured
		// nothing, and reporting "no limit found" here would be a lie that reads
		// like a result.
		return fmt.Errorf("built no transactions, so nothing was measured — " +
			"check the fuel pool has claimable coins")
	}

	unconfirmed, rejectedAt, reason := probeStatuses(ctx, c.Oracle, built)
	if rejectedAt >= 0 {
		fmt.Printf("\nREJECTED at chain position %d: %s\n", rejectedAt+1, reason)
	} else {
		fmt.Printf("\nno rejection in %d transactions; deepest unconfirmed run %d\n",
			len(built), unconfirmed)
		fmt.Printf("no ancestor limit was reached at depth %d on this network\n", unconfirmed)
	}
	return nil
}

// probeSweep is how far back from the tip a status sample reaches.
//
// It is bounded because the sweep is otherwise O(n) per sample and O(n²) over a
// run, and at a few hundred deep that dominates the measurement completely: an
// unbounded sweep made the probe's own throughput appear to collapse from 3.4
// to 0.9 tx/s, which reads exactly like the network degrading with chain depth
// and is nothing of the sort. Walking back from the tip still finds the newest
// mined transaction and any rejection near the frontier, which is all the
// measurement needs.
const probeSweep = 60

// probeStatuses samples the tail of the chain, returning the run of consecutive
// unmined transactions at the tip, the position of the first rejection found
// (or -1), and its reason.
//
// The unconfirmed RUN is the number that matters, not the total built: a block
// landing mid-probe confirms a prefix of the chain and resets the ancestry the
// node is actually tracking.
func probeStatuses(ctx context.Context, oracle arcade.TxOracle, txids []string) (unconfirmed, rejectedAt int, reason string) {
	rejectedAt = -1
	from := max(0, len(txids)-probeSweep)
	lastMined := from - 1

	for i := from; i < len(txids); i++ {
		rec, err := oracle.GetTx(ctx, txids[i])
		if err != nil || rec == nil {
			continue
		}
		switch rec.Status {
		case arcade.StatusMined, arcade.StatusImmutable:
			lastMined = i
		case arcade.StatusRejected, arcade.StatusDoubleSpendAttempted:
			if rejectedAt < 0 {
				rejectedAt = i
				reason = string(rec.Status)
				if rec.ExtraInfo != "" {
					reason += ": " + rec.ExtraInfo
				}
			}
		}
	}
	// Nothing in the sampled tail was mined, so the unconfirmed run is at least
	// the sample width — report that floor rather than implying more precision.
	return len(txids) - 1 - lastMined, rejectedAt, reason
}
