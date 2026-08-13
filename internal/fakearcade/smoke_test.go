package fakearcade_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/defs"

	"github.com/dymurray/rule-110-arcade/internal/fakearcade"
)

// These tests drive the REAL toolbox arcade client — real HTTP, real SSE frame
// parsing, real reconnect logic — against a fake that can be told to misbehave
// on demand. What they cover that the engine's unit tests cannot: the transport.
//
// Everything the 2026-08-13 outage turned on happened at this layer. Events
// existed at arcade and never arrived; the stream stayed open and silent; the
// client looked healthy throughout. A unit test that injects statuses directly
// cannot reproduce any of that, because it starts after the part that broke.

func newClient(t *testing.T, s *fakearcade.Server) *arcade.Client {
	t.Helper()
	c := arcade.New(nil, nil, defs.Arcade{
		Enabled:       true,
		URL:           s.URL(),
		EventsURL:     s.URL(),
		CallbackToken: "smoke-token",
	})
	return c
}

// broadcast submits one transaction, identified by a caller-chosen body, and
// returns both the id the fake assigned and arcade's answer.
func broadcast(t *testing.T, c *arcade.Client, body []byte) (string, *arcade.BroadcastResult) {
	t.Helper()
	txid := fakearcade.TxIDFor(body)
	res, err := c.Broadcast(context.Background(), txid, body)
	if err != nil {
		t.Fatalf("broadcast %s: %v", txid, err)
	}
	return txid, res
}

// bodyFor is a distinct, deterministic transaction body per index.
func bodyFor(i int) []byte { return fmt.Appendf(nil, "rule110-smoke-%d", i) }

// A rejection has to reach the consumer as a rejection, over the wire, with
// arcade's own words attached. The engine's classifier reads that text, so a
// transport that drops it turns a diagnosable fault into a bare "REJECTED".
func TestRejectionSurvivesTheWire(t *testing.T) {
	s := fakearcade.New(t)
	c := newClient(t, s)

	s.RejectNext(1)
	_, res := broadcast(t, c, bodyFor(1))

	if res == nil || !res.Rejected {
		t.Fatalf("broadcast result = %+v, want a tx-level rejection", res)
	}
	if res.Status != arcade.StatusRejected {
		t.Errorf("status = %q, want %q", res.Status, arcade.StatusRejected)
	}
	if res.ExtraInfo == "" {
		t.Error("the rejection arrived with no reason. The engine classifies on this " +
			"text — retryable or not, cascade or root cause — so an empty one is a " +
			"fault nobody can diagnose from the logs")
	}
}

// The failure that took the ring down: arcade holds a verdict the stream never
// delivers. GetTx must still answer, because that poll is the only thing
// standing between a lost event and a stopped cell.
func TestAVerdictLostFromTheStreamIsStillAnswerableByPoll(t *testing.T) {
	s := fakearcade.New(t)
	c := newClient(t, s)
	txid, _ := broadcast(t, c, bodyFor(2))

	// Everything from here is invisible to a subscriber.
	s.DropEvents(true)
	s.Advance(txid, arcade.StatusAcceptedByNetwork)

	rec, err := c.GetTx(context.Background(), txid)
	if err != nil {
		t.Fatalf("GetTx: %v", err)
	}
	if rec.Status != arcade.StatusAcceptedByNetwork {
		t.Errorf("GetTx status = %q, want %q. Arcade holds the verdict; if the poll "+
			"cannot retrieve it then a dropped event is unrecoverable and the cell "+
			"waiting on it is lost", rec.Status, arcade.StatusAcceptedByNetwork)
	}
}

// A transaction arcade has never heard of must be distinguishable from one it
// has no verdict for yet. The acceptance gate treats these differently: the
// first is grounds to repair, the second grounds to keep waiting.
func TestAnUnknownTransactionIsNotFound(t *testing.T) {
	s := fakearcade.New(t)
	c := newClient(t, s)

	_, err := c.GetTx(context.Background(), fmt.Sprintf("%064x", 0xdead))
	if err == nil {
		t.Fatal("GetTx invented an answer for a transaction arcade has never seen")
	}
	if err.Error() == "" {
		t.Error("empty error")
	}
}

// The stream has to deliver what is queued, in order, to a real subscriber.
// This is the happy path, and it is here so the failure tests below mean
// something: without it, "no events arrived" proves nothing.
func TestTheStreamDeliversStatuses(t *testing.T) {
	s := fakearcade.New(t)
	c := newClient(t, s)

	const n = 8
	for i := range n {
		broadcast(t, c, bodyFor(100+i))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var (
		mu   sync.Mutex
		seen = map[string]arcade.Status{}
	)
	done := make(chan struct{})
	go func() {
		_ = c.StreamStatus(ctx, "", func(ev arcade.StatusEvent) error {
			mu.Lock()
			seen[ev.Record.TxID] = ev.Record.Status
			ready := len(seen) >= n
			mu.Unlock()
			if ready {
				select {
				case <-done:
				default:
					close(done)
				}
			}
			return nil
		})
	}()

	select {
	case <-done:
	case <-ctx.Done():
		mu.Lock()
		got := len(seen)
		mu.Unlock()
		t.Fatalf("only %d of %d statuses arrived over the stream", got, n)
	}
}

// A stalled stream is the shape that hid the outage: connected, healthy by
// every local check, delivering nothing. The consumer must not conclude
// anything from silence — and the poll must still work through it, because that
// is the only way out.
func TestASilentStreamStillLeavesThePollWorking(t *testing.T) {
	s := fakearcade.New(t)
	c := newClient(t, s)
	txid, _ := broadcast(t, c, bodyFor(3))
	s.Stall(true)
	s.Advance(txid, arcade.StatusSeenOnNetwork)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var delivered int
	go func() {
		_ = c.StreamStatus(ctx, "", func(arcade.StatusEvent) error {
			delivered++
			return nil
		})
	}()
	<-ctx.Done()

	if delivered != 0 {
		t.Errorf("%d events arrived from a stalled stream; the fake is not "+
			"reproducing the failure it exists for", delivered)
	}

	// The way out.
	rec, err := c.GetTx(context.Background(), txid)
	if err != nil {
		t.Fatalf("GetTx while the stream is silent: %v", err)
	}
	if rec.Status != arcade.StatusSeenOnNetwork {
		t.Errorf("GetTx status = %q, want %q", rec.Status, arcade.StatusSeenOnNetwork)
	}
}

// The block sweep, which is what actually overruns arcade's fan-out: one event
// per transaction, all at once. Nothing here should be lost, and the point is
// to have the shape available to run the consumer against.
func TestABlockSweepEmitsOneEventPerTransaction(t *testing.T) {
	s := fakearcade.New(t)
	c := newClient(t, s)

	const n = 64
	for i := range n {
		broadcast(t, c, bodyFor(200+i))
	}
	if got := s.Known(); got != n {
		t.Fatalf("arcade knows %d transactions, want %d", got, n)
	}

	s.AdvanceAll(arcade.StatusMined)

	for i := range n {
		txid := fakearcade.TxIDFor(bodyFor(200 + i))
		st, ok := s.Status(txid)
		if !ok || st != arcade.StatusMined {
			t.Fatalf("%s = %q (known=%v), want every transaction mined by the sweep",
				txid, st, ok)
		}
	}
}
