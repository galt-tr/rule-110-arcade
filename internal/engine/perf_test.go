package engine

import (
	"testing"
	"time"

	"github.com/dymurray/rule-110-arcade/internal/chain"
)

// perfEngine is the smallest engine the generation-timing code needs: the
// instruments, the stamp map, and MaxLag, which bounds that map.
func perfEngine(maxLag uint64) *Engine {
	return &Engine{
		chain:    &chain.Chain{Config: chain.Config{MaxLag: maxLag}},
		perf:     newPerf(),
		raisedAt: map[uint64]time.Time{},
	}
}

func TestGenerationIsMeasuredFromWhenItWasAskedFor(t *testing.T) {
	e := perfEngine(32)

	e.noteTargetRaisedLocked(1)
	time.Sleep(20 * time.Millisecond)
	done, ok := e.observeFrontierLocked(0, 1)

	if !ok {
		t.Fatal("the frontier reaching generation 1 was not reported as complete")
	}
	if done.number != 1 {
		t.Errorf("done.number = %d, want 1", done.number)
	}
	if done.took < 20*time.Millisecond {
		t.Errorf("took = %v, want at least the 20ms that elapsed", done.took)
	}
	if got := e.perf.generation.Count(); got != 1 {
		t.Errorf("generation histogram count = %d, want 1", got)
	}
	if got := e.perf.generationsTotal.Value(); got != 1 {
		t.Errorf("generations counter = %d, want 1", got)
	}
}

// The clock's underflow guard and SetMode both re-seed the target after a halt.
// If a re-seed restarted the stopwatch, a generation already in flight would be
// reported as having taken however long was left rather than how long it took.
func TestReSeedingATargetKeepsTheFirstStamp(t *testing.T) {
	e := perfEngine(32)

	e.noteTargetRaisedLocked(1)
	first := e.raisedAt[1]
	time.Sleep(10 * time.Millisecond)
	e.noteTargetRaisedLocked(1)

	if got := e.raisedAt[1]; !got.Equal(first) {
		t.Errorf("the stamp moved from %v to %v; a re-seed must not restart the clock", first, got)
	}
}

// Halting the slowest cell removes it from the minimum, so the frontier can
// jump several generations at once. Those really are complete among the cells
// that remain, and dropping them would make the histogram stop counting after
// the first halt.
func TestAFrontierJumpRecordsEveryGenerationItPassed(t *testing.T) {
	e := perfEngine(32)
	for g := uint64(1); g <= 4; g++ {
		e.noteTargetRaisedLocked(g)
	}

	done, ok := e.observeFrontierLocked(0, 4)

	if !ok {
		t.Fatal("a frontier jump reported nothing complete")
	}
	if done.number != 4 {
		t.Errorf("done.number = %d, want the newest, 4", done.number)
	}
	if got := e.perf.generation.Count(); got != 4 {
		t.Errorf("generation histogram count = %d, want 4", got)
	}
	if len(e.raisedAt) != 0 {
		t.Errorf("stamps left behind: %v", e.raisedAt)
	}
}

// A generation nobody stamped is still a generation completed, but its duration
// is unknown — and measuring it from whenever the target last moved would be
// worse than not measuring it.
func TestAnUnstampedGenerationIsCountedButNotTimed(t *testing.T) {
	e := perfEngine(32)

	_, ok := e.observeFrontierLocked(0, 1)

	if ok {
		t.Error("an unstamped generation must not be reported as timed")
	}
	if got := e.perf.generationsTotal.Value(); got != 1 {
		t.Errorf("generations counter = %d, want 1 — it still completed", got)
	}
	if got := e.perf.generation.Count(); got != 0 {
		t.Errorf("generation histogram count = %d, want 0 — its duration is unknown", got)
	}
}

func TestFrontierGoingNowhereRecordsNothing(t *testing.T) {
	e := perfEngine(32)
	e.noteTargetRaisedLocked(5)

	if _, ok := e.observeFrontierLocked(5, 5); ok {
		t.Error("a frontier that did not move reported a completed generation")
	}
	if got := e.perf.generationsTotal.Value(); got != 0 {
		t.Errorf("generations counter = %d, want 0", got)
	}
}

// Every cell halting while targets are outstanding leaves stamps nothing will
// ever collect. The map must not become the leak in a long run.
func TestStampsDoNotAccumulateForGenerationsThatNeverComplete(t *testing.T) {
	e := perfEngine(4)

	for g := uint64(1); g <= 500; g++ {
		e.noteTargetRaisedLocked(g)
	}
	// The frontier finally moves to 400: everything at or below it is finished
	// with, whether or not it was ever observed.
	e.observeFrontierLocked(0, 400)

	if len(e.raisedAt) > 100 {
		t.Errorf("raisedAt holds %d stamps after the frontier passed them", len(e.raisedAt))
	}
	for g := range e.raisedAt {
		if g <= 400 {
			t.Errorf("stamp for generation %d survived the frontier reaching 400", g)
		}
	}
}

// The first acknowledgement is measured once per transaction. Folding a later
// Seen -> Mined into it would mix block intervals into a distribution read as
// "is the diagram live?".
func TestStatusLagMeasuresTheFirstAcknowledgementOnly(t *testing.T) {
	e := perfEngine(32)
	loc := txLoc{generation: 1, cell: 0, broadcastAt: time.Now().Add(-50 * time.Millisecond)}

	e.observeStatusLag(loc, TxBroadcast, TxSeen)
	e.observeStatusLag(loc, TxSeen, TxMined)

	if got := e.perf.statusLag.Count(); got != 1 {
		t.Errorf("statusLag count = %d, want 1 — only the first acknowledgement", got)
	}
	if got := e.perf.minedLag.Count(); got != 1 {
		t.Errorf("minedLag count = %d, want 1", got)
	}
	if got := e.perf.statusesTotal.Value(); got != 2 {
		t.Errorf("statuses counter = %d, want 2", got)
	}
}

// Transactions re-indexed at startup were broadcast by an earlier process, so
// their lag is unknowable. Measuring it would report "however long this process
// has been up" — wrong, and wrong in the flattering direction.
func TestStatusLagIsNotMeasuredForAnUnknownBroadcastTime(t *testing.T) {
	e := perfEngine(32)

	e.observeStatusLag(txLoc{generation: 1, cell: 0}, TxBroadcast, TxMined)

	if got := e.perf.statusLag.Count(); got != 0 {
		t.Errorf("statusLag count = %d, want 0", got)
	}
	if got := e.perf.minedLag.Count(); got != 0 {
		t.Errorf("minedLag count = %d, want 0", got)
	}
	if got := e.perf.statusesTotal.Value(); got != 1 {
		t.Errorf("statuses counter = %d, want 1 — it was still applied", got)
	}
}
