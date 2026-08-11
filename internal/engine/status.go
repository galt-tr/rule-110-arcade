package engine

import (
	"context"
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

// watchStatus subscribes to arcade's status stream and reflects it in the UI.
//
// Without this the engine only ever knows that a transaction was ACCEPTED at
// intake; it never learns that the network saw it, mined it, or rejected it. A
// rejected cell would keep showing as broadcast and, worse, the engine would go
// on trying to spend an output that does not exist.
func (e *Engine) watchStatus(ctx context.Context) {
	// lastEventID is the replay cursor. Reconnecting with an empty cursor would
	// resume from "now" and silently drop every event that fired while we were
	// disconnected — precisely the updates most likely to matter, since a
	// dropped stream and a burst of status changes tend to coincide.
	lastEventID := ""

	for {
		err := e.chain.Oracle.StreamStatus(ctx, lastEventID, func(ev arcade.StatusEvent) error {
			if ev.ID != "" {
				lastEventID = ev.ID
			}
			e.applyStatus(ev.Record.TxID, ev.Record.Status, ev.Record.ExtraInfo)
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		e.logger.WarnContext(ctx, "arcade status stream ended, reconnecting",
			"err", err, "resumeFrom", lastEventID)
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
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
		pending, err := e.store.Unsettled(ctx)
		if err != nil {
			e.logger.ErrorContext(ctx, "load unsettled", "err", err)
			continue
		}
		for _, c := range pending {
			txid := c.TxID
			if txid == "" {
				continue
			}
			rec, err := e.chain.Oracle.GetTx(ctx, txid)
			if err != nil || rec == nil {
				continue // not known to arcade yet; the stream may still deliver it
			}
			e.applyStatus(rec.TxID, rec.Status, rec.ExtraInfo)
		}
	}
}

// applyStatus records a status update against whichever cell owns the txid.
func (e *Engine) applyStatus(txid string, status arcade.Status, extra string) {
	next, ok := stateFor(status)
	if !ok {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	loc, known := e.txIndex[txid]
	if !known {
		return // not one of ours
	}
	genIdx := indexOfGeneration(e.history, loc.generation)
	if genIdx < 0 || loc.cell >= len(e.history[genIdx].Cells) {
		// Trimmed out of the in-memory window. The store still has it, so
		// persist the status even though nothing is left to redraw.
		e.persistStatus(txid, next, status, extra)
		return
	}
	cell := &e.history[genIdx].Cells[loc.cell]
	if rank(next) <= rank(cell.State) {
		return
	}

	cell.State = next

	// A mined transaction shortens that cell's unconfirmed chain, which is what
	// releases its depth gate. Without this the governor would clamp every cell
	// shut after MaxUnconfirmedDepth generations and never reopen.
	if next == TxMined && loc.generation > e.lastMined[loc.cell] {
		e.lastMined[loc.cell] = loc.generation
	}

	// Persist before anything else: this status is the fact worth keeping.
	e.persistStatus(txid, next, status, extra)

	if next == TxFailed {
		cell.Err = rejectionMessage(status, extra)
		// Halt the cell. Its successor UTXO does not exist, so every later
		// generation would try to spend a phantom output and fail with
		// "utxo already spent" — noise that hides the original rejection.
		e.halted[loc.cell] = true
		e.logger.Warn("cell halted by rejection", "cell", loc.cell, "txid", txid, "status", string(status))
	}
	e.notify()
}

// persistStatus writes a status to the store and drops the transaction from the
// live index once it can no longer change. Callers must hold the write lock.
func (e *Engine) persistStatus(txid string, next TxState, status arcade.Status, extra string) {
	st := history.Status(next)
	errMsg := ""
	if next == TxFailed {
		errMsg = rejectionMessage(status, extra)
	}
	if err := e.store.UpdateStatus(context.Background(), txid, st, errMsg); err != nil {
		e.logger.Error("persist status", "txid", txid, "err", err)
	}

	// Terminal: arcade has nothing further to say, so stop tracking it. The
	// record stays in the store; only the live index shrinks.
	if st.IsTerminal() {
		delete(e.txIndex, txid)
	}
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
