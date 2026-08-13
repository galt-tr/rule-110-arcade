package engine

import (
	"math"
	"testing"
)

// -full-status looks like a display setting and is not one. Dropping it drops
// ACCEPTED_BY_NETWORK, which is the FIRST status that releases the acceptance
// gate; the next one, SEEN_ON_NETWORK, is 55x further away. At depth 1 the gate
// is in every generation's critical path, so that difference IS the generation
// rate.
//
// This is the arithmetic that stops the two flags being treated as independent.
func TestDroppingFullStatusCollapsesTheRateAtDepthOne(t *testing.T) {
	full := sustainableRate(1, true)
	milestones := sustainableRate(1, false)

	if full < 5 {
		t.Errorf("sustainable rate at depth 1 with full updates = %.2f gen/s, want > 5; "+
			"the acknowledgement is 154ms so the gate should barely bind", full)
	}
	if milestones > 0.2 {
		t.Errorf("sustainable rate at depth 1 on milestones = %.2f gen/s, want < 0.2; "+
			"waiting for SEEN_ON_NETWORK is an 8.5s round trip per generation", milestones)
	}
	if ratio := full / milestones; ratio < 40 {
		t.Errorf("the two differ by %.0fx, want at least 40x. If this shrinks, the "+
			"coupling between -full-status and -max-unseen has changed and the "+
			"defaults should be revisited", ratio)
	}
}

// The escape from that trap is depth, not luck: a deeper gate absorbs a slower
// acknowledgement. This pins the depth at which milestones alone can carry the
// rate the deployment actually wants.
func TestADeeperGateAbsorbsTheSlowerAcknowledgement(t *testing.T) {
	const want = 1.0 // the deployment's target: 1 gen/s at 256 cells

	if got := sustainableRate(8, false); got >= want {
		t.Errorf("depth 8 on milestones = %.2f gen/s; expected it to still fall short "+
			"of %.1f, so the ladder has a reason to keep climbing", got, want)
	}
	if got := sustainableRate(32, false); got < want {
		t.Errorf("depth 32 on milestones = %.2f gen/s, want at least %.1f — this is the "+
			"rung at which -full-status can be turned off and the event volume halved",
			got, want)
	}
}

// Zero means the gate is off, so nothing is capped. Reading it as "depth zero,
// therefore rate zero" would make an unset field look like a hang, which is the
// same trap zero-is-unbounded avoids everywhere else in this config.
func TestNoGateMeansNoCap(t *testing.T) {
	if got := sustainableRate(0, false); !math.IsInf(got, 1) {
		t.Errorf("sustainable rate with the gate disabled = %v, want unbounded", got)
	}
}
