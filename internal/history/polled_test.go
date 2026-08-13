package history

import (
	"testing"
	"time"
)

// The reconciler only writes a row when it LEARNS something, so a transaction
// arcade cannot answer for keeps its timestamp, stays at the head of Stale's
// ordering, and is re-selected at the front of every pass for ever. A handful of
// those hide the entire backlog behind them while the counters insist the
// reconciler is working.
//
// This is the invariant that stops it: a polled row moves to the back of the
// queue whether or not anything was learned.
func TestMarkPolledMovesARowToTheBackOfTheQueue(t *testing.T) {
	s := openTestStore(t)
	if err := s.RecordGeneration(t.Context(), 1, "aa"); err != nil {
		t.Fatal(err)
	}
	for _, r := range []CellTx{
		{Generation: 1, Cell: 0, TxID: "unanswerable", Status: StatusBroadcast},
		{Generation: 1, Cell: 1, TxID: "second", Status: StatusBroadcast},
	} {
		if err := s.RecordTx(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}

	// A pass of one row: without MarkPolled this is the same row every time.
	first, err := s.Stale(t.Context(), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].TxID != "unanswerable" {
		t.Fatalf("first pass = %+v, want the oldest broadcast row", first)
	}

	if err := s.MarkPolled(t.Context(), []string{"unanswerable"}); err != nil {
		t.Fatalf("mark polled: %v", err)
	}

	second, err := s.Stale(t.Context(), 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].TxID != "second" {
		t.Errorf("second pass = %+v, want it to have moved on to the next row. A "+
			"transaction arcade cannot answer for now blocks the head of every pass, "+
			"which hides the whole backlog behind it", second)
	}
}

// Stale also filters on the same column, so stamping gives each row a re-poll
// interval instead of re-asking arcade about it every single tick.
func TestMarkPolledDefersTheNextPollByTheAgeBound(t *testing.T) {
	s := openTestStore(t)
	if err := s.RecordGeneration(t.Context(), 1, "aa"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTx(t.Context(), CellTx{
		Generation: 1, Cell: 0, TxID: "tx", Status: StatusBroadcast,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkPolled(t.Context(), []string{"tx"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Stale(t.Context(), time.Minute, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a just-polled row was selected again under a one-minute age bound: %+v", got)
	}
}

// Stamping must not resurrect a settled row into the reconciler's work list.
func TestMarkPolledLeavesTerminalRowsAlone(t *testing.T) {
	s := openTestStore(t)
	if err := s.RecordGeneration(t.Context(), 1, "aa"); err != nil {
		t.Fatal(err)
	}
	for _, r := range []CellTx{
		{Generation: 1, Cell: 0, TxID: "done", Status: StatusMined},
		{Generation: 1, Cell: 1, TxID: "dead", Status: StatusFailed, Err: "refused"},
	} {
		if err := s.RecordTx(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.MarkPolled(t.Context(), []string{"done", "dead"}); err != nil {
		t.Fatalf("mark polled: %v", err)
	}

	got, err := s.Stale(t.Context(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("terminal rows re-entered the reconciler's work list: %+v", got)
	}
}

// Polling must not destroy the record of how long a row has been unresolved.
//
// The first version of MarkPolled reused updated_at, which works — the
// reconciler still cycles — but makes every polled row look freshly written. On
// the live deployment that turned a visible backlog into an invisible one: rows
// unresolved for minutes all reported an age under 90 seconds, in exactly the
// query used to look for them. updated_at means "last CHANGED"; when we last
// asked about it is a separate fact and needs a separate column.
func TestMarkPolledPreservesHowLongARowHasBeenUnresolved(t *testing.T) {
	s := openTestStore(t)
	if err := s.RecordGeneration(t.Context(), 1, "aa"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTx(t.Context(), CellTx{
		Generation: 1, Cell: 0, TxID: "tx", Status: StatusBroadcast,
	}); err != nil {
		t.Fatal(err)
	}

	read := func() time.Time {
		t.Helper()
		var at time.Time
		q := s.rebind(`SELECT updated_at FROM cell_txs WHERE txid = ?`)
		if err := s.db.QueryRowContext(t.Context(), q, "tx").Scan(&at); err != nil {
			t.Fatalf("read updated_at: %v", err)
		}
		return at
	}

	before := read()
	if err := s.MarkPolled(t.Context(), []string{"tx"}); err != nil {
		t.Fatal(err)
	}
	if after := read(); !after.Equal(before) {
		t.Errorf("updated_at moved from %v to %v when the row was merely POLLED. Its "+
			"age is the only number that says how long a transaction has gone "+
			"unacknowledged, and polling now hides it", before, after)
	}
}

func TestMarkPolledOfNothingIsNotAnError(t *testing.T) {
	if err := openTestStore(t).MarkPolled(t.Context(), nil); err != nil {
		t.Errorf("empty: %v", err)
	}
}
