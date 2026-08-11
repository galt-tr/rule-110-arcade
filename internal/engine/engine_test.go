package engine

import "testing"

// TestIndexOfGeneration covers the lookup that keeps a retried generation from
// appearing twice in the diagram. A generation number repeats whenever an
// attempt leaves a cell unproved, and the retry must find and supersede the
// earlier entry rather than append beside it.
func TestIndexOfGeneration(t *testing.T) {
	gens := []Generation{{Number: 7}, {Number: 8}, {Number: 9}}

	tests := map[string]struct {
		number uint64
		want   int
	}{
		"newest":            {9, 2},
		"middle":            {8, 1},
		"oldest":            {7, 0},
		"beyond the newest": {10, -1},
		"before the oldest": {6, -1},
		"gap":               {0, -1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := indexOfGeneration(gens, tc.number); got != tc.want {
				t.Errorf("indexOfGeneration(%d) = %d, want %d", tc.number, got, tc.want)
			}
		})
	}

	if got := indexOfGeneration(nil, 1); got != -1 {
		t.Errorf("indexOfGeneration(nil, 1) = %d, want -1", got)
	}
}

// TestRankIsTotalAndFailedWins pins the ordering guard against out-of-order SSE
// delivery: a late SEEN must not undo a MINED, and nothing may undo a failure.
func TestRankIsTotalAndFailedWins(t *testing.T) {
	ascending := []TxState{TxPending, TxBroadcast, TxSeen, TxMined, TxFailed}
	for i := 1; i < len(ascending); i++ {
		if rank(ascending[i-1]) >= rank(ascending[i]) {
			t.Errorf("rank(%s) = %d must be below rank(%s) = %d",
				ascending[i-1], rank(ascending[i-1]), ascending[i], rank(ascending[i]))
		}
	}

	// An unrecognised state ranks lowest, so it can never displace a real one.
	if rank(TxState("nonsense")) != rank(TxPending) {
		t.Errorf("an unknown state must rank as low as pending")
	}
}
