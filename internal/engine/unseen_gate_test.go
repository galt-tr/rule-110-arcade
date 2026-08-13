package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"
)

// scriptedOracle answers GetTx from a table, or with a fixed error. It is the
// whole of the network for these tests.
type scriptedOracle struct {
	status map[string]arcade.Status
	err    error
	asked  int
}

func (o *scriptedOracle) GetTx(_ context.Context, txid string) (*arcade.TxRecord, error) {
	o.asked++
	if o.err != nil {
		return nil, o.err
	}
	st, ok := o.status[txid]
	if !ok {
		return nil, arcade.ErrTxNotFound
	}
	return &arcade.TxRecord{TxID: txid, Status: st}, nil
}

// gatedCell builds an engine with one cell held by the acceptance gate: its tip
// is a broadcast transaction the network has said nothing about.
func gatedCell(t *testing.T, o *scriptedOracle, timeout time.Duration) (*Engine, string) {
	t.Helper()
	const txid = "aa"
	e := newTestEngine(t, 2, 1)
	e.chain.Config.MaxUnseenDepth = 1
	e.chain.Config.UnseenTimeout = timeout
	e.oracle = o
	atGeneration(e, 0)

	e.mu.Lock()
	e.mode = ModeRunning
	e.target = 1000
	e.tips[0].Generation = 1
	e.tips[0].TxID = txid
	e.lastSeen[0] = 0
	e.history[0].Cells[0] = CellTx{Cell: 0, TxID: txid, State: TxBroadcast}
	e.indexTx(txid, 1, 0, time.Now())
	e.mu.Unlock()

	if e.turnReady(0) {
		t.Fatal("the cell is not gated; the fixture is wrong")
	}
	return e, txid
}

// expireDeadline puts cell 0's gate clock far enough in the past that the
// deadline has certainly passed.
//
// Set explicitly rather than slept for, and — importantly — re-set AFTER a probe
// in the tests below. probeAcceptance restarts the clock on its way out, which
// would otherwise mask the very regression these tests exist to catch: with the
// old timer-releases-the-gate code, `turnReady` right after a probe reads a
// freshly restarted clock and answers "not expired yet" for the wrong reason.
func expireDeadline(e *Engine) {
	e.turnReady(0) // arms the clock on the first refusal
	e.unseenSince[0].Store(time.Now().Add(-time.Hour).UnixNano())
}

// The common case by a wide margin: the transaction was always fine and only
// the event was lost. Of the transactions the live deployment had stuck, 11 of
// 11 and then 242 of 242 came back ACCEPTED_BY_NETWORK.
func TestExpiredGateAsksArcadeBeforeAdvancing(t *testing.T) {
	o := &scriptedOracle{status: map[string]arcade.Status{"aa": arcade.StatusAcceptedByNetwork}}
	e, _ := gatedCell(t, o, time.Millisecond)
	expireDeadline(e)

	e.probeAcceptance(t.Context(), 0)

	if o.asked == 0 {
		t.Error("the gate expired without asking arcade anything")
	}
	// lastSeen is the reason the gate opens — not the clock. Assert it directly,
	// so this cannot pass on a timer the way the old code would have.
	e.mu.RLock()
	seen := e.lastSeen[0]
	e.mu.RUnlock()
	if seen != 1 {
		t.Errorf("lastSeen = %d after arcade confirmed acceptance, want 1", seen)
	}
	if !e.turnReady(0) {
		t.Error("arcade said ACCEPTED_BY_NETWORK and the gate stayed shut; a status " +
			"arcade holds must release the cell exactly as a delivered event would")
	}
}

// The case the blind deadline got wrong, and it is why this change exists. A
// REJECTED parent produces the same silence as a lost event, and building on it
// makes every later generation of the cell invalid too.
func TestExpiredGateRepairsInsteadOfAdvancingOnAReject(t *testing.T) {
	o := &scriptedOracle{status: map[string]arcade.Status{"aa": arcade.StatusRejected}}
	e, _ := gatedCell(t, o, time.Millisecond)
	expireDeadline(e)

	e.probeAcceptance(t.Context(), 0)
	// The gate must stay shut even with the deadline expired again — otherwise
	// the assertion below passes only because the probe restarted the clock.
	expireDeadline(e)

	if e.turnReady(0) {
		t.Error("the cell advanced onto a parent arcade had REJECTED. That is how one " +
			"blind advance becomes a cascade: every later generation spends an output " +
			"that does not exist")
	}
	e.mu.RLock()
	repair := e.needsRepair[0]
	e.mu.RUnlock()
	if !repair {
		t.Error("a rejected parent must schedule a rebuild, not merely refuse to advance")
	}
}

// "We could not ask" is not "yes". This is the branch that must fail closed.
func TestExpiredGateDoesNotAdvanceWhenArcadeIsUnreachable(t *testing.T) {
	o := &scriptedOracle{err: errors.New("dial tcp: connection refused")}
	e, _ := gatedCell(t, o, time.Millisecond)
	expireDeadline(e)

	e.probeAcceptance(t.Context(), 0)
	// The gate must stay shut even with the deadline expired again — otherwise
	// the assertion below passes only because the probe restarted the clock.
	expireDeadline(e)

	if e.turnReady(0) {
		t.Error("the cell advanced while arcade was unreachable. An unanswered question " +
			"is not an acknowledgement")
	}
	if got := e.perf.unseenProbesUnanswered.Value(); got != 1 {
		t.Errorf("unanswered probes = %d, want 1; this is the number to alarm on", got)
	}
}

// A transaction arcade has never heard of is the same answer as no answer: it
// is not evidence that the parent landed.
func TestExpiredGateDoesNotAdvanceOnAnUnknownTransaction(t *testing.T) {
	o := &scriptedOracle{status: map[string]arcade.Status{}} // every lookup is ErrTxNotFound
	e, _ := gatedCell(t, o, time.Millisecond)
	expireDeadline(e)

	e.probeAcceptance(t.Context(), 0)
	// The gate must stay shut even with the deadline expired again — otherwise
	// the assertion below passes only because the probe restarted the clock.
	expireDeadline(e)

	if e.turnReady(0) {
		t.Error("the cell advanced on a transaction arcade has never heard of")
	}
}

// A probe that resolves nothing must wait another deadline rather than spin.
func TestAnUnresolvedProbeRestartsTheClock(t *testing.T) {
	o := &scriptedOracle{err: errors.New("unreachable")}
	e, _ := gatedCell(t, o, time.Hour)

	e.turnReady(0) // arms the clock
	e.probeAcceptance(t.Context(), 0)

	if e.unseenDeadlinePassed(0, time.Hour) {
		t.Error("the deadline is still expired straight after a probe, so the worker " +
			"will probe again immediately and hammer arcade in a loop")
	}
}

// The deadline must not fire while the pipeline is healthy. A gate that expires
// under normal operation is not a gate.
func TestAcceptanceGateDoesNotExpireWhileStatusesArrive(t *testing.T) {
	o := &scriptedOracle{}
	e, _ := gatedCell(t, o, time.Hour)

	// The acknowledgement lands, as it does in the healthy case.
	e.mu.Lock()
	e.lastSeen[0] = 1
	e.mu.Unlock()

	if !e.turnReady(0) {
		t.Fatal("the gate must reopen on acceptance")
	}
	if o.asked != 0 {
		t.Errorf("arcade was polled %d times on a healthy cell; the probe is for the "+
			"deadline, not for every turn", o.asked)
	}
	// And the timer must be cleared, or the NEXT refusal inherits an
	// already-expired deadline and probes instantly.
	if got := e.unseenSince[0].Load(); got != 0 {
		t.Errorf("unseenSince[0] = %d after acceptance, want 0", got)
	}
}

// Zero means wait for ever, matching MaxUnseenDepth's own zero-is-unbounded
// convention. An operator must be able to ask for the old behaviour.
func TestAcceptanceGateDeadlineDisabledByZero(t *testing.T) {
	o := &scriptedOracle{status: map[string]arcade.Status{"aa": arcade.StatusAcceptedByNetwork}}
	e, _ := gatedCell(t, o, 0)

	for range 5 {
		if e.unseenDeadlinePassed(0, 0) {
			t.Fatal("UnseenTimeout=0 must mean wait for ever, not expire immediately")
		}
		time.Sleep(time.Millisecond)
	}
	if o.asked != 0 {
		t.Errorf("arcade was polled %d times with the deadline disabled", o.asked)
	}
}

// A blocked cell has to wake for its own deadline. It cannot rely on notify():
// the deadline exists for the case where no status arrives, and no status means
// no notification — and when enough cells are stuck the clock stops raising the
// target, so nothing else wakes anybody either. That is the shape of the wedge.
func TestBlockedCellSchedulesItsOwnDeadlineWakeup(t *testing.T) {
	o := &scriptedOracle{}
	e, _ := gatedCell(t, o, time.Minute)

	e.unseenSince[0].Store(0)
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
