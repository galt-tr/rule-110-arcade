package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dymurray/rule-110-arcade/internal/engine"
)

// fakeAutomaton is an Automaton whose published value and change notifications
// the test drives directly.
type fakeAutomaton struct {
	mu        sync.Mutex
	published []byte
	changed   chan struct{}

	snapshotCalls, tailCalls, statsCalls int

	// calls records the order of Changed/PublishedTail, which is the whole
	// subject of TestEventsSubscribesBeforeItReads.
	calls []string
}

func newFakeAutomaton() *fakeAutomaton {
	return &fakeAutomaton{changed: make(chan struct{})}
}

// publish sets the value and wakes subscribers, exactly as the engine's
// publisher and notify() do together.
func (f *fakeAutomaton) publish(payload string) {
	f.mu.Lock()
	f.published = []byte(payload)
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

func (f *fakeAutomaton) PublishedTail() ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "read")
	if f.published == nil {
		return nil, false
	}
	return f.published, true
}

func (f *fakeAutomaton) Changed() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "subscribe")
	return f.changed
}

func (f *fakeAutomaton) callOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeAutomaton) Snapshot() engine.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshotCalls++
	return engine.Snapshot{Cells: 128, History: make([]engine.Generation, 2048)}
}

func (f *fakeAutomaton) SnapshotTail() engine.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tailCalls++
	return engine.Snapshot{Cells: 128, History: make([]engine.Generation, engine.TailGenerations)}
}

func (f *fakeAutomaton) Stats() engine.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statsCalls++
	return engine.Snapshot{Cells: 128}
}

func (f *fakeAutomaton) counts() (snapshot, tail, stats int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshotCalls, f.tailCalls, f.statsCalls
}

func (f *fakeAutomaton) SetMode(engine.Mode) {}
func (f *fakeAutomaton) SetRate(float64)     {}
func (f *fakeAutomaton) Step()               {}

func testServer(f *fakeAutomaton) *httptest.Server {
	return httptest.NewServer(New(f, slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
}

// readFrames reads `data:` payloads off an SSE stream into a channel.
func readFrames(t *testing.T, body io.Reader) <-chan string {
	t.Helper()
	out := make(chan string, 64)
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(body)
		for scanner.Scan() {
			line := scanner.Text()
			if payload, ok := strings.CutPrefix(line, "data: "); ok {
				out <- payload
			}
		}
	}()
	return out
}

func nextFrame(t *testing.T, frames <-chan string, timeout time.Duration, msg string) string {
	t.Helper()
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatalf("%s: stream closed", msg)
		}
		return f
	case <-time.After(timeout):
		t.Fatal(msg)
		return ""
	}
}

// TestEventsSubscribesBeforeItReads is the regression guard for the
// missed-update race, asserted as the ordering contract rather than by trying to
// hit the window.
//
// The handler used to acquire the change channel AFTER sending. notify() closes
// the current channel and installs a fresh one, so a change landing between the
// read and the next subscription closed a channel nobody held — and then waited
// for some LATER change to notice it. Once the automaton goes quiet that later
// change never comes, and the browser sits on a stale final frame; the 15s
// keepalive carries no data.
//
// Racing the window directly is not a test, it is a coin flip: it is a few
// instructions wide and passes against the broken code most of the time. What
// makes the code correct is the order of the two calls, so that is what is
// pinned here. Against the previous handler the first recorded call is "read".
func TestEventsSubscribesBeforeItReads(t *testing.T) {
	f := newFakeAutomaton()
	f.publish(`{"n":0}`)
	srv := testServer(f)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	frames := readFrames(t, res.Body)
	nextFrame(t, frames, 5*time.Second, "no initial frame")

	f.publish(`{"n":1}`)
	nextFrame(t, frames, 5*time.Second, "the change was never sent")

	// Every read must be covered by a subscription taken before it, so there is
	// no window in which a change can close a channel nobody holds.
	order := f.callOrder()
	if len(order) < 4 {
		t.Fatalf("recorded only %d calls: %v", len(order), order)
	}
	if order[0] != "subscribe" {
		t.Errorf("first call was %q, want %q — the handler read the state before "+
			"subscribing, so a change landing in between is lost", order[0], "subscribe")
	}
	subscribed := 0
	for i, call := range order {
		switch call {
		case "subscribe":
			subscribed++
		case "read":
			if subscribed == 0 {
				t.Fatalf("call %d read the state with no outstanding subscription: %v", i, order)
			}
			subscribed--
		}
	}
}

// TestEventsConvergesOnTheFinalState is the behavioural companion: after a burst
// followed by silence, the stream must settle on the last published value
// without another change to prompt it.
func TestEventsConvergesOnTheFinalState(t *testing.T) {
	f := newFakeAutomaton()
	f.publish(`{"n":0}`)
	srv := testServer(f)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	frames := readFrames(t, res.Body)
	if got := nextFrame(t, frames, 5*time.Second, "no initial frame"); got != `{"n":0}` {
		t.Fatalf("initial frame = %s", got)
	}

	// A burst, then silence. The LAST of these is the one the old code could
	// strand: it may land while the handler is still writing an earlier frame.
	for i := 1; i <= 20; i++ {
		f.publish(`{"n":` + string(rune('0'+i%10)) + `,"i":` + itoa(i) + `}`)
		time.Sleep(time.Millisecond)
	}

	// Whatever the coalescing, the stream must converge on the final published
	// value without another change to prompt it.
	want := `"i":20`
	deadline := time.After(5 * time.Second)
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("stream closed before the final state arrived")
			}
			if strings.Contains(frame, want) {
				return
			}
		case <-deadline:
			t.Fatal("the final published state was never delivered; a change that landed " +
				"during a send was stranded")
		}
	}
}

// TestEventsSendsOnTheLeadingEdge pins the other half: an isolated change is not
// taxed a fixed coalescing interval before it is sent.
func TestEventsSendsOnTheLeadingEdge(t *testing.T) {
	f := newFakeAutomaton()
	f.publish(`{"gen":0}`)
	srv := testServer(f)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	frames := readFrames(t, res.Body)
	nextFrame(t, frames, 5*time.Second, "no initial frame")

	start := time.Now()
	f.publish(`{"gen":1}`)
	got := nextFrame(t, frames, 5*time.Second, "an isolated change was never sent")

	if !strings.Contains(got, `"gen":1`) {
		t.Fatalf("frame = %s, want the new state", got)
	}
	// The old handler slept 100ms before every send. 50ms is comfortably under
	// that and comfortably over what a local round trip needs.
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("an isolated change took %v to reach the client; the coalescing "+
			"interval is being applied as a delay", elapsed)
	}
}

// TestEventsWritesThePublishedBytes proves the handler does no snapshot work of
// its own: N subscribers cost one marshal, done by the publisher.
func TestEventsWritesThePublishedBytes(t *testing.T) {
	f := newFakeAutomaton()
	f.publish(`{"shared":true}`)
	srv := testServer(f)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for range 3 {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		frames := readFrames(t, res.Body)
		if got := nextFrame(t, frames, 5*time.Second, "no frame"); got != `{"shared":true}` {
			t.Errorf("frame = %s, want the published bytes verbatim", got)
		}
		_ = res.Body.Close()
	}

	if _, tail, _ := f.counts(); tail != 0 {
		t.Errorf("the events handler built %d tail snapshots; it should write the "+
			"published bytes and build none", tail)
	}
}

// TestMetricsAndReadyzDoNotCopyHistory covers the scrape path. Both are polled
// on a timer whether or not a browser is connected.
func TestMetricsAndReadyzDoNotCopyHistory(t *testing.T) {
	f := newFakeAutomaton()
	srv := testServer(f)
	defer srv.Close()

	for _, path := range []string{"/metrics", "/readyz"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: status %d", path, res.StatusCode)
		}
	}

	snapshot, _, stats := f.counts()
	if snapshot != 0 {
		t.Errorf("the scrape path built %d full snapshots, want 0", snapshot)
	}
	if stats != 2 {
		t.Errorf("Stats called %d times, want 2 (one per endpoint)", stats)
	}
}

// TestControlReturnsTheTail — every button click used to return the whole
// history. The client merges by generation number, so a tail says everything a
// control response usefully can.
func TestControlReturnsTheTail(t *testing.T) {
	f := newFakeAutomaton()
	srv := testServer(f)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/api/control", "application/json", strings.NewReader(`{"action":"pause"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	var snap engine.Snapshot
	if err := json.NewDecoder(res.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.History) != engine.TailGenerations {
		t.Errorf("control returned %d generations, want the tail (%d)",
			len(snap.History), engine.TailGenerations)
	}
	if snapshot, _, _ := f.counts(); snapshot != 0 {
		t.Errorf("control built %d full snapshots, want 0", snapshot)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
