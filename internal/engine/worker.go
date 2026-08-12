package engine

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/history"
)

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

// retryPause is how long a cell waits before re-attempting a generation whose
// transition failed before anything could be signed.
//
// It exists because such a failure no longer halts the cell — see
// transitionFailed — so nothing else bounds the retry. The tip has not moved, so
// turnReady is true again the instant this returns, and the worker would
// otherwise re-attempt at the speed the error comes back: 128 goroutines
// reconnecting as fast as they can to the database that just answered "sorry,
// too many clients already", which is the one response guaranteed to keep it
// saying that.
//
// Half a second bounds a whole ring in trouble to a couple of hundred attempts
// a second, and one stuck cell to two — which is also what keeps
// transitionFailed's log line from becoming the outage. It is backpressure, not
// a cure: the cure is an operator reading lastError.
const retryPause = 500 * time.Millisecond

// Run starts the clock, the status pipeline, and one worker per cell.
//
// There is no per-generation barrier. Cells advance independently and a slow
// cell falls behind rather than holding every other cell up, so the automaton's
// rate stops being set by its worst cell.
func (e *Engine) Run(ctx context.Context) {
	go e.trackFunds(ctx)
	go e.watchStatus(ctx)
	go e.writeStatuses(ctx)
	// Before any cell worker, because they block on it: a cell cannot broadcast
	// until its write-ahead record is durable, and nothing makes it durable
	// until this is running.
	go e.commitTxs(ctx)
	go e.PublishTails(ctx, PublishInterval)
	go e.reconcile(ctx)
	go e.clock(ctx)
	go e.holdLease(ctx)

	cells := e.deployment.Cells
	done := make(chan struct{})
	for cell := range cells {
		go func(cell int) {
			defer func() { done <- struct{}{} }()
			e.runCell(ctx, cell)
		}(cell)
	}
	for range cells {
		<-done
	}
	// Nothing to checkpoint on the way out. Every tip is written to the history
	// store as part of recording the transaction that created it, before the
	// worker moves on, so there is no in-memory position left to lose — which
	// there was when the tips lived in a file written on a one-second timer.
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
		frontier, maxLag := e.frontierLocked(), e.chain.Config.MaxLag
		e.mu.RUnlock()

		// The frontier can overtake the target, so this subtraction has to
		// saturate. frontier is the minimum over cells that are NOT halted, so
		// halting the slowest cell REMOVES it from that minimum and the frontier
		// jumps upward — past the target, which was tracking the laggard.
		//
		// Unguarded, `target - frontier` then underflows to about 2^64, which is
		// not less than maxLag, so the clock stops raising the target and never
		// starts again: every cell sits at or above a target that no longer moves,
		// and the automaton is frozen with nothing in any log to say why. It
		// survives a restart, because derivation reproduces the same shape.
		//
		// Snapshot's Lag has always saturated (`target - min(target, frontier)`),
		// which is what made this so hard to see: the UI reports a calm lag of 0
		// while the clock is dead. Observed live — cell 44 was the slowest cell at
		// generation 905, was refused, halted, and took the whole ring down with
		// it. SetMode already re-seeds the target this way, but only at a mode
		// change; nothing re-seeded it when a halt moved the frontier mid-run.
		if mode == ModeRunning {
			e.mu.Lock()
			if e.target < frontier {
				e.target = frontier
			}
			if e.target-frontier < maxLag {
				e.target++
				e.noteTargetRaisedLocked(e.target)
			}
			e.mu.Unlock()
			e.wake()
		}

		if mode == ModeRunning {
			// The interval, and ONLY the interval. Waking on Changed here as
			// well looks harmless and is not: notify() fires once per cell that
			// records a transition, about 128 times a generation, so the clock
			// woke on essentially every completed cell and raised the target
			// again. The configured rate was not a rate at all — the automaton
			// ran flat out, bounded only by maxLag.
			//
			// It hid because it could not bite until the automaton was fast.
			// While a transition took ~4 seconds, cells could not keep up with
			// 1 gen/s anyway and the target was always waiting for them.
			// Measured after that stopped being true: -rate 1, 2, 3 and 5 all
			// produced between 1.7 and 2.0 generations a second, which is
			// capacity, not any of the numbers asked for.
			//
			// Nothing is lost by not watching Changed while running: mode is
			// re-read at the top of every iteration, so a pause takes effect on
			// the next tick and cannot raise the target in the meantime.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(float64(time.Second) / rate)):
			}
			continue
		}

		// Paused or starved: nothing to schedule, so sleep on the state change
		// instead of spinning. Step raises the target directly.
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		case <-e.Changed():
		}
	}
}

// runCell advances one cell for as long as the engine is running.
//
// The context is checked HERE as well as inside awaitTurn, which looks
// redundant and is not: awaitTurn returns true the moment a cell's turn is
// ready, without reaching its select, so a worker whose turn is ready never
// observes cancellation there at all. It then calls advanceCell, which fails
// immediately once the store committer has stopped, and comes straight back
// round to a turn that is still ready.
//
// That is an unbounded spin, and it is only unbounded now: persist used to
// block on the database, which made each turn of the loop slow enough to look
// like nothing was wrong. Group-committing made the failure instant, and a
// single shutdown produced 7.3 MILLION log lines from 128 workers racing a
// cancelled context. Measured, not theorised — it is what the second
// connection-pool run wrote to stderr while stopping.
func (e *Engine) runCell(ctx context.Context, cell int) {
	for {
		if ctx.Err() != nil {
			return
		}
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
		mode      = e.mode
		target    = e.target
		tip       = e.tips[cell].Generation
		mined     = e.lastMined[cell]
		seen      = e.lastSeen[cell]
		maxDeep   = e.chain.Config.MaxUnconfirmedDepth
		maxUnseen = e.chain.Config.MaxUnseenDepth
		leader    = e.leader
		rederive  = e.needsRederive
	)
	e.mu.RUnlock()

	if !leader {
		// Another instance owns these chains. See holdLease.
		return false
	}
	if rederive {
		// We hold the lease but have not yet re-derived the tips under it. If the
		// lease had genuinely lapsed, another writer may have advanced every cell
		// while we were not looking, and the tips in memory would be a generation
		// or more behind — so advancing now would double-spend all 128 at once.
		// See rederive.
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
	// this cell's unbroken run of unconfirmed transactions inside whatever
	// mempool ancestor limit the network enforces.
	//
	// It is OFF by default, because the networks this runs against enforce no
	// such limit and a bound is a rate throttle: depth grows at the generation
	// rate and drains only when a block lands, so any finite bound caps the
	// sustained rate at depth/blocktime however fast the code is. See
	// chain.Config.MaxUnconfirmedDepth.
	//
	// A zero limit means unbounded, not "stop". Reading it the other way turns
	// an unset field into an automaton that silently never advances, which is
	// the worst possible failure for a config mistake.
	if maxDeep > 0 && tip-mined >= maxDeep {
		return false
	}

	// The acceptance gate, and the one that matters most at rate. Arcade's 202
	// is "accepted for processing", not "validated" and not "in a mempool", so
	// building on it submits a child that spends an output the network has not
	// yet heard of — refused for a missing input, purely for arriving first.
	// That is how all 256 cells were lost in under two minutes. See
	// Config.MaxUnseenDepth.
	//
	// `tip > seen` is not redundant. repairCell pulls a tip BACKWARD, and on
	// unsigned integers tip-seen would then wrap to an enormous number and clamp
	// the cell shut for ever, with nothing anywhere saying why.
	if maxUnseen > 0 && tip > seen && tip-seen >= maxUnseen {
		return false
	}
	return tip < target
}

// depthPoll is how often a cell re-checks its turn when the depth gate is
// armed, and it is armed only when an operator sets -max-depth.
//
// The gate reopens when a status event raises lastMined, and a status batch
// notifies only when it changed something a cell DISPLAYS — so a mined status
// for a generation already shown as mined releases the gate silently, and a
// worker waiting on Changed alone would sleep through it. Every other input to
// turnReady does arrive as a notify, which is why this poll runs only while the
// gate is on: 128 cells waking four times a second is a real cost to carry for
// a case the default configuration does not have.
const depthPoll = 250 * time.Millisecond

// awaitTurn blocks until this cell should advance, or until ctx ends (returning
// false).
func (e *Engine) awaitTurn(ctx context.Context, cell int) bool {
	// Read once: the gate is configuration, fixed for the life of the process.
	gated := e.chain.Config.MaxUnconfirmedDepth > 0

	for {
		// Subscribe BEFORE testing, not after. Changed() hands out a channel that
		// notify() replaces, so a state change landing between the test below and
		// the select would close a channel this worker no longer holds, and the
		// cell would sleep until something unrelated woke it. The 250 ms poll used
		// to paper over that; with the poll gone by default it would be a cell
		// that stops for no visible reason, which is the failure mode this whole
		// file is written to avoid. Same discipline as PublishTails.
		changed := e.Changed()

		e.mu.RLock()
		halted := e.halted[cell]
		repair := e.needsRepair[cell]
		e.mu.RUnlock()
		if halted {
			// The chain is broken; nothing this worker does can mend it.
			<-ctx.Done()
			return false
		}
		if repair {
			// A refusal left this cell pointing at an output the network never
			// produced. Rebuild before advancing — here, not where the rejection
			// arrived, because this is the only goroutine that can be sure no
			// transition for this cell is in flight. See repairCell.
			//
			// Backing off first, because the halt budget now spans a minute and
			// rebuilding flat out for that minute — on every affected cell at
			// once — would be this ring doing to arcade what arcade's backlog
			// did to it. See rebuildPauseLocked.
			e.mu.RLock()
			pause := e.rebuildPauseLocked(cell)
			e.mu.RUnlock()
			select {
			case <-ctx.Done():
				return false
			case <-time.After(pause):
			}
			e.repairCell(ctx, cell)
			continue
		}
		if e.turnReady(cell) {
			return true
		}

		// nil unless the depth gate is armed, and a nil channel never fires.
		var poll <-chan time.Time
		if gated {
			poll = time.After(depthPoll)
		}
		select {
		case <-ctx.Done():
			return false
		case <-changed:
		case <-poll:
		}
	}
}

// advanceCell moves one cell forward a generation and records the result.
func (e *Engine) advanceCell(ctx context.Context, cell int) {
	e.mu.RLock()
	tip := e.tips[cell]
	e.mu.RUnlock()
	cells, rule := e.deployment.Cells, e.deployment.Rule

	// Write the intent BEFORE building anything. Signing broadcasts, so there is
	// a window in which the output is spent on chain and we do not yet know it;
	// a process killed there would come back a generation behind and re-spend an
	// output that is already gone. This row is what tells the next startup that
	// the tip is unknown rather than stale. See history.StatusAttempting.
	//
	// And if it cannot be written, do not broadcast. persist has already halted
	// the cell at this point, but a halt only stops the cell's NEXT turn — it
	// does not unwind this call. Continuing would spend the tip with nothing
	// durable saying we ever tried, so the next startup would derive the tip one
	// generation back and re-spend an output the network had already consumed.
	// Observed live: a saturated database halted 70 cells this way, every one of
	// which had broadcast regardless.
	start := time.Now()
	persistStart := start
	ok := e.persist(history.CellTx{
		Generation: tip.Generation + 1, Cell: cell, Status: history.StatusAttempting,
	})
	e.perf.persistAttempting.Observe(time.Since(persistStart))
	if !ok {
		return
	}

	res, err := e.chain.AdvanceCell(ctx, e.compiled, tip, cells, rule)
	if err != nil {
		e.perf.failuresTotal.Inc()
		e.transitionFailed(ctx, tip.Generation+1, cell, err)
		return
	}
	e.perf.observeStep(res.Timings)

	e.doneWaiting(cell)
	e.clearStarvation()
	e.ensureGeneration(ctx, res.Generation, res.RowHex)
	e.recordCell(res.Generation, cell, res, nil)

	// Measured around everything, including the lock waits and the two durable
	// writes, so the gap between this and the sum of the phases is the cost of
	// being one of 128 rather than the only one.
	e.perf.advanceTotal.Observe(time.Since(start))
}

// transitionFailed decides what a transition that did not complete leaves
// behind, which is really the question of whether the cell survives it.
//
// # A failure that never reached the network must not be durable
//
// chain.ErrNotBroadcast is set immediately BEFORE SignAction, and signing is
// what broadcasts. So it says, with certainty, that nothing was signed and
// nothing reached the network: the cell's tip is untouched and re-attempting the
// same generation cannot double spend. Two things follow.
//
// The write-ahead record is retracted, because it now describes an attempt that
// provably never happened and the next startup would otherwise read it as "this
// cell's tip is UNKNOWN".
//
// And no `failed` row is written. That row is not a note for a human — it is
// what DeriveTips reads to halt a cell, permanently, at this startup and every
// one after it. Writing one for a local error means a database hiccup costs a
// cell for good: 21 of the live deployment's 128 were halted by a single burst
// of "sorry, too many clients already" inside CreateAction, every one of them
// with an empty txid, none of them having spent anything. Nothing is lost by
// staying quiet in the store, because the row would say nothing an operator can
// act on that lastError, the cell's state in the UI and the log line below do
// not already say — and unlike them it would outlive the problem it describes.
//
// # Everything else stays durable
//
// Past that line a transaction may exist on the network whatever the error says,
// so the failure is recorded exactly as before and the cell halts until a human
// or `rule110 recover` decides. A rejection is the network's verdict on a
// transition that WAS offered to it, and rebuilding that generation unattended
// is the double spend the whole of this design exists to avoid.
//
// Funding shortfalls are neither: they are ordinary backpressure, they return
// early, and the automaton's own starvation handling owns them.
func (e *Engine) transitionFailed(ctx context.Context, generation uint64, cell int, err error) {
	notBroadcast := errors.Is(err, chain.ErrNotBroadcast)
	if notBroadcast {
		e.retractAttempt(generation, cell)
	}
	if ctx.Err() != nil {
		return
	}
	if isFundingShortfall(err) {
		e.onShortfall(ctx, cell)
		return
	}
	e.doneWaiting(cell)

	if notBroadcast {
		// Visible, but not halting: the cell re-attempts this same generation on
		// its next turn.
		e.noteFailure(generation, cell, err)
		e.logger.WarnContext(ctx, "cell transition failed before anything was signed; retrying",
			"cell", cell, "generation", generation, "err", err)
		select {
		case <-ctx.Done():
		case <-time.After(retryPause):
		}
		return
	}
	e.recordCell(generation, cell, nil, err)
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
	gen := Generation{Number: number, RowHex: rowHex, Cells: make([]CellTx, e.deployment.Cells)}
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
	for i, c := range e.tips {
		if e.halted[i] {
			continue
		}
		if first || c.Generation < frontier {
			frontier, first = c.Generation, false
		}
	}
	if first {
		return e.generation // every cell halted; hold position
	}
	return frontier
}

// deepestLocked returns the deepest unconfirmed chain. Callers must hold the lock.
func (e *Engine) deepestLocked() uint64 {
	var deepest uint64
	for i, c := range e.tips {
		if e.halted[i] {
			continue
		}
		if c.Generation > e.lastMined[i] {
			if d := c.Generation - e.lastMined[i]; d > deepest {
				deepest = d
			}
		}
	}
	return deepest
}

// deepestUnseenLocked is the largest gap between any live cell's tip and the
// newest generation of it the NETWORK HAS ACCEPTED.
//
// This is what waiting on the acceptance gate looks like from outside, and it
// needs to be visible: a cell blocked here is doing the right thing, but from
// the diagram it is indistinguishable from a cell that has stopped. Callers
// must hold the lock.
func (e *Engine) deepestUnseenLocked() uint64 {
	var deepest uint64
	for i, c := range e.tips {
		if e.halted[i] {
			continue
		}
		// Guarded, not subtracted blind: repairCell pulls a tip behind lastSeen
		// and the unsigned difference would wrap to an enormous number.
		if c.Generation > e.lastSeen[i] {
			if d := c.Generation - e.lastSeen[i]; d > deepest {
				deepest = d
			}
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
	e.perf.shortfallsTotal.Inc()
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

// recordCell stores one cell's outcome durably and updates its chain tip.
//
// generation is the generation NUMBER, resolved to a slice index under the lock.
// A cell can finish long after its generation was created, and the history trim
// shifts every index, so an index captured earlier would address the wrong row
// by the time the result lands.
func (e *Engine) recordCell(generation uint64, cell int, res *chain.StepResult, err error) {
	if err != nil {
		e.noteFailure(generation, cell, err)
		// The store is the durable record and does not depend on the window, so
		// it is written whether or not the generation is still displayable. This
		// row is also what halts the cell at the next startup, which is why
		// transitionFailed keeps failures that never reached the network away
		// from here.
		e.persist(history.CellTx{
			Generation: generation, Cell: cell,
			Status: history.StatusFailed, Err: err.Error(),
		})
		return
	}

	lockStart := time.Now()
	e.mu.Lock()
	e.perf.recordLockWait.Observe(time.Since(lockStart))
	genIdx := indexOfGeneration(e.history, generation)
	if genIdx >= 0 {
		e.history[genIdx].Cells[cell] = CellTx{Cell: cell, TxID: res.TxID, State: TxBroadcast}
	}
	e.indexTx(res.TxID, generation, cell, time.Now())
	e.tips[cell] = chain.CellChain{
		Cell:       cell,
		TxID:       res.TxID,
		Vout:       0,
		Satoshis:   e.tips[cell].Satoshis,
		Generation: res.Generation,
		RowHex:     res.RowHex,
		RawTxHex:   res.RawTxHex,
	}
	// The frontier moving is what "a generation completed" means: it is the
	// minimum over unhalted cells, so it passing g says every cell that still
	// can has proved g. Recorded here because this is the only place it moves
	// forward under load.
	wasFrontier := e.generation
	e.generation = e.frontierLocked()
	done, completed := e.observeFrontierLocked(wasFrontier, e.generation)
	e.totalTx++
	// This cell has moved past whatever it was last refused for, so the refusal
	// count that would eventually halt it no longer describes anything.
	e.clearRetriesLocked(cell, res.Generation)
	e.notify()
	e.mu.Unlock()

	if completed {
		e.logGeneration(done)
	}

	// Persist outside the lock: at 512 transactions a second a synchronous
	// write inside the critical section would serialise the whole pipeline.
	//
	// This row is now the ONLY record of where the cell is — there is no state
	// file behind it — so persist halts the cell if it cannot be written rather
	// than logging and carrying on. See Engine.persist.
	persistStart := time.Now()
	e.persist(history.CellTx{
		Generation: generation, Cell: cell,
		TxID: res.TxID, Status: history.StatusBroadcast,
	})
	e.perf.persistBroadcast.Observe(time.Since(persistStart))
}

// noteFailure shows one cell's failure to whoever is watching, and writes
// nothing durable.
//
// Split out of recordCell because the two halves answer different questions. The
// UI's copy and lastError describe what is happening NOW and are replaced by the
// next thing that happens; the store's `failed` row is a verdict that outlives
// the process and halts the cell. A failure that never reached the network earns
// the first and must not earn the second. See transitionFailed.
func (e *Engine) noteFailure(generation uint64, cell int, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Resolved to a slice index here under the lock: a cell can finish long after
	// its generation was created, and the history trim shifts every index, so an
	// index captured earlier would address the wrong row by the time the result
	// lands.
	if genIdx := indexOfGeneration(e.history, generation); genIdx >= 0 {
		e.history[genIdx].Cells[cell] = CellTx{Cell: cell, State: TxFailed, Err: err.Error()}
	}
	e.lastError = err.Error()
	e.notify()
}

// wake nudges every waiter without changing anything.
func (e *Engine) wake() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notify()
}

// LeaseName is the single-writer election key.
//
// Exported because it is not only the engine's. Any tool that spends a cell's
// UTXO — the depth probe, `recover`, `import-tips` — must take the SAME lease,
// and a tool that takes a different one runs alongside the engine while both
// believe they are alone. The resulting rejection is indistinguishable from an
// ordinary failure, which is the worst possible way to be wrong about this. It
// was a const here and a duplicated string literal twice in the probe; one
// definition removes the way to get it wrong.
const LeaseName = "rule110-engine"

// LeaseTTL and LeaseRenew are exported for the same reason LeaseName is: the
// cold start holds this lease before there is an engine, and a bootstrapper
// using different timings would either lose the claim under it or hold it past
// its own death.
const (
	// LeaseTTL is how long a lease survives without renewal.
	//
	// The TTL is the failover delay: a pod that dies without releasing keeps the
	// automaton stopped for this long. Short enough that a crash is not an
	// outage, long enough that a slow renewal does not hand the chains to a
	// second writer while the first is still using them.
	LeaseTTL = 30 * time.Second

	// LeaseRenew must be comfortably shorter than LeaseTTL so a transient
	// database hiccup does not cost the lease.
	LeaseRenew = 10 * time.Second
)

// holdLease keeps this instance's claim on being the single writer.
//
// The engine owns 128 live UTXO chains. Two instances advancing them would
// double-spend every one, and a Kubernetes rolling update runs two pods at once
// by design — so this is not a theoretical concern, it is the default
// deployment behaviour. An instance without the lease still serves the UI and
// still applies statuses; it just does not advance anything.
func (e *Engine) holdLease(ctx context.Context) {
	tick := time.NewTicker(LeaseRenew)
	defer tick.Stop()

	for {
		held, err := e.store.AcquireLease(ctx, LeaseName, e.owner, LeaseTTL)
		if err != nil {
			// Treat an unreachable store as not holding it. Advancing on the
			// assumption that we still do is the one outcome worth avoiding.
			held = false
			e.logger.ErrorContext(ctx, "lease renewal failed", "err", err)
		}
		e.setLeader(held)
		if held {
			// Runs in this goroutine, not a new one, so a slow derivation simply
			// delays the next renewal instead of racing itself. Nothing advances
			// meanwhile: turnReady blocks on needsRederive.
			e.rederiveIfNeeded(ctx)
		}

		select {
		case <-ctx.Done():
			// Hand over promptly on a clean shutdown rather than making the
			// successor wait out the whole TTL.
			if e.isLeader() {
				rel, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				if err := e.store.ReleaseLease(rel, LeaseName, e.owner); err != nil {
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
//
// Gaining the lease sets needsRederive. This instance cannot tell a lease it
// never really lost — a database hiccup during renewal drops `leader` to false
// and the next tick puts it back — from one that genuinely expired and was held
// by somebody else in between. In the second case that somebody advanced all 128
// chains and every tip in this process's memory is stale, so carrying on from
// them would double-spend the lot. Re-deriving costs one indexed query per cell
// and settles the question, so it is done unconditionally rather than guessed at.
func (e *Engine) setLeader(held bool) {
	e.mu.Lock()
	was := e.leader
	e.leader = held
	if held && !was {
		e.needsRederive = true
	}
	e.mu.Unlock()

	if was == held {
		return
	}
	if held {
		e.logger.Info("acquired the writer lease; re-deriving tips before advancing", "owner", e.owner)
	} else {
		e.logger.Warn("lost the writer lease; serving read-only", "owner", e.owner)
	}
	e.wake()
}

// rederiveIfNeeded rebuilds the tips from the store while this instance holds
// the lease, and releases the cells once it succeeds.
//
// A failure leaves needsRederive set, which leaves every cell blocked. That is
// the right way round: the alternative is advancing on tips nobody has
// confirmed, and a stopped automaton is recoverable where a double spend is not.
// holdLease calls this on every renewal, so a transient failure clears itself.
func (e *Engine) rederiveIfNeeded(ctx context.Context) {
	e.mu.RLock()
	needed := e.needsRederive
	e.mu.RUnlock()
	if !needed {
		return
	}

	positions, err := DeriveTips(ctx, e.ledger, e.compiled, e.deployment, e.store)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		e.logger.ErrorContext(ctx, "could not re-derive tips; cells stay stopped", "err", err)
		e.mu.Lock()
		e.lastError = "tips could not be re-derived, so no cell may advance: " + err.Error()
		e.notify()
		e.mu.Unlock()
		return
	}
	if err := CheckMigrationFloor(ctx, e.store, e.chain.Oracle, positions, e.deployment.LegacyTips()); err != nil {
		e.logger.ErrorContext(ctx, "refusing to advance", "err", err)
		e.mu.Lock()
		e.lastError = err.Error()
		e.notify()
		e.mu.Unlock()
		return
	}

	if e.opts.AutoRecover {
		positions = e.recoverUnknown(ctx, positions)
	}
	e.applyPositions(positions)

	e.mu.Lock()
	e.needsRederive = false
	e.mu.Unlock()
	e.wake()
}

// isLeader reports whether this instance holds the writer lease.
func (e *Engine) isLeader() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.leader
}
