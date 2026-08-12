package engine

import (
	"time"

	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/metrics"
)

// perf is every latency this application measures about itself.
//
// It exists as one struct in one file so that the answer to "what do we
// actually know about where the time goes?" is a thing you can read, rather
// than a grep for Observe across three packages. Nothing here is on a code
// path's critical section: an observation is a handful of atomic adds.
//
// The breakdown is chosen to attribute every millisecond of a transition to
// something with an owner. build+verify is our CPU, create+sign is the
// wallet's (and sign alone carries the arcade round trip), and the two persist
// phases are the history store's. If the phases do not add up to advance_total,
// the remainder is lock contention and scheduling — which is itself the answer
// to a question, and why advance_total is measured separately rather than
// summed.
type perf struct {
	reg *metrics.Registry

	// Per-transition phases, in the order they happen.
	persistAttempting *metrics.Histogram
	buildScripts      *metrics.Histogram
	createAction      *metrics.Histogram
	verify            *metrics.Histogram
	sign              *metrics.Histogram
	recordLockWait    *metrics.Histogram
	persistBroadcast  *metrics.Histogram
	advanceTotal      *metrics.Histogram

	// generation is the number being raced: the clock raising a target, to the
	// last cell reaching it.
	generation *metrics.Histogram

	// statusLag is broadcast to the network's first acknowledgement, and
	// minedLag is broadcast to a block. The first is the "is the diagram live?"
	// number; the second is what governs how fast unconfirmed depth grows.
	statusLag *metrics.Histogram
	minedLag  *metrics.Histogram

	// persistRows and persistBatches are the group commit's compression ratio:
	// rows divided by batches is how many cells a round trip served. At 1 it is
	// doing nothing, and something upstream is releasing cells one at a time.
	persistRows    *metrics.Counter
	persistBatches *metrics.Counter

	generationsTotal *metrics.Counter
	transitionsTotal *metrics.Counter
	failuresTotal    *metrics.Counter
	shortfallsTotal  *metrics.Counter
	statusesTotal    *metrics.Counter
}

func newPerf() *perf {
	r := metrics.NewRegistry()
	return &perf{
		reg: r,

		persistAttempting: r.Histogram("rule110_persist_attempting_seconds",
			"Writing the write-ahead record, which blocks the transition until it is durable."),
		buildScripts: r.Histogram("rule110_build_scripts_seconds",
			"Building the locking and unlocking scripts and the sighash preimage."),
		createAction: r.Histogram("rule110_create_action_seconds",
			"Wallet CreateAction: assembling the unsigned transaction and claiming a fuel coin."),
		verify: r.Histogram("rule110_verify_seconds",
			"Running the covenant through the script interpreter before broadcasting."),
		sign: r.Histogram("rule110_sign_action_seconds",
			"Wallet SignAction, which is what broadcasts. The only phase containing the arcade round trip."),
		recordLockWait: r.Histogram("rule110_record_lock_wait_seconds",
			"Waiting for the engine's write lock to record a completed transition."),
		persistBroadcast: r.Histogram("rule110_persist_broadcast_seconds",
			"Writing the broadcast record, which is the only durable note of where a cell is."),
		advanceTotal: r.Histogram("rule110_advance_cell_seconds",
			"One cell's whole transition, end to end. The phases above should nearly account for it."),

		generation: r.Histogram("rule110_generation_seconds",
			"From the clock raising a generation to the last cell reaching it. This is the rate being raced."),

		statusLag: r.Histogram("rule110_status_lag_seconds",
			"From broadcast to the network's first acknowledgement of that transaction."),
		minedLag: r.Histogram("rule110_mined_lag_seconds",
			"From broadcast to that transaction appearing in a block."),

		persistRows: r.Counter("rule110_persist_rows_total",
			"Cell rows written durably. Divided by the batch count, this is how many cells one round trip served."),
		persistBatches: r.Counter("rule110_persist_batches_total",
			"Round trips the durable cell writes were committed in."),

		generationsTotal: r.Counter("rule110_generations_completed_total",
			"Generations every unhalted cell has proved. Its rate is the achieved generation rate, as opposed to the configured one."),
		transitionsTotal: r.Counter("rule110_transitions_broadcast_total",
			"Cell transitions successfully signed and handed to arcade."),
		failuresTotal: r.Counter("rule110_transitions_failed_total",
			"Cell transitions that did not complete, for any reason."),
		shortfallsTotal: r.Counter("rule110_funding_shortfalls_total",
			"Transitions that could not claim a coin and backed off."),
		statusesTotal: r.Counter("rule110_statuses_applied_total",
			"Arcade status records applied to a cell we own."),
	}
}

// observeStep records one successful transition's phase breakdown.
func (p *perf) observeStep(t chain.StepTimings) {
	p.buildScripts.Observe(t.BuildScripts)
	p.createAction.Observe(t.CreateAction)
	p.verify.Observe(t.Verify)
	p.sign.Observe(t.Sign)
	p.transitionsTotal.Inc()
}

// Metrics is the registry behind this engine's latency instruments, for the
// HTTP layer to render. Exported for that and nothing else.
func (e *Engine) Metrics() *metrics.Registry { return e.perf.reg }

// noteTargetRaisedLocked stamps when a generation was first asked for. Callers
// must hold the write lock.
//
// Every place that moves the target calls this, not just the clock: Step raises
// it while paused, and SetMode and the clock's own underflow guard re-seed it
// after a halt. A generation nobody stamped is simply not measured, which is
// the right failure — better a gap in a histogram than a duration measured from
// whenever the target happened to last move.
func (e *Engine) noteTargetRaisedLocked(target uint64) {
	if e.raisedAt == nil {
		e.raisedAt = map[uint64]time.Time{}
	}
	if _, seen := e.raisedAt[target]; seen {
		// Keep the FIRST time it was asked for. A re-seed after a halt must not
		// restart the clock on a generation already in flight.
		return
	}
	e.raisedAt[target] = time.Now()
}

// observeStatusLag records how long the network took to say something about a
// transaction we broadcast. Callers must hold the write lock.
//
// from and to are the cell's states either side of this update, and the caller
// has already established that this is a genuine advance.
//
// Two different questions, so two histograms. statusLag measures the FIRST
// acknowledgement of any kind, which is what "is the diagram live?" means and
// is therefore recorded once per transaction — a later Seen→Mined must not be
// folded into it or the distribution acquires a second mode made of block
// intervals. minedLag is that second mode, kept separately, because now that
// nothing bounds unconfirmed depth it is the figure that says how fast depth
// grows.
func (e *Engine) observeStatusLag(loc txLoc, from, to TxState) {
	e.perf.statusesTotal.Inc()
	if loc.broadcastAt.IsZero() {
		return // broadcast by an earlier process; see txLoc.broadcastAt
	}
	since := time.Since(loc.broadcastAt)
	if from == TxPending || from == TxBroadcast {
		e.perf.statusLag.Observe(since)
	}
	if to == TxMined {
		e.perf.minedLag.Observe(since)
	}
}

// generationDone is one completed generation, reported out of the lock so the
// log line is written without holding it.
type generationDone struct {
	number uint64
	took   time.Duration
}

// observeFrontierLocked records every generation completed by the frontier
// moving from was to now. Callers must hold the write lock.
//
// The frontier is the minimum over unhalted cells, so it advancing past a
// generation means every cell that still can has proved it — which is exactly
// the definition of that generation being done.
//
// It can also jump by more than one, because halting the slowest cell removes
// it from the minimum. Those generations really are complete among the cells
// that remain, so they are recorded rather than dropped; the alternative is a
// histogram that silently stops counting after the first halt.
func (e *Engine) observeFrontierLocked(was, now uint64) (newest generationDone, any bool) {
	if now <= was {
		return generationDone{}, false
	}
	for g := was + 1; g <= now; g++ {
		at, ok := e.raisedAt[g]
		delete(e.raisedAt, g)
		e.perf.generationsTotal.Inc()
		if !ok {
			continue
		}
		took := time.Since(at)
		e.perf.generation.Observe(took)
		newest, any = generationDone{number: g, took: took}, true
	}

	// Bound the map against generations that never complete — every cell halted
	// while a target was outstanding, and nothing will ever delete the stamp.
	// The clock holds the target within maxLag of the frontier, so anything at
	// or below the frontier is finished with by definition.
	if len(e.raisedAt) > int(e.chain.Config.MaxLag)+2 {
		for g := range e.raisedAt {
			if g <= now {
				delete(e.raisedAt, g)
			}
		}
	}
	return newest, any
}

// logGeneration writes the per-generation performance line, at most once a
// second.
//
// Rate limited because it is one line per generation and the whole point of
// this work is to run several generations a second — an unthrottled log would
// be its own load. The quantiles are cumulative since startup rather than
// windowed, which is what makes the line cheap: no ring buffer, no second
// aggregation, just a read of the histograms that are being kept anyway.
func (e *Engine) logGeneration(done generationDone) {
	if e.opts.PerfLog == nil {
		return
	}
	e.perfLogMu.Lock()
	if time.Since(e.lastPerfLog) < time.Second {
		e.perfLogMu.Unlock()
		return
	}
	e.lastPerfLog = time.Now()
	e.perfLogMu.Unlock()

	p := e.perf
	ms := func(h *metrics.Histogram, q float64) float64 { return h.Quantile(q) * 1000 }
	e.opts.PerfLog.Info("generation complete",
		"generation", done.number,
		"took_ms", done.took.Milliseconds(),
		"gen_p50_ms", ms(p.generation, 0.5),
		"gen_p99_ms", ms(p.generation, 0.99),
		"build_p50_ms", ms(p.buildScripts, 0.5),
		"verify_p50_ms", ms(p.verify, 0.5),
		"create_p50_ms", ms(p.createAction, 0.5),
		"sign_p50_ms", ms(p.sign, 0.5),
		"sign_p99_ms", ms(p.sign, 0.99),
		"persist_p50_ms", ms(p.persistAttempting, 0.5),
		"lockwait_p99_ms", ms(p.recordLockWait, 0.99),
		"status_lag_p50_ms", ms(p.statusLag, 0.5),
		"broadcast_total", p.transitionsTotal.Value(),
		"failed_total", p.failuresTotal.Value(),
	)
}
