// Package contract holds the on-chain cellular-automaton cell.
//
// This file is BOTH the Rúnar contract source (compiled to Bitcoin Script) and
// ordinary, runnable Go. Every helper it calls has a real implementation in
// packages/runar-go, so `go test ./contracts` executes the automaton natively
// and exercises exactly the logic the compiler lowers into Script.
package contract

import runar "github.com/icellan/runar/packages/runar-go"

// Cell is one site of an elementary cellular automaton on a ring of N cells.
//
// Each cell's UTXO carries the WHOLE current row and verifies exactly ONE bit
// of the next row — its own. N such transactions therefore prove a complete
// generation, and they are fully independent of each other, which is what lets
// a generation fan out across N parallel UTXO chains.
//
// Because the struct embeds runar.StatefulSmartContract, the compiler injects
// checkPreimage at method entry and the state-continuation output at method
// exit, so the successor UTXO is forced to carry the new row.
//
// Row layout: bit i of the automaton lives in byte i/8, at bit position i%8
// (least-significant bit first within each byte).
type Cell struct {
	runar.StatefulSmartContract

	// Row is the mutable state: RowBytes bytes, one bit per cell.
	Row runar.ByteString

	// The neighbourhood is addressed by six constants baked in at deploy time,
	// two per neighbour: which byte to read, and what to divide by to shift the
	// wanted bit down to position 0.
	//
	// The ring wrap lives ENTIRELY in these constants. Cell 0's right neighbour
	// is cell N-1 and cell N-1's left neighbour is cell 0 — expressed purely as
	// different baked values, costing zero extra opcodes and needing no branch.
	ByteL runar.Int `runar:"readonly"`
	DivL  runar.Int `runar:"readonly"`
	ByteC runar.Int `runar:"readonly"`
	DivC  runar.Int `runar:"readonly"`
	ByteR runar.Int `runar:"readonly"`
	DivR  runar.Int `runar:"readonly"`

	// RowBytes is N/8 — the only well-formedness constraint on a row.
	RowBytes runar.Int `runar:"readonly"`

	// Rule is the Wolfram rule number, on-chain as data. 110 is Rule 110.
	// Bit `nbhd` of this byte IS the transition function, so the automaton's
	// entire rule table is a single number living in the locking script.
	Rule runar.Int `runar:"readonly"`
}

// bitAt reads one bit out of a row.
//
// A trailing zero byte is appended before Bin2Num because Script numbers are
// little-endian sign-magnitude: the high bit of the LAST byte is the sign, so
// decoding a lone byte of 0x80..0xFF would yield a negative value. Padding to
// two bytes forces a positive 0..255.
func bitAt(row runar.ByteString, byteIndex runar.Int, div runar.Int) runar.Int {
	b := runar.Bin2Num(runar.Cat(runar.Substr(row, byteIndex, 1), runar.ByteString("\x00")))
	return (b / div) % 2
}

// Step advances this cell by one generation.
//
// It is the ONLY public method, so the compiler emits no method selector and
// the unlocking script is just the pushed arguments.
func (c *Cell) Step(nextRow runar.ByteString) {
	runar.Assert(runar.Len(nextRow) == c.RowBytes)

	l := bitAt(c.Row, c.ByteL, c.DivL)
	ce := bitAt(c.Row, c.ByteC, c.DivC)
	r := bitAt(c.Row, c.ByteR, c.DivR)

	// Select bit (4l + 2c + r) of Rule, branchlessly.
	//
	// Since l, ce and r are each 0 or 1, the divisor factors exactly:
	//     2^(4l + 2ce + r) = 2^4l * 2^2ce * 2^r = (1+15l)(1+3ce)(1+r)
	// which avoids both an 8-way branch and a runtime power. The largest
	// intermediate is 128, so this stays far inside int64 for native Go tests.
	pow := (1 + 15*l) * (1 + 3*ce) * (1 + r)
	next := (c.Rule / pow) % 2

	runar.Assert(bitAt(nextRow, c.ByteC, c.DivC) == next)

	c.Row = nextRow
}
