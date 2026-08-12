package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dymurray/rule-110-arcade/internal/audit"
	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// cmdAudit checks that the recorded history really is Rule 110, and that the
// transactions on chain are what the record says they are.
//
// It exists because the project's headline claim is only half enforced. Each
// cell's covenant proves its own bit follows from the neighbourhood it CLAIMS to
// have read, and nothing on chain compares that claim with what the neighbours
// actually did — the README calls this out as an auditable invariant rather than
// a script-enforced one. Until this command there was nothing that audited it.
//
// It does NOT take the engine's writer lease, and it must not. The lease exists
// to stop two writers double-spending 128 live UTXO chains; this spends nothing,
// signs nothing and writes nothing, and an audit that could only run with the
// automaton stopped is an audit that would never be run. For the same reason it
// reads transaction bytes through chain.TxReader — a read-only connection to the
// wallet's database — instead of opening a wallet, which would start a second
// monitor daemon against the deployment it is supposed to be observing.
func cmdAudit(args []string) error {
	cfg := chain.DefaultConfig()
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	network := bindCommon(fs, &cfg)
	from := fs.Int64("from", -1, "first generation to audit; -1 derives it from -last")
	to := fs.Int64("to", -1, "last generation to audit; -1 uses the newest recorded")
	last := fs.Uint64("last", 10, "audit the newest N generations, when -from is not given")
	maxFailures := fs.Int("max-failures", 50,
		"stop once this many failures have been found (0 = no limit)")
	listGaps := fs.Int("gaps", 20, "how many gaps to list; the count is always reported")
	if err := fs.Parse(args); err != nil {
		return err
	}
	net, err := parseNetwork(*network)
	if err != nil {
		return err
	}
	cfg.Network = net
	if *last == 0 && *from < 0 {
		return fmt.Errorf("-last must be positive, or pass -from")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// The ring size and the rule come from the file genesis wrote, because they
	// are what the contract was compiled with — an audit that guessed them would
	// decode every script into nonsense and report the deployment broken.
	facts, err := chain.LoadDeploymentFrom(cfg.DataDir)
	if err != nil {
		return err
	}
	compiled, err := cellscript.Compile(facts.Cells, facts.Rule)
	if err != nil {
		return err
	}
	seed, err := facts.Seed()
	if err != nil {
		return err
	}

	store, err := history.Open(ctx, cfg.PostgresDSN, cfg.DataDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	source, err := chain.OpenTxReader(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()

	stats, err := store.Stats(ctx)
	if err != nil {
		return err
	}
	first, final, err := auditRange(*from, *to, *last, stats.Latest)
	if err != nil {
		return err
	}

	fmt.Printf("rule %d · %d cells · generations %d..%d (newest recorded: %d)\n",
		facts.Rule, facts.Cells, first, final, stats.Latest)
	fmt.Printf("genesis: %s\n\n", facts.GenesisTxID)

	rep, err := audit.Run(ctx, compiled, store, source, audit.Options{
		From: first, To: final, Seed: seed, MaxFailures: *maxFailures,
	})
	if err != nil {
		return err
	}
	return printAudit(rep, facts.Rule, *listGaps)
}

// auditRange resolves the flags into an inclusive range.
//
// -from and -to are absolute and win; -last is relative to the newest recorded
// generation, which is what an operator actually wants when they run this after
// a restart or a suspicious-looking UI.
func auditRange(from, to int64, last, latest uint64) (uint64, uint64, error) {
	final := latest
	if to >= 0 {
		final = uint64(to) //nolint:gosec // guarded non-negative
	}
	var first uint64
	switch {
	case from >= 0:
		first = uint64(from) //nolint:gosec // guarded non-negative
	case final+1 > last:
		first = final + 1 - last
	}
	if first > final {
		return 0, 0, fmt.Errorf(
			"generation range %d..%d is empty (the newest recorded generation is %d)", first, final, latest)
	}
	return first, final, nil
}

// printAudit renders the report and decides the exit status.
//
// Failures set the exit status; gaps do not. A gap is evidence the audit could
// not obtain — a cell still in flight, a transaction whose bytes `rule110 prune`
// has reclaimed — and failing on those would make the command useless against a
// live deployment. They are printed regardless, because "passed, having checked
// almost nothing" is the one result this command must never quietly produce.
func printAudit(rep *audit.Report, rule ca.Rule, listGaps int) error {
	for _, c := range rep.Counts() {
		fmt.Printf("  %-22s %d\n", c.Check, c.N)
	}
	fmt.Println()
	fmt.Printf("generations examined:   %d\n", rep.Generations)
	fmt.Printf("cell transactions:      %d\n", rep.Transactions)
	fmt.Printf("checks passed:          %d\n", rep.PassedTotal())
	fmt.Printf("gaps (not checked):     %d\n", len(rep.Gaps))
	fmt.Printf("failures:               %d\n", len(rep.Failures))

	if len(rep.Gaps) > 0 && listGaps > 0 {
		fmt.Println()
		for i, g := range rep.Gaps {
			if i == listGaps {
				fmt.Printf("  ... and %d more gap(s)\n", len(rep.Gaps)-listGaps)
				break
			}
			fmt.Printf("  GAP   %s\n", g)
		}
	}

	if !rep.OK() {
		fmt.Println()
		for _, f := range rep.Failures {
			fmt.Printf("  FAIL  %s\n", f)
		}
		fmt.Println()
		if rep.Truncated {
			fmt.Println("stopped at the failure limit; generations beyond this point were NOT audited")
		}
		return fmt.Errorf("audit FAILED: %d check(s) did not hold over generations %d..%d",
			len(rep.Failures), rep.From, rep.To)
	}

	fmt.Println()
	if len(rep.Gaps) > 0 {
		// Never the unqualified "PASS" line when there are gaps. A range where
		// most of the evidence was missing must not read like a range that was
		// checked and held.
		fmt.Printf("PASS — every check that could be made over generations %d..%d held, "+
			"but %d gap(s) above were NOT checked\n", rep.From, rep.To, len(rep.Gaps))
		return nil
	}
	fmt.Printf("PASS — generations %d..%d are rule %d, cell by cell and generation by generation\n",
		rep.From, rep.To, rule)
	return nil
}
