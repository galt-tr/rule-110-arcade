// Package ca is the pure-Go reference implementation of an elementary
// cellular automaton on a ring.
//
// It is the authority the application drives the chain from: rows are computed
// here, and each on-chain transaction then proves one bit of the result. The
// contract in contracts/Cell.runar.go must agree with this package exactly, and
// the neighbour-index helpers here are the single source of truth both use.
package ca

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// Rule is a Wolfram elementary cellular-automaton rule number. Bit n of the
// rule byte gives the next state for neighbourhood n, where the neighbourhood
// is encoded as 4*left + 2*centre + right.
type Rule uint8

// Rule110 is the rule this project exists to run: the simplest known
// Turing-complete elementary cellular automaton.
const Rule110 Rule = 110

// Next returns the rule's output for one neighbourhood. l, c and r must each
// be 0 or 1.
func (rule Rule) Next(l, c, r int) int {
	return int(rule>>(4*l+2*c+r)) & 1
}

// Row is one generation of the automaton: one bit per cell, with cell i stored
// in byte i/8 at bit position i%8 (least-significant bit first).
//
// This layout is not incidental — it is the layout the on-chain contract reads
// with OP_SPLIT and integer division, so it must not change independently.
type Row []byte

// NewRow allocates an all-dead row of n cells. n must be a positive multiple
// of 8, because a row occupies whole bytes on-chain.
func NewRow(n int) (Row, error) {
	if n <= 0 || n%8 != 0 {
		return nil, fmt.Errorf("ca: cell count must be a positive multiple of 8, got %d", n)
	}
	return make(Row, n/8), nil
}

// Cells returns the number of cells in the row.
func (r Row) Cells() int { return len(r) * 8 }

// Get reports whether cell i is alive.
func (r Row) Get(i int) bool { return r[i/8]&(1<<(i%8)) != 0 }

// Set sets cell i alive or dead.
func (r Row) Set(i int, alive bool) {
	if alive {
		r[i/8] |= 1 << (i % 8)
		return
	}
	r[i/8] &^= 1 << (i % 8)
}

// Clone returns an independent copy.
func (r Row) Clone() Row {
	out := make(Row, len(r))
	copy(out, r)
	return out
}

// Hex returns the row as lowercase hex, the form the contract carries as state.
func (r Row) Hex() string { return hex.EncodeToString(r) }

// Equal reports whether two rows are identical.
func (r Row) Equal(other Row) bool {
	if len(r) != len(other) {
		return false
	}
	for i := range r {
		if r[i] != other[i] {
			return false
		}
	}
	return true
}

// String renders the row for humans and logs, highest cell index leftmost so
// the classic Rule 110 triangle grows the way it is usually drawn.
func (r Row) String() string {
	var b strings.Builder
	for i := r.Cells() - 1; i >= 0; i-- {
		if r.Get(i) {
			b.WriteByte('#')
			continue
		}
		b.WriteByte('.')
	}
	return b.String()
}

// LeftIndex returns the index of cell i's left neighbour on a ring of n cells.
//
// "Left" is the next-higher cell index, matching how a row is printed with the
// highest index leftmost. On a ring, cell n-1's left neighbour is cell 0.
func LeftIndex(n, i int) int { return (i + 1) % n }

// RightIndex returns the index of cell i's right neighbour on a ring of n
// cells. Cell 0's right neighbour wraps to cell n-1.
func RightIndex(n, i int) int { return ((i-1)%n + n) % n }

// Step advances the whole row by one generation on a ring.
func (rule Rule) Step(row Row) Row {
	n := row.Cells()
	out := make(Row, len(row))
	for i := range n {
		l, c, r := 0, 0, 0
		if row.Get(LeftIndex(n, i)) {
			l = 1
		}
		if row.Get(i) {
			c = 1
		}
		if row.Get(RightIndex(n, i)) {
			r = 1
		}
		if rule.Next(l, c, r) == 1 {
			out.Set(i, true)
		}
	}
	return out
}

// SeedSingle returns a row of n cells with only cell 0 alive — the classic
// Rule 110 seed, from which the pattern grows toward higher indices.
func SeedSingle(n int) (Row, error) {
	row, err := NewRow(n)
	if err != nil {
		return nil, err
	}
	row.Set(0, true)
	return row, nil
}

// SeedHex parses a row from hex, requiring exactly n cells.
func SeedHex(n int, s string) (Row, error) {
	if n <= 0 || n%8 != 0 {
		return nil, fmt.Errorf("ca: cell count must be a positive multiple of 8, got %d", n)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("ca: parse seed hex: %w", err)
	}
	if len(b) != n/8 {
		return nil, fmt.Errorf("ca: seed is %d bytes, want %d for %d cells", len(b), n/8, n)
	}
	return Row(b), nil
}
