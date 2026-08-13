package engine

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"

	"github.com/dymurray/rule-110-arcade/internal/history"
)

// txLoc points back from a txid to the cell that produced it.
//
// It carries the generation NUMBER, not an index into the history slice. The
// slice is trimmed to maxHistory by dropping from the front, which shifts every
// index down — so an index recorded when a transaction was broadcast addresses a
// different generation by the time its status arrives, and the status lands on an
// unrelated row. Numbers are stable; indexes are not.
type txLoc struct {
	generation uint64
	cell       int
}

// stateFor maps an arcade lifecycle status onto a cell's display state.
//
// Returns ok=false for statuses that say nothing new about the cell, so an
// unknown or intermediate status never downgrades what we already showed.
func stateFor(s arcade.Status) (TxState, bool) {
	switch s {
	case arcade.StatusSeenOnNetwork, arcade.StatusSeenMultipleNodes, arcade.StatusAcceptedByNetwork:
		return TxSeen, true
	case arcade.StatusMined, arcade.StatusImmutable:
		return TxMined, true
	case arcade.StatusRejected, arcade.StatusDoubleSpendAttempted:
		return TxFailed, true
	default:
		return "", false
	}
}

// rank orders the states so a late-arriving weaker status cannot undo a
// stronger one. SSE delivery is not ordered, so SEEN can arrive after MINED.
func rank(s TxState) int {
	switch s {
	case TxPending:
		return 0
	case TxBroadcast:
		return 1
	case TxSeen:
		return 2
	case TxMined:
		return 3
	case TxFailed:
		return 4 // terminal, and the one thing that must always win
	default:
		return 0
	}
}

// watchStatus attaches the engine to the monitor's status pipeline.
//
// Without this the engine only ever knows that a transaction was ACCEPTED at
// intake; it never learns that the network saw it, mined it, or rejected it. A
// rejected cell would keep showing as broadcast and, worse, the engine would go
// on trying to spend an output that does not exist.
//
// This used to be our own arcade.StreamStatus subscription, which was wrong in
// two compounding ways.
//
// It was a SECOND connection on the same callback token, while the wallet
// monitor already held one. Arcade's GET /events has no per-client filter, so
// the duplicate received every event twice over and doubled arcade's per-event
// fan-out cost — against a fan-out that is already the measured bottleneck.
//
// And it was the slow consumer that behaviour punishes: every event applied
// synchronously on the reader goroutine, under the engine's global write lock,
// across a Postgres round trip. A reader that stops draining its socket is a
// reader arcade drops events for.
//
// The replay cursor went with it, and that is a strict gain: it lived in a local
// variable that reset to "" on every restart, and arcade replays only
// NON-terminal statuses to a cold connection — so terminal statuses that landed
// while we were down were silently lost. The monitor's cursor is durable.
func (e *Engine) watchStatus(ctx context.Context) {
	e.chain.OnStatus(e.applyStatusBatch)
	<-ctx.Done()
	e.chain.OnStatus(nil)
}

// reconcile polls arcade for cells that are still non-terminal.
//
// The event stream is the fast path, not the authority. Events can be missed
// across a reconnect gap, and a status that fired before we ever subscribed is
// never replayed at all. Polling the transactions we are still waiting on makes
// the displayed state converge on arcade's own view rather than drifting from
// it — which is the difference between a diagram that is merely live and one
// that is correct.
func (e *Engine) reconcile(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}

		// The store is the authority on what is still in flight, so a restart
		// resumes reconciling transactions this process never broadcast.
		//
		// Bounded on both axes. Only transactions that have been in flight a
		// while are worth polling — the stream resolves the rest on its own —
		// and only a batch of them per pass, because the in-flight set grows
		// with the automaton and a pass that outlasts its own interval never
		// completes again.
		pending, err := e.store.Stale(ctx, reconcileAfter, reconcileLimit)
		if err != nil {
			e.logger.ErrorContext(ctx, "load stale transactions", "err", err)
			continue
		}
		e.reconcileBatch(ctx, pending)
	}
}

// applyStatus records a status update against whichever cell owns the txid.
//
// The single-record door into applyStatusBatch, kept for the reconciler and for
// the rejection-reason backfill.
func (e *Engine) applyStatus(txid string, status arcade.Status, extra string) {
	e.applyStatusBatch([]arcade.TxRecord{{TxID: txid, Status: status, ExtraInfo: extra}})
}

// applyStatusBatch records a batch of status updates against the cells that own
// them. It is what monitor.WithStatusObserver delivers into.
//
// Three properties matter here and none of them held before.
//
// ONE LOCK ACQUISITION PER BATCH, NOT PER EVENT. Ownership is decided first,
// under a read lock; a status for a txid that is not ours — and with
// FullStatusUpdates the stream carries a great many — now costs a read lock
// rather than the engine's global write lock.
//
// NO DATABASE WRITE UNDER THE LOCK. The writes are handed to a batching writer
// after the lock is released. Holding the global lock across a Postgres round
// trip serialised all 128 cell workers, every snapshot reader and the clock
// behind the network, once per event.
//
// IT MUST NOT BLOCK. This runs inline on the monitor's applier goroutine, so
// anything slow here stops that applier draining its hand-off queue, and a
// hand-off queue that fills stalls the SSE reader — which is how arcade comes to
// drop events for a slow client. Everything below is memory plus a non-blocking
// enqueue.
func (e *Engine) applyStatusBatch(recs []arcade.TxRecord) {
	if len(recs) == 0 {
		return
	}

	// Pass 1, read lock: keep only records that are ours and that say something
	// new. This is the cheap filter that keeps foreign traffic off the write
	// lock entirely.
	type pending struct {
		rec  arcade.TxRecord
		next TxState
	}
	var work []pending
	e.mu.RLock()
	for _, rec := range recs {
		next, ok := stateFor(rec.Status)
		if !ok {
			continue
		}
		if _, known := e.txIndex[rec.TxID]; !known {
			continue // not one of ours
		}
		work = append(work, pending{rec: rec, next: next})
	}
	e.mu.RUnlock()

	if len(work) == 0 {
		return
	}

	// Pass 2, one write lock for the whole batch. Nothing in here does I/O.
	var (
		writes  []history.StatusUpdate
		reasons []struct {
			txid string
			cell int
		}
		changed bool
	)

	e.mu.Lock()
	for _, w := range work {
		// Re-read the index under the write lock: a terminal status earlier in
		// this same batch may already have evicted it.
		loc, known := e.txIndex[w.rec.TxID]
		if !known {
			continue
		}

		st := history.Status(w.next)
		errMsg := ""
		if w.next == TxFailed {
			errMsg = rejectionMessage(w.rec.Status, w.rec.ExtraInfo)
		}

		// BOTH GATES ARE RELEASED HERE, BEFORE ANY RENDER-WINDOW CHECK.
		//
		// These are the engine's governors, not display state, and everything
		// below this point is about whether a row is still on screen. Updating
		// them after the trim check meant a status for a generation that had
		// scrolled out of the in-memory window released nothing — which for the
		// depth gate is a cell that never reopens, and for the acceptance gate
		// is a cell that waits for ever on a transaction the network already
		// took. Mined statuses in particular arrive hundreds of generations
		// behind the frontier, which is exactly where the window ends.
		//
		// A mined transaction shortens that cell's unconfirmed chain, releasing
		// the depth gate; acceptance releases the gate that stops a child being
		// submitted before its parent lands (see Config.MaxUnseenDepth).
		//
		// TxMined counts for BOTH, and must: SSE delivery is unordered — rank
		// exists for that reason — so a cell whose SEEN frame was dropped but
		// whose MINED arrived would otherwise sit behind the acceptance gate on
		// a transaction the network has demonstrably accepted.
		if w.next == TxMined && loc.generation > e.lastMined[loc.cell] {
			e.lastMined[loc.cell] = loc.generation
		}
		if (w.next == TxSeen || w.next == TxMined) && loc.generation > e.lastSeen[loc.cell] {
			e.lastSeen[loc.cell] = loc.generation
		}

		genIdx := indexOfGeneration(e.history, loc.generation)
		if genIdx < 0 || loc.cell >= len(e.history[genIdx].Cells) {
			// Trimmed out of the in-memory window. The store still has it, so
			// persist the status even though nothing is left to redraw.
			writes = append(writes, history.StatusUpdate{TxID: w.rec.TxID, Status: st, Err: errMsg})
			if st.IsTerminal() {
				delete(e.txIndex, w.rec.TxID)
			}
			continue
		}

		cell := &e.history[genIdx].Cells[loc.cell]
		if rank(w.next) <= rank(cell.State) {
			continue
		}
		cell.State = w.next
		changed = true

		writes = append(writes, history.StatusUpdate{TxID: w.rec.TxID, Status: st, Err: errMsg})
		if st.IsTerminal() {
			delete(e.txIndex, w.rec.TxID)
		}

		if w.next == TxFailed {
			cell.Err = errMsg
			e.noteRefusalLocked(loc.cell, loc.generation, w.rec.TxID, string(w.rec.Status), errMsg)
			if w.rec.ExtraInfo == "" {
				reasons = append(reasons, struct {
					txid string
					cell int
				}{w.rec.TxID, loc.cell})
			}
		}
	}
	if changed {
		// Once per batch, not once per event. A generation settling used to
		// wake all 128 cell workers, the clock and every SSE handler 128 times.
		e.notify()
	}
	e.mu.Unlock()

	// Everything past here is off the lock.
	e.enqueueStatusWrites(writes)
	for _, r := range reasons {
		go e.fetchRejectionReason(r.txid, r.cell)
	}
}

// maxRetries is how many consecutive refusals of the SAME generation a cell may
// rebuild through before it halts and waits for a human.
//
// Refusals here are intermittent, not deterministic: measured at roughly 2 per
// 16,000 transactions, and a refused generation has been retracted, rebuilt and
// accepted on the live deployment. So the first refusal says almost nothing and
// the third says a great deal — something about this particular transition does
// not work, and rebuilding it a fourth time is a loop, not a repair.
const maxRetries = 3

// minHaltWindow is how long a cell's refusals must SPAN before any number of
// them is allowed to halt it.
//
// maxRetries alone counts attempts, and rebuilds are immediate, so three of them
// fit comfortably inside one bad second. That is not three pieces of evidence
// about a transition — it is one, sampled three times. The public deployment
// lost all 256 cells to exactly that: children refused for arriving before their
// parents were accepted, with the worst measured parent-acceptance lag at 21.7
// seconds and the whole retry budget spent inside the first four.
//
// 60s is comfortably past that worst case. A cell whose refusals have persisted
// for a minute has met something that is not going to resolve itself.
const minHaltWindow = 60 * time.Second

// retryState counts consecutive refusals of one generation, and when they began.
type retryState struct {
	generation uint64
	attempts   int
	// first is when this run of refusals started. See minHaltWindow.
	first time.Time
}

// maxRebuildPause caps the wait between rebuilds of a refused generation.
//
// The halt budget now spans a minute, so without a backoff a persistently
// refused generation would rebuild flat out for that minute — on every affected
// cell at once. On a 256-cell ring that turns a transient network wobble into a
// self-inflicted one, which is the failure mode this whole change exists to stop
// rather than to reproduce from the other end.
const maxRebuildPause = 8 * time.Second

// rebuildPauseLocked is how long this cell should wait before re-attempting the
// generation it is repairing: exponential in the number of refusals so far,
// capped. Callers must hold the lock.
//
// Growing the wait is also what makes the attempts EVIDENCE. Three rebuilds in
// the same second sample one instant of the network three times; spaced out,
// they sample three.
func (e *Engine) rebuildPauseLocked(cell int) time.Duration {
	st, ok := e.retries[cell]
	if !ok || st.attempts <= 0 {
		return retryPause
	}
	pause := retryPause << min(st.attempts-1, 8)
	return min(pause, maxRebuildPause)
}

// isRetryableRejection reports whether arcade has said this refusal resolves
// itself.
//
// There is no structured field for it: arcade puts the verdict in the free-text
// extraInfo, in its own words — "retryable — resubmit after the ancestor is
// accepted" — so this matches the wording, and a test pins the real message so a
// reworded upstream fails loudly rather than silently re-arming the halt.
//
// StatusPendingRetry is arcade saying the same thing structurally. It does not
// currently reach this path (stateFor ignores it), and is handled anyway because
// the cost of being wrong in the other direction is a dead cell.
func isRetryableRejection(status arcade.Status, extra string) bool {
	if status == arcade.StatusPendingRetry {
		return true
	}
	return strings.Contains(strings.ToLower(extra), "retryable")
}

// noteRefusalLocked records a network refusal and decides whether the cell
// rebuilds that generation or halts. Callers must hold the write lock.
//
// The cell used to halt here, unconditionally and for ever — one refusal cost a
// cell until an operator ran `rule110 recover`. Since refusals are intermittent
// that is monotonic erosion: the ring loses cells one at a time for a reason that
// usually goes away by itself.
//
// Nothing here does I/O or derivation. This runs inline on the monitor's applier
// goroutine, and a slow observer stops that applier draining its hand-off queue,
// which stalls the SSE reader for the whole process. The repair itself is left to
// the cell's own worker; see repairCell.
func (e *Engine) noteRefusalLocked(cell int, generation uint64, txid, status, reason string) {
	// A refusal arcade itself calls retryable is not evidence about this
	// transition, so it schedules a rebuild without spending any budget. The
	// engine used to count these, and they are precisely the class that a
	// too-fast child produces against a parent the network has not yet taken.
	if isRetryableRejection(arcade.Status(status), reason) {
		e.needsRepair[cell] = true
		e.logger.Warn("cell refused retryably, scheduling a rebuild",
			"cell", cell, "generation", generation, "txid", txid, "status", status)
		return
	}

	st := e.retries[cell]
	if st.generation != generation {
		st = retryState{generation: generation, first: time.Now()}
	}
	if st.first.IsZero() {
		st.first = time.Now()
	}
	st.attempts++
	e.retries[cell] = st

	// BOTH bounds, not either. Attempts alone halted a cell on three rebuilds
	// inside one bad second; time alone would halt a cell that refused once an
	// hour ago and has been fine since.
	if st.attempts > maxRetries && time.Since(st.first) >= minHaltWindow {
		// Halt. Its successor UTXO does not exist, so every later generation
		// would try to spend a phantom output and fail with "utxo already
		// spent" — noise that hides the original rejection.
		e.halted[cell] = true
		// The reason, which this path used to drop: it set halted and left
		// haltReason empty, so the UI and /metrics reported a halted cell with
		// nothing to say about it.
		e.haltReason[cell] = reason
		delete(e.needsRepair, cell)
		e.logger.Warn("cell halted by rejection",
			"cell", cell, "generation", generation, "txid", txid,
			"status", status, "attempts", st.attempts)
		return
	}

	e.needsRepair[cell] = true
	e.logger.Warn("cell refused, scheduling a rebuild",
		"cell", cell, "generation", generation, "txid", txid,
		"status", status, "attempt", st.attempts, "of", maxRetries)
}

// clearRetries forgets a cell's refusal count once it has advanced past the
// generation being counted. Callers must hold the write lock.
//
// Consecutive is the whole point of the bound: a cell that refuses generation
// 249 three times is stuck, but one that refuses 249, recovers, runs a hundred
// generations and then refuses 349 has met two unrelated intermittent faults and
// must not be halted for the sum of them.
func (e *Engine) clearRetriesLocked(cell int, generation uint64) {
	if st, ok := e.retries[cell]; ok && generation > st.generation {
		delete(e.retries, cell)
	}
}

// fetchRejectionReason asks arcade why a transaction was rejected, when the
// event that told us did not say.
//
// Arcade's asynchronous publish path builds its event from the status, the
// timestamp and a list of txids — there is nowhere in it for a per-transaction
// reason, so EVERY pushed rejection arrives bare. The reason does exist, and
// GET /tx returns it in full:
//
//	UTXO_SPENT (70): d8f2ccc0…:0 utxo already spent by tx ffb1c43e…[0]
//
// which names the conflicting spend outright. Losing that costs real time: we
// reconstructed exactly this by hand from a node's propagation logs, and it is
// the difference between "your state is stale, resync" and "this transaction is
// malformed, never retry" — opposite responses.
//
// It runs off the hot path because a rejection is terminal for the cell anyway,
// and it must not hold the engine's write lock across a network call.
// Reported upstream as bsv-blockchain/arcade#301.
func (e *Engine) fetchRejectionReason(txid string, cell int) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rec, err := e.chain.Oracle.GetTx(ctx, txid)
	if err != nil || rec == nil || rec.ExtraInfo == "" {
		return
	}
	reason := rejectionMessage(rec.Status, rec.ExtraInfo)
	e.logger.Warn("rejection reason", "cell", cell, "txid", txid, "reason", rec.ExtraInfo)

	e.mu.Lock()
	if loc, ok := e.txIndex[txid]; ok {
		if idx := indexOfGeneration(e.history, loc.generation); idx >= 0 {
			e.history[idx].Cells[loc.cell].Err = reason
		}
	}
	e.notify()
	e.mu.Unlock()

	if err := e.store.UpdateStatus(ctx, txid, history.StatusFailed, reason); err != nil {
		e.logger.Error("persist rejection reason", "txid", txid, "err", err)
	}
}

const (
	// statusWriteQueue is the apply→writer hand-off buffer. It has to absorb a
	// burst — a generation of 128 cells settling, or a mined block's wave —
	// without the enqueue ever blocking, because the enqueue happens on the
	// monitor's applier goroutine and a stall there propagates back to the SSE
	// reader.
	statusWriteQueue = 8192

	// statusWriteLinger is how long the writer waits for more updates once the
	// queue has run dry, before writing what it has. It converts arrival rate
	// into batch size: at idle it is never paid at all (the writer only lingers
	// when it already holds something), and under load it turns a burst of
	// single-row updates into one statement.
	statusWriteLinger = 10 * time.Millisecond

	// statusWriteMax bounds one statement.
	statusWriteMax = 512
)

// enqueueStatusWrites hands status writes to the batching writer.
//
// Never blocks. If the queue is full the write is dropped and logged rather than
// stalling the caller: the caller is the monitor's applier goroutine, and
// blocking it stalls the SSE reader for every consumer in the process. A dropped
// write costs a stale `status` column until the reconciler polls that
// transaction and re-applies it — which is exactly the convergence guarantee the
// reconciler exists to provide. The in-memory state, which is what the UI draws,
// is already correct by this point.
func (e *Engine) enqueueStatusWrites(writes []history.StatusUpdate) {
	for _, w := range writes {
		select {
		case e.statusWrites <- w:
		default:
			e.logger.Warn("status write queue full, deferring to the reconciler", "txid", w.TxID)
		}
	}
}

// writeStatuses drains the status-write queue into batched store writes.
//
// One goroutine, so writes stay ordered per txid, and a bounded batch per
// statement. Ordering across txids does not matter — each update is keyed by
// txid and the in-memory rank guard has already decided what the status is.
func (e *Engine) writeStatuses(ctx context.Context) {
	for {
		batch, ok := e.collectStatusWrites(ctx)
		if len(batch) > 0 {
			// Not ctx: a cancelled context here would abandon the final drain,
			// and these writes are the record of statuses already applied in
			// memory. Bounded instead by its own timeout.
			writeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := e.store.UpdateStatusBatch(writeCtx, batch); err != nil {
				e.logger.Error("persist statuses", "count", len(batch), "err", err)
			}
			cancel()
		}
		if !ok {
			return
		}
	}
}

// collectStatusWrites drains one batch. It reports false once the context is
// done and the queue is empty, having returned whatever was still in flight.
//
// Duplicate txids within a batch are collapsed to the last write, which is the
// one the rank guard settled on — and which keeps UpdateStatusBatch's VALUES
// join matching at most one row per txid.
func (e *Engine) collectStatusWrites(ctx context.Context) ([]history.StatusUpdate, bool) {
	var (
		batch []history.StatusUpdate
		seen  map[string]int
	)
	add := func(u history.StatusUpdate) {
		if seen == nil {
			seen = make(map[string]int, statusWriteMax)
		}
		if i, dup := seen[u.TxID]; dup {
			batch[i] = u
			return
		}
		seen[u.TxID] = len(batch)
		batch = append(batch, u)
	}

	// Block for the first one; there is nothing to write until it arrives.
	select {
	case <-ctx.Done():
		// Drain what is already queued, then stop.
		for {
			select {
			case u := <-e.statusWrites:
				add(u)
				if len(batch) >= statusWriteMax {
					return batch, true
				}
			default:
				return batch, false
			}
		}
	case u := <-e.statusWrites:
		add(u)
	}

	linger := time.NewTimer(statusWriteLinger)
	defer linger.Stop()
	for len(batch) < statusWriteMax {
		select {
		case u := <-e.statusWrites:
			add(u)
		case <-linger.C:
			return batch, true
		case <-ctx.Done():
			return batch, true
		}
	}
	return batch, true
}

// rejectionMessage renders arcade's verdict for display and for the store.
func rejectionMessage(status arcade.Status, extra string) string {
	msg := "arcade: " + string(status)
	if extra != "" {
		msg += ": " + extra
	}
	return msg
}

// indexTx remembers which cell a txid belongs to. Callers must hold the lock.
func (e *Engine) indexTx(txid string, generation uint64, cell int) {
	if e.txIndex == nil {
		e.txIndex = make(map[string]txLoc)
	}
	e.txIndex[txid] = txLoc{generation: generation, cell: cell}
}

const (
	// reconcileAfter is how long a transaction must have been in flight before
	// the reconciler polls it. Below this the status stream is still the fast
	// path and polling only duplicates its work.
	reconcileAfter = 90 * time.Second

	// reconcileLimit bounds one pass, and reconcileWorkers bounds how many
	// arcade lookups are in flight at once. Serially, a pass over a large
	// in-flight set takes hours; the point of the reconciler is to converge on
	// arcade, and a pass that never finishes converges on nothing.
	reconcileLimit   = 512
	reconcileWorkers = 16
)

// reconcileBatch polls arcade for a batch of in-flight transactions.
func (e *Engine) reconcileBatch(ctx context.Context, pending []history.CellTx) {
	if len(pending) == 0 {
		return
	}
	work := make(chan history.CellTx)
	// Collect what the poll finds and apply it as one batch at the end, rather
	// than having 16 workers each take the engine's write lock per transaction.
	// A pass of reconcileLimit used to be a write-lock storm on top of the live
	// stream, every tick.
	var (
		mu    sync.Mutex
		found []arcade.TxRecord
	)
	var wg sync.WaitGroup
	for range min(reconcileWorkers, len(pending)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range work {
				rec, err := e.chain.Oracle.GetTx(ctx, c.TxID)
				if err != nil || rec == nil {
					continue // not known to arcade yet; the stream may still deliver it
				}
				mu.Lock()
				found = append(found, *rec)
				mu.Unlock()
			}
		}()
	}
	for _, c := range pending {
		select {
		case <-ctx.Done():
			close(work)
			wg.Wait()
			e.applyStatusBatch(found)
			return
		case work <- c:
		}
	}
	close(work)
	wg.Wait()
	e.applyStatusBatch(found)
}
