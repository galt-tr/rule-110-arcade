package engine

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

// checkpointInterval is how often tip state is written to disk.
//
// Every advance changes a tip, and at 512 transactions a second writing the
// file each time would put a synchronous megabyte-scale write in the middle of
// the hot path. The durable record of what was broadcast is the history store,
// which is written per transaction; this file only exists so a restart does not
// have to rebuild the tips from it.
const checkpointInterval = time.Second

// starvationGrace is how long a cell keeps retrying a funding shortfall before
// the automaton declares itself starved.
//
// A brief shortfall is normal — the fuel pool drains and refills, and coins
// spend a moment unclaimable between broadcast and acceptance. Treating that as
// starvation would flap. Past this, it is not a blip.
const starvationGrace = 20 * time.Second

// backpressurePause is how long a cell waits before retrying a transient
// funding shortfall. Short enough to resume promptly, long enough not to spin.
const backpressurePause = 25 * time.Millisecond

// Run starts the clock, the status pipeline, and one worker per cell.
//
// There is no per-generation barrier. Cells advance independently and a slow
// cell falls behind rather than holding every other cell up, so the automaton's
// rate stops being set by its worst cell.
func (e *Engine) Run(ctx context.Context) {
	go e.trackBalance(ctx)
	go e.watchStatus(ctx)
	go e.reconcile(ctx)
	go e.checkpoint(ctx)
	go e.clock(ctx)
	go e.holdLease(ctx)

	done := make(chan struct{})
	for cell := range e.state.Cells {
		go func(cell int) {
			defer func() { done <- struct{}{} }()
			e.runCell(ctx, cell)
		}(cell)
	}
	for range e.state.Cells {
		<-done
	}
	// Final checkpoint on the way out, so a clean shutdown never loses a tip.
	e.saveIfDirty()
}

// clock raises the generation every cell is asked to reach.
//
// It refuses to run more than MaxLag ahead of the slowest cell. Without that a
// rate the chain cannot serve would queue without limit, and the rate the UI
// reported would be a number nobody was achieving.
func (e *Engine) clock(ctx context.Context) {
	for {
		e.mu.RLock()
		mode, rate := e.mode, e.rate
		frontier, target, maxLag := e.frontierLocked(), e.target, e.chain.Config.MaxLag
		e.mu.RUnlock()

		if mode == ModeRunning && target-frontier < maxLag {
			e.mu.Lock()
			e.target++
			e.mu.Unlock()
			e.wake()
		}

		wait := time.Duration(float64(time.Second) / rate)
		if mode != ModeRunning {
			// Paused or starved: nothing to schedule, so sleep on the state
			// change instead of spinning. Step raises the target directly.
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		case <-e.Changed():
		}
	}
}

// runCell advances one cell for as long as the engine is running.
func (e *Engine) runCell(ctx context.Context, cell int) {
	for {
		if !e.awaitTurn(ctx, cell) {
			return
		}
		e.advanceCell(ctx, cell)
	}
}

// turnReady reports whether this cell should advance right now. It is the whole
// decision, with no waiting in it, so every gate can be tested directly.
func (e *Engine) turnReady(cell int) bool {
	e.mu.RLock()
	var (
		mode    = e.mode
		target  = e.target
		tip     = e.state.Chains[cell].Generation
		mined   = e.lastMined[cell]
		maxDeep = e.chain.Config.MaxUnconfirmedDepth
	)
	e.mu.RUnlock()

	if !e.isLeader() {
		// Another instance owns these chains. See holdLease.
		return false
	}

	if mode == ModeStarved {
		// Starved is not a dead end. One cell at a time retries, and a success
		// is what resumes the automaton — a direct test of the only thing that
		// matters, rather than trusting a balance reading. See clearStarvation.
		return e.claimProbe()
	}

	// Two gates, and neither depends on the mode. The target says how far we
	// have been asked to go — the clock raises it while running, Step raises it
	// once while paused, and pausing simply freezes it, which lets cells that
	// are behind finish catching up rather than stranding a half-done
	// generation. The depth gate says how fast the chain will let us, by keeping
	// this cell's unbroken run of unconfirmed transactions inside the node's
	// mempool ancestor limit.
	//
	// A zero limit means unbounded, not "stop". Reading it the other way turns
	// an unset field into an automaton that silently never advances, which is
	// the worst possible failure for a config mistake.
	if maxDeep > 0 && tip-mined >= maxDeep {
		return false
	}
	return tip < target
}

// awaitTurn blocks until this cell should advance, or until ctx ends (returning
// false).
func (e *Engine) awaitTurn(ctx context.Context, cell int) bool {
	for {
		e.mu.RLock()
		halted := e.halted[cell]
		e.mu.RUnlock()
		if halted {
			// The chain is broken; nothing this worker does can mend it.
			<-ctx.Done()
			return false
		}
		if e.turnReady(cell) {
			return true
		}

		select {
		case <-ctx.Done():
			return false
		case <-e.Changed():
		case <-time.After(250 * time.Millisecond):
			// The depth gate reopens on a status event rather than a state
			// change, so do not rely solely on Changed.
		}
	}
}

// advanceCell moves one cell forward a generation and records the result.
func (e *Engine) advanceCell(ctx context.Context, cell int) {
	e.mu.RLock()
	tip := e.state.Chains[cell]
	cells, rule := e.state.Cells, e.state.Rule
	e.mu.RUnlock()

	// Write the intent BEFORE building anything. Signing broadcasts, so there is
	// a window in which the output is spent on chain and we do not yet know it;
	// a process killed there would come back a generation behind and re-spend an
	// output that is already gone. This row is what tells the next startup that
	// the tip is unknown rather than stale. See history.StatusAttempting.
	e.persist(history.CellTx{
		Generation: tip.Generation + 1, Cell: cell, Status: history.StatusAttempting,
	})

	res, err := e.chain.AdvanceCell(ctx, e.compiled, tip, cells, rule)
	if err != nil {
		// Nothing reached the network, so the write-ahead record is a false
		// alarm and has to go: leaving it would make the next startup treat a
		// perfectly healthy cell as having an unknown tip. Shortfalls are the
		// common case, and they all land here.
		if errors.Is(err, chain.ErrNotBroadcast) {
			e.retractAttempt(tip.Generation+1, cell)
		}
		if ctx.Err() != nil {
			return
		}
		if isFundingShortfall(err) {
			e.onShortfall(ctx, cell)
			return
		}
		e.doneWaiting(cell)
		e.recordCell(tip.Generation+1, cell, nil, err)
		return
	}

	e.doneWaiting(cell)
	e.clearStarvation()
	e.ensureGeneration(ctx, res.Generation, res.RowHex)
	e.recordCell(res.Generation, cell, res, nil)
}

// ensureGeneration makes sure a generation exists in the window and the store.
//
// Cells reach a generation at different moments, so whichever gets there first
// creates it. The row it carries is the row that cell actually proved on chain,
// which is the same row for every cell: the rule is deterministic and all cells
// started from one genesis row.
func (e *Engine) ensureGeneration(ctx context.Context, number uint64, rowHex string) {
	e.mu.Lock()
	if indexOfGeneration(e.history, number) >= 0 {
		e.mu.Unlock()
		return
	}
	gen := Generation{Number: number, RowHex: rowHex, Cells: make([]CellTx, e.state.Cells)}
	for i := range gen.Cells {
		gen.Cells[i] = CellTx{Cell: i, State: TxPending}
	}
	// Generations are created in ascending order in practice, but a cell that
	// fell behind can create an older one, so keep the slice ordered.
	e.history = append(e.history, gen)
	for i := len(e.history) - 1; i > 0 && e.history[i-1].Number > e.history[i].Number; i-- {
		e.history[i-1], e.history[i] = e.history[i], e.history[i-1]
	}
	if len(e.history) > maxHistory {
		e.history = e.history[len(e.history)-maxHistory:]
	}
	e.notify()
	e.mu.Unlock()

	if err := e.store.RecordGeneration(ctx, number, rowHex); err != nil {
		e.logger.ErrorContext(ctx, "persist generation", "generation", number, "err", err)
	}
}

// frontier is the generation every cell has reached. Callers must hold the lock.
//
// With no barrier, cells sit at different generations, so the automaton's
// position is the slowest of them — the newest row every cell has proved.
func (e *Engine) frontierLocked() uint64 {
	frontier := uint64(0)
	first := true
	for i, c := range e.state.Chains {
		if e.halted[i] {
			continue
		}
		if first || c.Generation < frontier {
			frontier, first = c.Generation, false
		}
	}
	if first {
		return e.state.Generation // every cell halted; hold position
	}
	return frontier
}

// deepestLocked returns the deepest unconfirmed chain. Callers must hold the lock.
func (e *Engine) deepestLocked() uint64 {
	var deepest uint64
	for i, c := range e.state.Chains {
		if e.halted[i] {
			continue
		}
		if d := c.Generation - e.lastMined[i]; d > deepest {
			deepest = d
		}
	}
	return deepest
}

// isFundingShortfall reports whether an error is the funder saying there is no
// claimable coin, as opposed to anything else going wrong.
func isFundingShortfall(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not enough funds")
}

// onShortfall handles a cell that could not be funded.
//
// A brief shortfall is ordinary: the fuel pool drains and refills, and a coin is
// unclaimable for a moment between broadcast and acceptance. So the cell simply
// waits and tries again. Only a shortfall that persists past the grace period
// means the deployment is actually out of coin, and then the whole automaton
// says so rather than each cell failing separately.
func (e *Engine) onShortfall(ctx context.Context, cell int) {
	e.mu.Lock()
	if e.starvedSince.IsZero() {
		e.starvedSince = time.Now()
	}
	starving := time.Since(e.starvedSince)
	alreadyStarved := e.mode == ModeStarved
	e.waitingOnCoin[cell] = true
	e.notify()
	e.mu.Unlock()

	if starving > starvationGrace && !alreadyStarved {
		e.enterStarvation(ctx, cell)
	}

	select {
	case <-ctx.Done():
	case <-time.After(backpressurePause):
	}
}

// starvationProbe is how often a single cell retries while starved. Often
// enough to resume promptly after a payment lands, rare enough that a long
// outage does not fill the log or hammer the wallet.
const starvationProbe = 15 * time.Second

// claimProbe grants one cell permission to retry while starved, at most once
// per starvationProbe. It is the resume mechanism.
func (e *Engine) claimProbe() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Since(e.lastProbe) < starvationProbe {
		return false
	}
	e.lastProbe = time.Now()
	return true
}

// retractAttempt removes a write-ahead record for something that never reached
// the network, so it cannot be mistaken later for a lost broadcast.
func (e *Engine) retractAttempt(generation uint64, cell int) {
	if err := e.store.DeleteAttempt(context.Background(), generation, cell); err != nil {
		e.logger.Error("retract write-ahead record",
			"generation", generation, "cell", cell, "err", err)
	}
}

// doneWaiting clears a cell's funding-shortfall flag.
func (e *Engine) doneWaiting(cell int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.waitingOnCoin[cell] {
		delete(e.waitingOnCoin, cell)
		e.notify()
	}
}

// enterStarvation stops the clock and says so, once.
func (e *Engine) enterStarvation(ctx context.Context, cell int) {
	e.mu.Lock()
	if e.mode == ModeStarved {
		e.mu.Unlock()
		return
	}
	e.mode = ModeStarved
	e.lastError = "out of funds — waiting for a payment to " + e.fundingAddress
	addr := e.fundingAddress
	e.notify()
	e.mu.Unlock()

	// One event on the transition, not one per cell per attempt.
	e.logger.WarnContext(ctx, "automaton starved: out of funds, waiting for a payment",
		"cell", cell, "address", addr)
}

// clearStarvation resumes the automaton once coin is available again.
func (e *Engine) clearStarvation() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.starvedSince.IsZero() && e.mode != ModeStarved {
		return
	}
	e.starvedSince = time.Time{}
	if e.mode == ModeStarved {
		e.mode = ModeRunning
		e.lastError = ""
		e.logger.Info("automaton resumed: funding restored")
	}
	e.notify()
}

// recordCell stores one cell's outcome and updates its chain tip.
//
// generation is the generation NUMBER, resolved to a slice index here under the
// lock. A cell can finish long after its generation was created, and the history
// trim shifts every index, so an index captured earlier would address the wrong
// row by the time the result lands.
func (e *Engine) recordCell(generation uint64, cell int, res *chain.StepResult, err error) {
	e.mu.Lock()

	// The store is the durable record and does not depend on the window, so it
	// is written whether or not the generation is still displayable.
	genIdx := indexOfGeneration(e.history, generation)

	if err != nil {
		if genIdx >= 0 {
			e.history[genIdx].Cells[cell] = CellTx{Cell: cell, State: TxFailed, Err: err.Error()}
		}
		e.lastError = err.Error()
		e.notify()
		e.mu.Unlock()
		e.persist(history.CellTx{
			Generation: generation, Cell: cell,
			Status: history.StatusFailed, Err: err.Error(),
		})
		return
	}

	if genIdx >= 0 {
		e.history[genIdx].Cells[cell] = CellTx{Cell: cell, TxID: res.TxID, State: TxBroadcast}
	}
	e.indexTx(res.TxID, generation, cell)
	e.state.Chains[cell] = chain.CellChain{
		Cell:       cell,
		TxID:       res.TxID,
		Vout:       0,
		Satoshis:   e.state.Chains[cell].Satoshis,
		Generation: res.Generation,
		RowHex:     res.RowHex,
		RawTxHex:   res.RawTxHex,
	}
	e.state.Generation = e.frontierLocked()
	if idx := indexOfGeneration(e.history, e.state.Generation); idx >= 0 {
		e.state.RowHex = e.history[idx].RowHex
	}
	e.totalTx++
	e.dirty = true
	e.notify()
	e.mu.Unlock()

	// Persist outside the lock: at 512 transactions a second a synchronous
	// write inside the critical section would serialise the whole pipeline.
	e.persist(history.CellTx{
		Generation: generation, Cell: cell,
		TxID: res.TxID, Status: history.StatusBroadcast,
	})
}

// checkpoint writes tip state periodically rather than on every advance.
func (e *Engine) checkpoint(ctx context.Context) {
	tick := time.NewTicker(checkpointInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			e.saveIfDirty()
		}
	}
}

// saveIfDirty writes the tip state if anything has moved since the last write.
func (e *Engine) saveIfDirty() {
	e.mu.Lock()
	if !e.dirty {
		e.mu.Unlock()
		return
	}
	snapshot := e.state.Clone()
	e.dirty = false
	e.mu.Unlock()

	if err := e.chain.SaveState(snapshot); err != nil {
		e.logger.Error("checkpoint tip state", "err", err)
		e.mu.Lock()
		e.dirty = true // try again on the next tick
		e.mu.Unlock()
	}
}

// wake nudges every waiter without changing anything.
func (e *Engine) wake() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notify()
}

const (
	// leaseName is the single-writer election key, and leaseTTL how long a lease
	// survives without renewal.
	//
	// The TTL is the failover delay: a pod that dies without releasing keeps the
	// automaton stopped for this long. Short enough that a crash is not an
	// outage, long enough that a slow renewal does not hand the chains to a
	// second writer while the first is still using them.
	leaseName = "rule110-engine"
	leaseTTL  = 30 * time.Second

	// leaseRenew must be comfortably shorter than leaseTTL so a transient
	// database hiccup does not cost the lease.
	leaseRenew = 10 * time.Second
)

// holdLease keeps this instance's claim on being the single writer.
//
// The engine owns 128 live UTXO chains. Two instances advancing them would
// double-spend every one, and a Kubernetes rolling update runs two pods at once
// by design — so this is not a theoretical concern, it is the default
// deployment behaviour. An instance without the lease still serves the UI and
// still applies statuses; it just does not advance anything.
func (e *Engine) holdLease(ctx context.Context) {
	tick := time.NewTicker(leaseRenew)
	defer tick.Stop()

	for {
		held, err := e.store.AcquireLease(ctx, leaseName, e.owner, leaseTTL)
		if err != nil {
			// Treat an unreachable store as not holding it. Advancing on the
			// assumption that we still do is the one outcome worth avoiding.
			held = false
			e.logger.ErrorContext(ctx, "lease renewal failed", "err", err)
		}
		e.setLeader(held)

		select {
		case <-ctx.Done():
			// Hand over promptly on a clean shutdown rather than making the
			// successor wait out the whole TTL.
			if e.isLeader() {
				rel, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				if err := e.store.ReleaseLease(rel, leaseName, e.owner); err != nil {
					e.logger.Error("release lease", "err", err)
				}
			}
			return
		case <-tick.C:
		}
	}
}

// setLeader records whether this instance may advance cells, logging only the
// transitions.
func (e *Engine) setLeader(held bool) {
	e.mu.Lock()
	was := e.leader
	e.leader = held
	e.mu.Unlock()

	if was == held {
		return
	}
	if held {
		e.logger.Info("acquired the writer lease; advancing", "owner", e.owner)
	} else {
		e.logger.Warn("lost the writer lease; serving read-only", "owner", e.owner)
	}
	e.wake()
}

// isLeader reports whether this instance holds the writer lease.
func (e *Engine) isLeader() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.leader
}
