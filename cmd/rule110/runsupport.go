package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/engine"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// seedFor builds generation 0's row: a single live cell at the right-hand edge.
//
// The same seed `genesis` uses by default, and it has to be the same one, since
// the cold start now creates genesis itself. One live cell is what makes the
// familiar triangle: Rule 110 from a single seed is the interesting case, and a
// random row is the uninteresting one.
func seedFor(cells int) (ca.Row, error) {
	row, err := ca.NewRow(cells)
	if err != nil {
		return ca.Row{}, err
	}
	row.Set(cells-1, true)
	return row, nil
}

// leaseHolder returns a predicate reporting whether this process holds the
// single-writer lease, keeping the claim renewed in the background.
//
// The cold start needs this for the same reason the engine does, and needs it
// EARLIER: two pods with an empty volume each would otherwise both mint fuel
// and both create a genesis, on one wallet, spending twice and orphaning one
// ring. The engine's own holdLease continues the same claim afterwards without
// a gap, because both use engine.InstanceOwner.
//
// A store error reads as "not the leader". Refusing to spend because the
// database is unreachable is the safe direction: the alternative is spending on
// the assumption that nobody else is.
func leaseHolder(ctx context.Context, store *history.Store, logger *slog.Logger) func() bool {
	owner := engine.InstanceOwner()

	held := func() bool {
		ok, err := store.AcquireLease(ctx, engine.LeaseName, owner, engine.LeaseTTL)
		if err != nil {
			logger.WarnContext(ctx, "lease check failed; not bootstrapping", "err", err)
			return false
		}
		return ok
	}

	// Renew in the background: the bootstrap can sit in the funding phase for
	// hours waiting for a stranger to pay, and a lapsed lease there would let a
	// second pod start spending while this one is still waiting to.
	go func() {
		ticker := time.NewTicker(engine.LeaseRenew)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = store.AcquireLease(ctx, engine.LeaseName, owner, engine.LeaseTTL)
			}
		}
	}()

	return held
}

// printBanner says what this process is, before it knows whether it has
// anything to run.
//
// Printed BEFORE the cold start rather than after, because the cold start can
// block indefinitely waiting for someone to send coin, and a process that has
// printed nothing looks hung rather than patient.
func printBanner(cfg chain.Config, compiled *cellscript.Compiled, addr string) {
	fmt.Printf("rule %d · %d cells\n", compiled.Rule(), compiled.Cells())
	fmt.Printf("arcade:  %s (%s)\n", cfg.ArcadeURL, cfg.Network)
	if basket, denom, target, on := cfg.FuelPool(); on {
		fmt.Printf("fuel:    %d x %d sat in %q, kept topped up\n", target, denom, basket)
	}
	if cfg.LockControls {
		fmt.Println("controls: LOCKED — play, pause, step and rate are refused")
	}
	if cfg.PublicFunding {
		fmt.Printf("funding: PUBLIC — minimum %d sat\n", cfg.MinPaymentSatoshis)
	}
	fmt.Printf("\n  UI ready at %s\n\n", uiURL(addr))
}
