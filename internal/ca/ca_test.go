package ca

import (
	"fmt"
	"testing"
)

// TestRule110TruthTable pins the rule against the canonical table, written out
// independently of the bit arithmetic in Rule.Next.
func TestRule110TruthTable(t *testing.T) {
	want := map[[3]int]int{
		{1, 1, 1}: 0,
		{1, 1, 0}: 1,
		{1, 0, 1}: 1,
		{1, 0, 0}: 0,
		{0, 1, 1}: 1,
		{0, 1, 0}: 1,
		{0, 0, 1}: 1,
		{0, 0, 0}: 0,
	}
	for nbhd, expect := range want {
		if got := Rule110.Next(nbhd[0], nbhd[1], nbhd[2]); got != expect {
			t.Errorf("Rule110.Next%v = %d, want %d", nbhd, got, expect)
		}
	}
}

// TestMatchesReferenceImplementation cross-checks against the algorithm from
// github.com/dymurray/rule-110-cellular-automata, reproduced here verbatim in
// spirit: 64 cells in a uint64, zero-padded edges, rule dispatched by
// formatting the neighbourhood into a string.
//
// That reference uses dead edges while this package uses a ring, so the two
// only have to agree while the pattern has not yet reached the boundary. Seeded
// at cell 0 the pattern grows toward higher indices, so the first 60
// generations are safely inside that window.
func TestMatchesReferenceImplementation(t *testing.T) {
	const cells = 64

	row, err := SeedSingle(cells)
	if err != nil {
		t.Fatal(err)
	}
	var ref uint64 = 0x1

	for gen := range 60 {
		if got := rowToUint64(row); got != ref {
			t.Fatalf("generation %d diverged\n  ours: %064b\n  ref:  %064b", gen, got, ref)
		}
		row = Rule110.Step(row)
		ref = referenceStep(ref)
	}
}

// referenceStep is the upstream implementation's generateNextAutomata, kept in
// its original shape (string-keyed truth table, shift-based neighbourhood,
// zero-padded edges) so it is a genuinely independent check rather than a
// restatement of ours.
func referenceStep(prev uint64) uint64 {
	out := prev
	for i := 63; i >= 0; i-- {
		middle := int((prev >> uint(i)) & 1)
		left := int((prev >> uint(i+1)) & 1) // i=63 shifts by 64 -> 0
		var right int
		if i > 0 {
			right = int((prev >> uint(i-1)) & 1)
		} // i=0 -> right edge reads dead
		if referenceRule(left, middle, right) == 1 {
			out |= 1 << uint(i)
		} else {
			out &^= 1 << uint(i)
		}
	}
	return out
}

func referenceRule(left, middle, right int) int {
	switch fmt.Sprintf("%d%d%d", left, middle, right) {
	case "001", "010", "011", "101", "110":
		return 1
	default: // 000, 100, 111
		return 0
	}
}

func rowToUint64(r Row) uint64 {
	var v uint64
	for i := range r.Cells() {
		if r.Get(i) {
			v |= 1 << uint(i)
		}
	}
	return v
}

// TestRingWrap checks the boundary cells genuinely see each other.
func TestRingWrap(t *testing.T) {
	const n = 16

	// Only cell n-1 alive. Cell 0 sees it as its right neighbour: (0,0,1) -> 1.
	row, _ := NewRow(n)
	row.Set(n-1, true)
	if next := Rule110.Step(row); !next.Get(0) {
		t.Errorf("cell 0 did not see cell %d as its right neighbour", n-1)
	}

	// Only cell 0 alive. Cell n-1 sees it as its left neighbour: (1,0,0) -> 0,
	// while cell 0 itself is (0,1,0) -> 1.
	row2, _ := NewRow(n)
	row2.Set(0, true)
	next2 := Rule110.Step(row2)
	if next2.Get(n - 1) {
		t.Errorf("cell %d should be dead: neighbourhood (1,0,0) -> 0", n-1)
	}
	if !next2.Get(0) {
		t.Error("cell 0 should be alive: neighbourhood (0,1,0) -> 1")
	}
}

func TestNeighbourIndicesWrap(t *testing.T) {
	const n = 8
	if got := LeftIndex(n, n-1); got != 0 {
		t.Errorf("LeftIndex(%d, %d) = %d, want 0", n, n-1, got)
	}
	if got := RightIndex(n, 0); got != n-1 {
		t.Errorf("RightIndex(%d, 0) = %d, want %d", n, got, n-1)
	}
}

// TestStepDoesNotMutateInput guards the classic cellular-automaton bug of
// reading half-updated state.
func TestStepDoesNotMutateInput(t *testing.T) {
	row, _ := SeedSingle(32)
	before := row.Clone()
	Rule110.Step(row)
	if !row.Equal(before) {
		t.Errorf("Step mutated its input: %s -> %s", before, row)
	}
}

func TestNewRowRejectsBadSizes(t *testing.T) {
	for _, n := range []int{0, -8, 7, 12, 130} {
		if _, err := NewRow(n); err == nil {
			t.Errorf("NewRow(%d) should have failed", n)
		}
	}
}

func TestSeedHexRoundTrip(t *testing.T) {
	row, err := SeedHex(16, "0180")
	if err != nil {
		t.Fatal(err)
	}
	if !row.Get(0) {
		t.Error("cell 0 should be alive")
	}
	if !row.Get(15) {
		t.Error("cell 15 should be alive")
	}
	if got := row.Hex(); got != "0180" {
		t.Errorf("Hex() = %q, want %q", got, "0180")
	}
	if _, err := SeedHex(16, "01"); err == nil {
		t.Error("SeedHex should reject a short seed")
	}
}
