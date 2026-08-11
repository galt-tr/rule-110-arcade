package contract

import (
	"testing"

	runar "github.com/icellan/runar/packages/runar-go"
)

// rowOf builds a RowBytes-length row from a list of live cell indices.
func rowOf(rowBytes int, live ...int) runar.ByteString {
	b := make([]byte, rowBytes)
	for _, i := range live {
		b[i/8] |= 1 << (i % 8)
	}
	return runar.ByteString(b)
}

// cellAt wires a Cell for index i on a ring of n cells, with the given rule.
func cellAt(n, i int, rule int64, row runar.ByteString) *Cell {
	rowBytes := n / 8
	li := (i + 1) % n
	ri := ((i-1)%n + n) % n
	return &Cell{
		Row:      row,
		ByteL:    runar.Int(li / 8),
		DivL:     runar.Int(int64(1) << (li % 8)),
		ByteC:    runar.Int(i / 8),
		DivC:     runar.Int(int64(1) << (i % 8)),
		ByteR:    runar.Int(ri / 8),
		DivR:     runar.Int(int64(1) << (ri % 8)),
		RowBytes: runar.Int(rowBytes),
		Rule:     runar.Int(rule),
	}
}

// TestRuleTable is the load-bearing test: it drives all eight neighbourhoods
// through the real contract and checks them against the canonical Rule 110
// truth table, independently of any helper this package defines.
func TestRuleTable(t *testing.T) {
	// Canonical Rule 110: neighbourhood (l,c,r) -> next.
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

	// A ring of 8 puts l/c/r inside one byte; cell 1 has neighbours 2 and 0.
	const n = 8
	for nbhd, expect := range want {
		l, c, r := nbhd[0], nbhd[1], nbhd[2]

		var live []int
		if l == 1 {
			live = append(live, 2) // left neighbour of cell 1
		}
		if c == 1 {
			live = append(live, 1)
		}
		if r == 1 {
			live = append(live, 0) // right neighbour of cell 1
		}

		for _, claimed := range []int{0, 1} {
			cell := cellAt(n, 1, 110, rowOf(n/8, live...))

			var next runar.ByteString
			if claimed == 1 {
				next = rowOf(n/8, 1)
			} else {
				next = rowOf(n / 8)
			}

			err := stepErr(cell, next)
			if claimed == expect && err != nil {
				t.Errorf("nbhd %v -> %d: correct bit rejected: %v", nbhd, expect, err)
			}
			if claimed != expect && err == nil {
				t.Errorf("nbhd %v -> %d: WRONG bit %d accepted", nbhd, expect, claimed)
			}
		}
	}
}

// TestRingWrap checks that the boundary cells really do wrap, which is the
// whole reason the neighbourhood is expressed as baked constants.
func TestRingWrap(t *testing.T) {
	const n = 16

	// Cell 0's right neighbour must be cell n-1. Light only cell n-1: that is
	// neighbourhood (l=0, c=0, r=1) -> 1 under Rule 110.
	c0 := cellAt(n, 0, 110, rowOf(n/8, n-1))
	if err := stepErr(c0, rowOf(n/8, 0)); err != nil {
		t.Errorf("cell 0 did not see cell %d as its right neighbour: %v", n-1, err)
	}

	// Cell n-1's left neighbour must be cell 0. Light only cell 0: that is
	// (l=1, c=0, r=0) -> 0 under Rule 110, so an all-dead next row is correct.
	cN := cellAt(n, n-1, 110, rowOf(n/8, 0))
	if err := stepErr(cN, rowOf(n/8)); err != nil {
		t.Errorf("cell %d did not see cell 0 as its left neighbour: %v", n-1, err)
	}
	// ...and claiming it turns on must be rejected.
	if err := stepErr(cN, rowOf(n/8, n-1)); err == nil {
		t.Errorf("cell %d accepted a wrong bit across the ring boundary", n-1)
	}
}

// TestHighBitBytesDecodePositive guards the sign-magnitude hazard: a byte of
// 0x80..0xFF must not decode as a negative number and corrupt the rule input.
func TestHighBitBytesDecodePositive(t *testing.T) {
	const n = 16
	// Light cells 8..15, so byte 1 is 0xFF — every bit of the high byte set.
	row := rowOf(n/8, 8, 9, 10, 11, 12, 13, 14, 15)
	if row[1] != 0xFF {
		t.Fatalf("test setup wrong: byte 1 = %#x, want 0xff", row[1])
	}

	// Cell 12 sits in that byte with l=c=r=1, which Rule 110 maps to 0.
	cell := cellAt(n, 12, 110, row)
	if err := stepErr(cell, rowOf(n/8)); err != nil {
		t.Errorf("0xff byte mis-decoded: %v", err)
	}
}

// TestRejectsMalformedRowLength ensures a row of the wrong size cannot enter
// the chain, since row length is the only well-formedness constraint.
func TestRejectsMalformedRowLength(t *testing.T) {
	const n = 16
	cell := cellAt(n, 1, 110, rowOf(n/8, 0))
	if err := stepErr(cell, rowOf(n/8+1, 1)); err == nil {
		t.Error("accepted a next row of the wrong length")
	}
}

func TestCompiles(t *testing.T) {
	if err := runar.CompileCheck("Cell.runar.go"); err != nil {
		t.Fatalf("Rúnar compile check failed: %v", err)
	}
}

// stepErr runs Step and converts runar.Assert's panic into an error, so tests
// can assert on rejection as easily as on acceptance.
func stepErr(c *Cell, nextRow runar.ByteString) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = assertionFailed{r}
		}
	}()
	c.Step(nextRow)
	return nil
}

type assertionFailed struct{ v any }

func (a assertionFailed) Error() string { return "assertion failed" }
