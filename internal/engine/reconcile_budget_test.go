package engine

import "testing"

// The pass has to grow while it is coming back full, or the safety net stays
// five times smaller than the rate it is meant to catch: a fixed 512 every 10
// seconds is 51 repairs a second against 256 transactions a second of
// broadcast.
func TestReconcileBudgetGrowsWhileSaturated(t *testing.T) {
	budget := reconcileLimit
	for range 8 {
		budget = nextReconcileBudget(budget, budget) // every pass came back full
	}
	if budget != reconcileMaxLimit {
		t.Errorf("budget after eight saturated passes = %d, want the ceiling %d; a "+
			"reconciler that cannot grow cannot close a gap it is too small for",
			budget, reconcileMaxLimit)
	}
}

// And it has to come back down, or a single busy spell leaves the reconciler
// permanently re-polling a healthy system.
func TestReconcileBudgetDecaysToTheFloor(t *testing.T) {
	budget := reconcileMaxLimit
	for range 8 {
		budget = nextReconcileBudget(budget, 0) // nothing outstanding
	}
	if budget != reconcileLimit {
		t.Errorf("budget after eight empty passes = %d, want the floor %d",
			budget, reconcileLimit)
	}
}

// The floor is a floor and the ceiling is a ceiling. The ceiling is what the
// workers can actually finish inside one interval; below the floor a pass stops
// being worth its own round trip.
func TestReconcileBudgetStaysWithinItsBounds(t *testing.T) {
	if got := nextReconcileBudget(reconcileMaxLimit, reconcileMaxLimit); got != reconcileMaxLimit {
		t.Errorf("saturated at the ceiling = %d, want it held at %d", got, reconcileMaxLimit)
	}
	if got := nextReconcileBudget(reconcileLimit, 0); got != reconcileLimit {
		t.Errorf("empty at the floor = %d, want it held at %d", got, reconcileLimit)
	}
}

// A pass that returns exactly its budget is saturated: there is no way to tell
// "exactly enough" from "more waiting", and guessing the optimistic way makes
// the reconciler blind in precisely the situation it exists for.
func TestReconcileBudgetTreatsAnExactlyFullPassAsSaturated(t *testing.T) {
	if got := nextReconcileBudget(reconcileLimit, reconcileLimit); got <= reconcileLimit {
		t.Errorf("budget after an exactly-full pass = %d, want it to grow past %d",
			got, reconcileLimit)
	}
}
