package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/engine"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// toolLeaseTTL is how long a one-shot tool holds the writer lease before it must
// renew. Short, because a tool that dies must not keep the automaton stopped for
// long; see holdToolLease for the renewal that makes a long run safe.
const toolLeaseTTL = 60 * time.Second

// deployment bundles what every tip-handling subcommand needs, with the writer
// lease already held.
type deployment struct {
	chain    *chain.Chain
	store    *history.Store
	compiled *cellscript.Compiled
	facts    *chain.Deployment
	owner    string
	release  func()
}

// openDeployment connects the wallet and the store, compiles the contract, and
// takes the writer lease.
//
// The lease is not optional for any of these. Each of them either spends a cell
// UTXO or rewrites the record the engine derives its tips from, and doing either
// while the engine is running means two writers on 128 live chains. The engine
// takes engine.LeaseName; so must everything else, or the exclusion is only
// notional.
func openDeployment(ctx context.Context, cfg chain.Config, tool string, logger *slog.Logger) (*deployment, error) {
	c, err := chain.Open(ctx, cfg, logger)
	if err != nil {
		return nil, err
	}
	closeAll := func() { _ = c.Close(context.WithoutCancel(ctx)) }

	facts, err := c.LoadDeployment()
	if err != nil {
		closeAll()
		return nil, err
	}
	compiled, err := cellscript.Compile(facts.Cells, facts.Rule)
	if err != nil {
		closeAll()
		return nil, err
	}
	store, err := history.Open(ctx, cfg.PostgresDSN, cfg.DataDir)
	if err != nil {
		closeAll()
		return nil, err
	}

	owner := fmt.Sprintf("%s-%d", tool, os.Getpid())
	held, err := store.AcquireLease(ctx, engine.LeaseName, owner, toolLeaseTTL)
	if err != nil {
		_ = store.Close()
		closeAll()
		return nil, err
	}
	if !held {
		_ = store.Close()
		closeAll()
		return nil, fmt.Errorf(
			"another writer holds the %q lease; stop the engine before running %s",
			engine.LeaseName, tool)
	}

	d := &deployment{chain: c, store: store, compiled: compiled, facts: facts, owner: owner}
	d.release = func() {
		rel := context.WithoutCancel(ctx)
		if err := store.ReleaseLease(rel, engine.LeaseName, owner); err != nil {
			logger.Error("release lease", "err", err)
		}
		_ = store.Close()
		closeAll()
	}
	return d, nil
}

// holdToolLease renews the lease in the background for as long as ctx lives.
//
// A one-shot tool that takes a long lease and never renews it silently loses
// exclusivity the moment the TTL passes — the engine could then start and
// advance the very cells the tool is still spending, and the rejection that
// follows looks exactly like whatever the tool was trying to measure. Renewing
// on a timer means the lease reflects the tool still being alive rather than a
// guess made when it started.
func (d *deployment) holdToolLease(ctx context.Context, logger *slog.Logger) {
	tick := time.NewTicker(toolLeaseTTL / 3)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		held, err := d.store.AcquireLease(ctx, engine.LeaseName, d.owner, toolLeaseTTL)
		if err != nil {
			logger.Error("renew writer lease", "err", err)
			continue
		}
		if !held {
			// Someone else has it. Say so loudly: from here on this process is no
			// longer the only writer, and anything it spends may be spent twice.
			logger.Error("LOST the writer lease mid-run; another writer may now be advancing these cells",
				"owner", d.owner)
		}
	}
}

func cmdRecover(args []string) error {
	cfg := chain.DefaultConfig()
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	network := bindCommon(fs, &cfg)
	apply := fs.Bool("apply", false,
		"write the decisions to the history store; without this it is a dry run")
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

	d, err := openDeployment(ctx, cfg, "recover", logger)
	if err != nil {
		return err
	}
	defer d.release()
	go d.holdToolLease(ctx, logger)

	positions, err := engine.DeriveTips(ctx, d.chain, d.compiled, d.facts, d.store)
	if err != nil {
		return err
	}
	if err := engine.CheckMigrationFloor(ctx, d.store, positions, d.facts.LegacyTips()); err != nil {
		return err
	}

	_, decisions, err := engine.Recover(ctx, d.chain, d.chain.Oracle, d.compiled, d.facts, d.store,
		positions, *apply)
	if err != nil {
		return err
	}
	if len(decisions) == 0 {
		fmt.Println("no cell has an unresolved transition, a tip spent out from under it, or a " +
			"rejection directly above its tip; nothing to recover")
		return nil
	}

	if !*apply {
		fmt.Println("DRY RUN — nothing has been written. Re-run with -apply to act on this.")
	}
	fmt.Println()
	for _, dec := range decisions {
		fmt.Println(engine.FormatRecovery(dec))
	}
	fmt.Println()
	fmt.Printf("%d cell(s) examined\n", len(decisions))
	return nil
}

// cmdImportTips backfills the history store from the per-cell tips a previous
// build recorded in state.json.
//
// This is the one command here that can cause a double spend, so it verifies
// before it writes and writes nothing at all by default.
//
// It exists because the migration to derived tips has a hole in it. Derivation
// reads the history store; the old build's authority was state.json. If the
// store is behind the file for any cell — an older deployment whose store was
// added later, a restored database, a store that was wiped — derivation
// concludes "no records, so generation 0" and the automaton re-spends 128
// outputs that were consumed hundreds of generations ago. CheckMigrationFloor
// refuses to start in that case; this is how the operator fixes it.
//
// Every legacy tip is checked the same way a derived one is: its bytes must hash
// to its txid, and it must carry this cell's covenant script for the row the
// seed and the rule produce at its generation. A tip that fails is REPORTED and
// not imported — the legacy file is exactly the record that was allowed to
// drift, so a claim in it that does not verify is evidence of the drift, not a
// reason to relax the check.
func cmdImportTips(args []string) error {
	cfg := chain.DefaultConfig()
	fs := flag.NewFlagSet("import-tips", flag.ContinueOnError)
	network := bindCommon(fs, &cfg)
	apply := fs.Bool("apply", false,
		"write the verified tips to the history store; without this it is a dry run")
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

	d, err := openDeployment(ctx, cfg, "import-tips", logger)
	if err != nil {
		return err
	}
	defer d.release()
	go d.holdToolLease(ctx, logger)

	legacy := d.facts.LegacyTips()
	if len(legacy) == 0 {
		fmt.Println("state.json records no per-cell tips, so there is nothing to import")
		return nil
	}
	seed, err := d.facts.Seed()
	if err != nil {
		return err
	}

	// The rows every legacy tip is checked against, recomputed from the seed.
	// The file's own row field is not consulted: it is part of the same record
	// being verified and cannot vouch for itself.
	highest := uint64(0)
	for _, t := range legacy {
		highest = max(highest, t.Generation)
	}
	rows := make([]ca.Row, highest+1)
	rows[0] = seed
	for g := uint64(1); g <= highest; g++ {
		rows[g] = d.facts.Rule.Step(rows[g-1])
	}

	type verified struct {
		tip chain.CellChain
		had uint64 // what the store already holds for this cell, if anything
	}
	var (
		ok       []verified
		rejected []string
		skipped  int
	)

	// Compare against the DERIVED position, which is the same number
	// CheckMigrationFloor refuses to start on. Reading the store's newest row
	// directly would answer a different question — that row can be an unresolved
	// attempt, which is a generation ahead of anything actually broadcast — and
	// the two answers disagreeing is how an operator ends up with a start that
	// refuses and an import that says there is nothing to do.
	//
	// The floor check itself is deliberately NOT run here. It failing is the
	// reason to run this command.
	existing, err := engine.DeriveTips(ctx, d.chain, d.compiled, d.facts, d.store)
	if err != nil {
		return err
	}

	// Ask the record which of these the network actually refused, BEFORE any of
	// the structural checks below — because every structural check passes on a
	// rejected transaction. Its bytes hash to its txid and its output 0 carries
	// exactly the covenant this cell's row demands; the only thing wrong with it
	// is that it does not exist. state.json was written by a build that advanced
	// a cell's tip on broadcast rather than on acceptance, so for every cell
	// halted by a rejection the file's tip IS the rejection. Importing one would
	// point the cell at a phantom outpoint and clear a halt that is protecting
	// it. See history.Store.RejectedTxIDs.
	txids := make([]string, 0, len(legacy))
	for _, t := range legacy {
		if t.TxID != "" {
			txids = append(txids, t.TxID)
		}
	}
	refusedByNetwork, err := d.store.RejectedTxIDs(ctx, txids)
	if err != nil {
		return err
	}

	for _, t := range legacy {
		if t.Cell < 0 || t.Cell >= d.facts.Cells || t.TxID == "" {
			continue
		}
		if refusedByNetwork[t.TxID] {
			rejected = append(rejected, fmt.Sprintf(
				"cell %d generation %d (%s): the record says the network REFUSED this transaction, so it "+
					"has no output to spend — the derived tip is correct and this cell is halted for a real reason",
				t.Cell, t.Generation, t.TxID))
			continue
		}
		if t.Generation > highest {
			rejected = append(rejected, fmt.Sprintf(
				"cell %d: generation %d is beyond anything the seed reaches", t.Cell, t.Generation))
			continue
		}
		raw, err := hex.DecodeString(t.RawTxHex)
		if err != nil || len(raw) == 0 {
			// Fall back to the wallet: an old file may carry a txid whose bytes
			// were dropped, and the wallet's copy is the same transaction or it
			// does not hash to the txid and is refused below.
			raw, err = d.chain.RawTx(ctx, t.TxID)
			if err != nil {
				rejected = append(rejected, fmt.Sprintf(
					"cell %d generation %d (%s): no usable bytes: %v", t.Cell, t.Generation, t.TxID, err))
				continue
			}
		}
		tip, err := chain.DeriveTip(d.compiled, t.Cell, t.Generation, rows[t.Generation], t.TxID, raw)
		if err != nil {
			rejected = append(rejected, fmt.Sprintf("cell %d: %v", t.Cell, err))
			continue
		}

		// Never move a cell BACKWARDS. The store is the authority now, so a cell
		// already derived at or beyond the legacy claim needs nothing — and
		// importing anyway would undo real progress the file never saw.
		have := existing[t.Cell].Tip.Generation
		if have >= tip.Generation {
			skipped++
			continue
		}
		ok = append(ok, verified{tip: tip, had: have})
	}

	fmt.Printf("legacy tips in state.json: %d\n", len(legacy))
	fmt.Printf("verified and importable:   %d\n", len(ok))
	fmt.Printf("already covered by store:  %d\n", skipped)
	fmt.Printf("refused:                   %d\n", len(rejected))
	for _, r := range rejected {
		fmt.Printf("  REFUSED  %s\n", r)
	}
	for _, v := range ok {
		fmt.Printf("  import   cell %3d  generation %d -> %d  %s:%d (%d sat)\n",
			v.tip.Cell, v.had, v.tip.Generation, v.tip.TxID, v.tip.Vout, v.tip.Satoshis)
	}

	if !*apply {
		fmt.Println("\nDRY RUN — nothing has been written. Re-run with -apply to import.")
		return nil
	}
	if len(rejected) > 0 {
		return fmt.Errorf(
			"%d legacy tip(s) did not verify; refusing to import a partial set, because a cell left behind "+
				"is a cell that will re-spend a spent output at the next start", len(rejected))
	}

	for _, v := range ok {
		if err := d.store.RecordGeneration(ctx, v.tip.Generation, v.tip.RowHex); err != nil {
			return err
		}
		// Recorded as seen, not broadcast: these transactions are old enough to
		// have been in the file, so the network has certainly had its say. The
		// reconciler will settle the true status; what matters here is that the
		// row exists and is not a write-ahead record.
		if err := d.store.RecordTx(ctx, history.CellTx{
			Generation: v.tip.Generation, Cell: v.tip.Cell,
			TxID: v.tip.TxID, Status: history.StatusSeen,
		}); err != nil {
			return err
		}
	}
	fmt.Printf("\nimported %d tip(s)\n", len(ok))
	return nil
}
