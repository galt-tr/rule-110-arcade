package history

import (
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), "", t.TempDir(), 0)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// loadCell returns the stored row for one cell of one generation.
func loadCell(t *testing.T, s *Store, generation uint64, cell int) (CellTx, bool) {
	t.Helper()
	gens, err := s.Load(t.Context(), 0, 4096)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, g := range gens {
		if g.Number != generation {
			continue
		}
		for _, c := range g.Cells {
			if c.Cell == cell {
				return c, true
			}
		}
	}
	return CellTx{}, false
}

// A batch must leave the store in exactly the state the same rows written one
// at a time would have. It is the write-ahead record: if batching changed what
// lands, it would change what the next startup believes about every cell.
func TestRecordTxBatchMatchesIndividualWrites(t *testing.T) {
	batched, individually := openTestStore(t), openTestStore(t)

	rows := []CellTx{
		{Generation: 5, Cell: 0, Status: StatusAttempting},
		{Generation: 5, Cell: 1, TxID: "aa", Status: StatusBroadcast},
		{Generation: 5, Cell: 2, TxID: "bb", Status: StatusFailed, Err: "refused"},
		{Generation: 6, Cell: 0, TxID: "cc", Status: StatusMined},
	}
	for _, s := range []*Store{batched, individually} {
		if err := s.RecordGeneration(t.Context(), 5, "aa"); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordGeneration(t.Context(), 6, "bb"); err != nil {
			t.Fatal(err)
		}
	}
	if err := batched.RecordTxBatch(t.Context(), rows); err != nil {
		t.Fatalf("batch: %v", err)
	}
	for _, r := range rows {
		if err := individually.RecordTx(t.Context(), r); err != nil {
			t.Fatalf("single: %v", err)
		}
	}

	for _, r := range rows {
		got, ok := loadCell(t, batched, r.Generation, r.Cell)
		want, wantOK := loadCell(t, individually, r.Generation, r.Cell)
		if !ok || !wantOK {
			t.Fatalf("generation %d cell %d: batched=%v individually=%v", r.Generation, r.Cell, ok, wantOK)
		}
		if got.TxID != want.TxID || got.Status != want.Status || got.Err != want.Err {
			t.Errorf("generation %d cell %d: batched %+v, individually %+v",
				r.Generation, r.Cell, got, want)
		}
	}
}

// A batch upserts, exactly as RecordTx does: a retried cell supersedes its
// earlier attempt rather than colliding with it.
func TestRecordTxBatchSupersedesAnEarlierRow(t *testing.T) {
	s := openTestStore(t)
	if err := s.RecordGeneration(t.Context(), 5, "aa"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTx(t.Context(), CellTx{Generation: 5, Cell: 3, Status: StatusAttempting}); err != nil {
		t.Fatal(err)
	}

	if err := s.RecordTxBatch(t.Context(), []CellTx{
		{Generation: 5, Cell: 3, TxID: "dd", Status: StatusBroadcast},
	}); err != nil {
		t.Fatalf("batch: %v", err)
	}

	got, ok := loadCell(t, s, 5, 3)
	if !ok {
		t.Fatal("row vanished")
	}
	if got.Status != StatusBroadcast || got.TxID != "dd" {
		t.Errorf("row = %+v, want the broadcast record", got)
	}
}

// Two rows for one (generation, cell) in a single batch must not fail it.
// PostgreSQL rejects an ON CONFLICT DO UPDATE that touches a row twice, and a
// failed batch halts every unrelated cell in it.
func TestRecordTxBatchToleratesDuplicates(t *testing.T) {
	s := openTestStore(t)
	if err := s.RecordGeneration(t.Context(), 9, "aa"); err != nil {
		t.Fatal(err)
	}

	if err := s.RecordTxBatch(t.Context(), []CellTx{
		{Generation: 9, Cell: 1, Status: StatusAttempting},
		{Generation: 9, Cell: 2, TxID: "xx", Status: StatusBroadcast},
		{Generation: 9, Cell: 1, TxID: "yy", Status: StatusBroadcast}, // supersedes the first
	}); err != nil {
		t.Fatalf("a batch containing a duplicate key failed: %v", err)
	}

	got, ok := loadCell(t, s, 9, 1)
	if !ok {
		t.Fatal("cell 1 row missing")
	}
	if got.Status != StatusBroadcast || got.TxID != "yy" {
		t.Errorf("cell 1 = %+v, want the LAST row for that key", got)
	}
	if other, ok := loadCell(t, s, 9, 2); !ok || other.TxID != "xx" {
		t.Errorf("cell 2 = %+v, want to be unaffected by the duplicate", other)
	}
}

func TestRecordTxBatchOfNothingIsNotAnError(t *testing.T) {
	if err := openTestStore(t).RecordTxBatch(t.Context(), nil); err != nil {
		t.Errorf("empty batch: %v", err)
	}
}

func TestDedupeCellTxsKeepsTheLastPerKeyInOrder(t *testing.T) {
	got := dedupeCellTxs([]CellTx{
		{Generation: 1, Cell: 0, TxID: "a"},
		{Generation: 1, Cell: 1, TxID: "b"},
		{Generation: 1, Cell: 0, TxID: "c"},
		{Generation: 2, Cell: 0, TxID: "d"},
	})

	want := []CellTx{
		{Generation: 1, Cell: 0, TxID: "c"},
		{Generation: 1, Cell: 1, TxID: "b"},
		{Generation: 2, Cell: 0, TxID: "d"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
