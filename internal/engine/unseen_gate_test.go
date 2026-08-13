package engine

import (
	"testing"
	"time"
)

// The acceptance gate must have an exit. Without one a status that never
// arrives holds its cell for ever, and "never arrives" is the normal case at
// rate rather than an exotic one: arcade drops events its single-goroutine
// fan-out cannot keep up with, its mid-stream catch-up truncates rather than
// replaying them all, and a backlogged transaction skips the seen statuses
// entirely on its way to MINED.
//
// This is the test that separates one slow cell from a stopped automaton.
func TestAcceptanceGateReleasesACellWhoseStatusNeverArrives(t *testing.T) {
	e := newTestEngine(t, 2)
	e.chain.Config.MaxUnseenDepth = 1
	e.chain.Config.UnseenTimeout = 50 * time.Millisecond
	atGeneration(e, 0)

	e.mu.Lock()
	e.mode = ModeRunning
	e.target = 1000
	e.tips[0].Generation = 1
	e.lastSeen[0] = 0 // arcade said 202 and then nothing, ever
	e.mu.Unlock()

	// Before the deadline the gate holds, which is the whole point of it.
	if e.turnReady(0) {
		t.Fatal("the gate released immediately; it is meant to WAIT for the parent")
	}

	deadline := time.Now().Add(2 * time.Second)
	for !e.turnReady(0) {
		if time.Now().After(deadline) {
			t.Fatal("the cell never advanced. A status that is never delivered now " +
				"stops this cell for ever, which is how one dropped SSE frame wedges " +
				"the whole automaton")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := e.perf.unseenTimeouts.Value(); got != 1 {
		t.Errorf("unseen gate timeouts = %d, want 1; the one number that says statuses "+
			"are being LOST rather than merely delayed must be recorded", got)
	}
}

// A released cell must not freewheel. Releasing restarts the deadline, so a cell
// whose statuses are genuinely lost advances once per timeout — still governed,
// just no longer stopped.
func TestAcceptanceGateRearmsAfterReleasingACell(t *testing.T) {
	e := newTestEngine(t, 2)
	e.chain.Config.MaxUnseenDepth = 1
	e.chain.Config.UnseenTimeout = 50 * time.Millisecond
	atGeneration(e, 0)

	e.mu.Lock()
	e.mode = ModeRunning
	e.target = 1000
	e.tips[0].Generation = 1
	e.lastSeen[0] = 0
	e.mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for !e.turnReady(0) {
		if time.Now().After(deadline) {
			t.Fatal("cell never released")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Straight after the release the gate is armed again, not open.
	if e.turnReady(0) {
		t.Error("the gate stayed open after one timeout, so the cell now builds " +
			"unacknowledged generations back to back — the deadline is an escape " +
			"hatch, not an off switch")
	}
}

// The deadline must not fire while the pipeline is healthy. A gate that expires
// under normal operation is not a gate.
func TestAcceptanceGateDoesNotTimeOutWhileStatusesArrive(t *testing.T) {
	e := newTestEngine(t, 2)
	e.chain.Config.MaxUnseenDepth = 1
	e.chain.Config.UnseenTimeout = time.Hour
	atGeneration(e, 0)

	e.mu.Lock()
	e.mode = ModeRunning
	e.target = 1000
	e.tips[0].Generation = 1
	e.lastSeen[0] = 0
	e.mu.Unlock()

	if e.turnReady(0) {
		t.Fatal("gate released early")
	}

	// The acknowledgement lands, as it does in the healthy case.
	e.mu.Lock()
	e.lastSeen[0] = 1
	e.mu.Unlock()

	if !e.turnReady(0) {
		t.Fatal("the gate must reopen on acceptance")
	}
	if got := e.perf.unseenTimeouts.Value(); got != 0 {
		t.Errorf("unseen gate timeouts = %d, want 0; the deadline fired on a cell "+
			"that was acknowledged normally", got)
	}

	// And the timer must have been cleared, or the NEXT refusal would inherit an
	// already-expired deadline and release without waiting at all.
	if got := e.unseenSince[0].Load(); got != 0 {
		t.Errorf("unseenSince[0] = %d after acceptance, want 0; a stale timer makes "+
			"the next refusal expire instantly", got)
	}
}

// Zero means wait for ever, matching MaxUnseenDepth's own zero-is-unbounded
// convention. An operator must be able to ask for the old behaviour.
func TestAcceptanceGateDeadlineDisabledByZero(t *testing.T) {
	e := newTestEngine(t, 2)
	e.chain.Config.MaxUnseenDepth = 1
	e.chain.Config.UnseenTimeout = 0
	atGeneration(e, 0)

	e.mu.Lock()
	e.mode = ModeRunning
	e.target = 1000
	e.tips[0].Generation = 1
	e.lastSeen[0] = 0
	e.mu.Unlock()

	for range 5 {
		if e.turnReady(0) {
			t.Fatal("UnseenTimeout=0 must mean wait for ever, not release immediately")
		}
		time.Sleep(time.Millisecond)
	}
}

// A blocked cell has to wake for its own deadline. It cannot rely on notify():
// the deadline exists for the case where no status arrives, and no status means
// no notification — and when enough cells are stuck the clock stops raising the
// target, so nothing else wakes anybody either. That is the shape of the wedge.
func TestBlockedCellSchedulesItsOwnDeadlineWakeup(t *testing.T) {
	e := newTestEngine(t, 2)
	e.chain.Config.MaxUnseenDepth = 1
	e.chain.Config.UnseenTimeout = time.Minute
	atGeneration(e, 0)

	e.mu.Lock()
	e.mode = ModeRunning
	e.target = 1000
	e.tips[0].Generation = 1
	e.lastSeen[0] = 0
	e.mu.Unlock()

	if d := e.wakeupDelay(0, false); d != 0 {
		t.Errorf("wakeup delay = %v before the gate has refused anything, want 0", d)
	}

	e.turnReady(0) // first refusal starts the clock

	d := e.wakeupDelay(0, false)
	if d <= 0 || d > time.Minute {
		t.Errorf("wakeup delay = %v, want a positive delay no larger than the timeout; "+
			"a blocked cell that schedules no wakeup sleeps through its own deadline", d)
	}

	// The depth gate's blanket poll still wins when it is armed and sooner.
	if got := e.wakeupDelay(0, true); got != depthPoll {
		t.Errorf("wakeup delay with the depth gate armed = %v, want %v", got, depthPoll)
	}
}
