package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/dymurray/rule-110-arcade/internal/history"
)

const (
	// persistLinger is how long the committer waits for more rows once the
	// queue runs dry, before writing what it has.
	//
	// Short, because every cell in the batch is BLOCKED on it. This is not the
	// status writer, where lingering costs nothing because nobody is waiting:
	// here the linger is added directly to the latency of a transition that has
	// not been built yet. Two milliseconds is enough to collect a generation's
	// worth of cells — they are released together by one clock tick — while
	// costing a lone cell on a quiet ring almost nothing.
	persistLinger = 2 * time.Millisecond

	// persistBatchMax bounds one statement, at one generation's worth of rows.
	persistBatchMax = 128

	// persistQueueSize is the hand-off buffer. A generation can offer 128 rows
	// at once and a slow commit must not make the 129th caller wait on the
	// channel rather than on the database.
	persistQueueSize = 512
)

// persistRequest is one durable write and the caller blocked on it.
type persistRequest struct {
	row history.CellTx
	// done carries the commit's verdict back. Buffered, so the committer never
	// blocks on a caller that has gone away.
	done chan error
}

// persist writes a cell transaction to the durable store, batched with whatever
// else is being written at the same moment, and reports whether it landed.
//
// # It still blocks until the row is durable
//
// This is a group commit, not a queue. The caller waits for the COMMIT, so the
// guarantee the write-ahead record exists for is unchanged: when this returns
// true the row is in the store, and the caller may broadcast. What changed is
// that 128 cells reaching this point together now cost one round trip between
// them instead of 128 — a generation used to spend 256 individual round trips
// here, every one with a cell worker stopped on it.
//
// # Why a failure still halts the cell
//
// Unchanged, and it is the whole reason this is worth being careful about:
//
//   - the write-ahead `attempting` record is the ONLY thing that says a
//     transition might have been broadcast. Losing it means the next startup
//     sees a healthy cell and re-spends an output that may already be gone;
//   - the `broadcast` record is the only record of where the cell IS. Losing it
//     means the next startup derives the tip one generation back and spends an
//     output the network has already consumed.
//
// So a cell whose record cannot be written stops advancing — visibly, with the
// error attached — rather than running on with no record of what it did. A
// caller about to broadcast MUST check the result: halting a cell stops its NEXT
// turn, it does not unwind the call already in flight.
//
// Batching does mean one cell's commit failure is shared by everything batched
// with it, so a failure halts all of them. That is the correct direction to be
// wrong in: the alternative is deciding, on no evidence, that some rows in a
// failed transaction landed anyway.
func (e *Engine) persist(c history.CellTx) bool {
	req := persistRequest{row: c, done: make(chan error, 1)}

	select {
	case e.persistQueue <- req:
	case <-e.persistStopped:
		return e.persistAbandoned(c)
	}

	// Waiting on the reply ALONE would hang. The queue is buffered, so a send
	// racing the committer's final drain succeeds into a buffer nobody will read
	// again, and this caller would block for ever on a reply that cannot come.
	var err error
	select {
	case err = <-req.done:
	case <-e.persistStopped:
		// The committer answers everything it is holding before it closes this,
		// so a reply may already be waiting even though both cases are ready.
		select {
		case err = <-req.done:
		default:
			return e.persistAbandoned(c)
		}
	}
	if err == nil {
		return true
	}
	e.logger.Error("persist cell transaction; halting the cell",
		"generation", c.Generation, "cell", c.Cell, "err", err)
	e.haltCell(c.Cell, fmt.Sprintf(
		"generation %d could not be recorded (%v), so this cell's position is no longer durable and "+
			"advancing it would risk re-spending a spent output", c.Generation, err))
	return false
}

// persistAbandoned reports a write that could not be committed because the
// process is shutting down.
//
// Deliberately not a halt. A halt is a durable verdict about a cell — it is what
// the UI shows and what an operator has to clear — and inventing one on the way
// out would make every clean shutdown look like damage at the next startup. The
// caller still gets false and so still does not broadcast, which is the part
// that matters.
func (e *Engine) persistAbandoned(c history.CellTx) bool {
	e.logger.Warn("not recording a cell transaction; the store committer has stopped",
		"generation", c.Generation, "cell", c.Cell)
	return false
}

// commitTxs is the single writer behind persist. Started by Run.
//
// One goroutine, so rows for one cell commit in the order the cell asked for
// them — which matters, because `attempting` and `broadcast` for the same
// (generation, cell) are the same row written twice and the later one must win.
func (e *Engine) commitTxs(ctx context.Context) {
	defer close(e.persistStopped)

	for {
		batch, more := e.collectPersists(ctx)
		if len(batch) > 0 {
			err := e.commitBatch(batch)
			for _, req := range batch {
				req.done <- err
			}
		}
		if !more {
			return
		}
	}
}

// collectPersists drains one batch. It reports false once the context is done
// and the queue is empty, having returned whatever was still waiting — those
// callers are blocked on a reply and must get one.
func (e *Engine) collectPersists(ctx context.Context) ([]persistRequest, bool) {
	var batch []persistRequest

	select {
	case <-ctx.Done():
		for {
			select {
			case req := <-e.persistQueue:
				batch = append(batch, req)
				if len(batch) >= persistBatchMax {
					return batch, true
				}
			default:
				return batch, false
			}
		}
	case req := <-e.persistQueue:
		batch = append(batch, req)
	}

	linger := time.NewTimer(persistLinger)
	defer linger.Stop()
	for len(batch) < persistBatchMax {
		select {
		case req := <-e.persistQueue:
			batch = append(batch, req)
		case <-linger.C:
			return batch, true
		case <-ctx.Done():
			return batch, true
		}
	}
	return batch, true
}

// commitBatch writes one batch, retrying briefly.
//
// Short and few, as before. This runs with every cell in the batch blocked on
// it, and the failures worth riding out here are the momentary ones — a
// connection recycled, a lock contended. Anything longer-lived will not clear
// inside a retry loop, and the correct response is to stop those cells rather
// than to keep advancing while their records go unwritten.
//
// Not the caller's context: a cancelled context here would abandon rows whose
// callers are still waiting to be told whether they may broadcast, and "we do
// not know" is the one answer that cannot be acted on safely. Bounded by its own
// timeout instead.
func (e *Engine) commitBatch(batch []persistRequest) error {
	rows := make([]history.CellTx, len(batch))
	for i, req := range batch {
		rows[i] = req.row
	}

	var err error
	for attempt := 1; attempt <= persistRetries; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
		err = e.store.RecordTxBatch(ctx, rows)
		cancel()
		if err == nil {
			e.perf.persistRows.Add(uint64(len(rows)))
			e.perf.persistBatches.Inc()
			return nil
		}
		e.logger.Warn("persist cell transactions failed; retrying",
			"rows", len(rows), "attempt", attempt, "err", err)
		time.Sleep(persistBackoff)
	}
	return err
}
