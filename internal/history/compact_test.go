package history

import (
	"strings"
	"testing"
)

// seed writes `gens` generations of `cells` cells each, all mined except one
// failed cell per generation so the state string has something to distinguish.
func seed(t *testing.T, s *Store, gens int, cells int) {
	t.Helper()
	for g := range gens {
		if err := s.RecordGeneration(t.Context(), uint64(g), strings.Repeat("aa", cells/8)); err != nil {
			t.Fatalf("record generation %d: %v", g, err)
		}
		for c := range cells {
			st := StatusMined
			if c == g%cells {
				st = StatusFailed
			}
			if err := s.RecordTx(t.Context(), CellTx{
				Generation: uint64(g), Cell: c,
				TxID:   strings.Repeat("f", 63) + string(rune('a'+c%26)),
				Status: st,
			}); err != nil {
				t.Fatalf("record tx %d/%d: %v", g, c, err)
			}
		}
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), "", t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestCompactCarriesNoTransactionIDs is the property the whole archive view
// rests on.
//
// Measured on the live deployment: a generation carrying ids is 26,097 bytes
// and 62% of that is the ids, which the UI needs one at a time on click and
// never in bulk. If one leaks back into this payload the archive stops being
// viewable, and it would leak silently.
func TestCompactCarriesNoTransactionIDs(t *testing.T) {
	s := testStore(t)
	seed(t, s, 4, 8)

	gens, err := s.LoadCompact(t.Context(), 0, 10, 8)
	if err != nil {
		t.Fatalf("LoadCompact: %v", err)
	}
	if len(gens) != 4 {
		t.Fatalf("got %d generations, want 4", len(gens))
	}
	// The seeded ids are 63 'f's plus a letter. Nothing of that shape may appear
	// in the payload, and every generation must carry exactly one character per
	// cell — no more.
	for _, g := range gens {
		if strings.Contains(g.States, strings.Repeat("f", 8)) {
			t.Errorf("generation %d states look like a transaction id: %q", g.Number, g.States)
		}
		if len(g.States) != 8 {
			t.Errorf("generation %d has %d state characters, want one per cell (8)", g.Number, len(g.States))
		}
	}
}

// One character per cell, indexed BY cell, so the renderer can colour column c
// from States[c] without a lookup.
func TestCompactStatesAreIndexedByCell(t *testing.T) {
	s := testStore(t)
	seed(t, s, 3, 8)

	gens, err := s.LoadCompact(t.Context(), 0, 10, 8)
	if err != nil {
		t.Fatalf("LoadCompact: %v", err)
	}
	for _, g := range gens {
		failed := int(g.Number) % 8
		for c, ch := range g.States {
			want := byte(StatusMined[0])
			if c == failed {
				want = byte(StatusFailed[0])
			}
			if byte(ch) != want {
				t.Errorf("generation %d cell %d = %q, want %q", g.Number, c, string(ch), string(want))
			}
		}
	}
}

// A cell with no record must be distinguishable from one that is merely
// pending, or a partially written generation would render as a solid row.
func TestCompactMarksMissingCells(t *testing.T) {
	s := testStore(t)
	if err := s.RecordGeneration(t.Context(), 7, "aa"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTx(t.Context(), CellTx{Generation: 7, Cell: 2, TxID: "a", Status: StatusMined}); err != nil {
		t.Fatal(err)
	}

	gens, err := s.LoadCompact(t.Context(), 7, 1, 8)
	if err != nil {
		t.Fatalf("LoadCompact: %v", err)
	}
	if len(gens) != 1 {
		t.Fatalf("got %d generations, want 1", len(gens))
	}
	if got := gens[0].States; got != "--m-----" {
		t.Errorf("states = %q, want %q", got, "--m-----")
	}
}

// The window is the point: an arbitrary slice of the archive, not the tail.
func TestCompactWindowsAnywhereInTheArchive(t *testing.T) {
	s := testStore(t)
	seed(t, s, 50, 8)

	gens, err := s.LoadCompact(t.Context(), 20, 5, 8)
	if err != nil {
		t.Fatalf("LoadCompact: %v", err)
	}
	if len(gens) != 5 {
		t.Fatalf("got %d generations, want 5", len(gens))
	}
	for i, g := range gens {
		if want := uint64(20 + i); g.Number != want {
			t.Errorf("generation[%d] = %d, want %d (ascending from the requested start)", i, g.Number, want)
		}
	}

	// Past the end is empty, not an error: a scrollbar can ask for a window
	// that the frontier has not reached yet.
	past, err := s.LoadCompact(t.Context(), 10_000, 5, 8)
	if err != nil {
		t.Errorf("a window past the end errored: %v", err)
	}
	if len(past) != 0 {
		t.Errorf("got %d generations past the end, want none", len(past))
	}
}

// Extent is what sizes the scrollbar, so it must report the real range and must
// say plainly when there is nothing.
func TestExtentReportsTheArchiveRange(t *testing.T) {
	s := testStore(t)

	if _, _, ok, err := s.Extent(t.Context()); err != nil || ok {
		t.Errorf("empty store reported an extent (ok=%v, err=%v)", ok, err)
	}

	seed(t, s, 12, 8)
	oldest, newest, ok, err := s.Extent(t.Context())
	if err != nil || !ok {
		t.Fatalf("Extent: ok=%v err=%v", ok, err)
	}
	if oldest != 0 || newest != 11 {
		t.Errorf("extent = [%d,%d], want [0,11]", oldest, newest)
	}
}

// The other half of dropping ids from the bulk payload: one cell, on demand —
// and it must carry the refusal reason as well as the id, because a failed cell
// without its reason is a red square with nothing to say.
func TestCellAtIsAPointLookup(t *testing.T) {
	s := testStore(t)
	seed(t, s, 3, 8)

	d, ok, err := s.CellAt(t.Context(), 1, 4)
	if err != nil || !ok {
		t.Fatalf("CellAt: ok=%v err=%v", ok, err)
	}
	if d.TxID == "" {
		t.Error("no transaction id for a cell that has one")
	}
	if d.Status == "" {
		t.Error("no status returned")
	}

	if err := s.UpdateStatus(t.Context(), d.TxID, StatusFailed, "arcade: REJECTED: nope"); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	again, ok, err := s.CellAt(t.Context(), 1, 4)
	if err != nil || !ok {
		t.Fatalf("CellAt after failure: ok=%v err=%v", ok, err)
	}
	if again.Err == "" {
		t.Error("a refused cell reported no reason; that reason is what an outage is diagnosed from")
	}

	if _, ok, err := s.CellAt(t.Context(), 999, 0); err != nil || ok {
		t.Errorf("a generation that never existed reported a transaction (ok=%v, err=%v)", ok, err)
	}
}
