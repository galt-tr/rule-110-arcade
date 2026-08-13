package web

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
	published *[]byte
	changed   chan struct{}

	// locked is what Stats reports, and therefore what handleControl refuses on.
	locked bool

	// driven records every mutator the HTTP layer actually reached.
	driven []string

	// generation is what Stats reports as the frontier.
	generation uint64

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
	// A FRESH slice, and therefore a fresh pointer, on every publish — the
	// engine's PublishTail does the same, and subscribers use that pointer to
	// tell a new frame from the one they already sent.
	b := []byte(payload)
	f.published = &b
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

// notifyOnly wakes subscribers WITHOUT publishing anything new, which is what
// the engine's notify() does on every recorded cell and status batch between
// the publisher's throttled rebuilds.
func (f *fakeAutomaton) notifyOnly() {
	f.mu.Lock()
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

func (f *fakeAutomaton) PublishedTail() (*[]byte, bool) {
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
	return engine.Snapshot{Cells: 128, Locked: f.locked, Generation: f.generation}
}

func (f *fakeAutomaton) counts() (snapshot, tail, stats int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshotCalls, f.tailCalls, f.statsCalls
}

// The mutators record that they were reached at all. A locked deployment must
// refuse BEFORE the engine is touched, so "was it called" is the assertion, not
// "what did it do".
func (f *fakeAutomaton) SetMode(engine.Mode) { f.drove("SetMode") }
func (f *fakeAutomaton) SetRate(float64)     { f.drove("SetRate") }
func (f *fakeAutomaton) Step()               { f.drove("Step") }

func (f *fakeAutomaton) drove(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.driven = append(f.driven, name)
}

func (f *fakeAutomaton) drivenCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.driven...)
}

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

// TestLockedControlsAreRefused is the guard for the one endpoint a public
// deployment cannot afford to leave open.
//
// POST /api/control has no authentication of any kind, so on a public URL it is
// one request from anyone: pause the automaton, single-step it, or set the rate
// to something the chain cannot serve. Hiding the buttons in the browser is
// presentation. The refusal has to be here, it has to cover every action rather
// than the obvious ones, and it has to happen before the engine is reached.
func TestLockedControlsAreRefused(t *testing.T) {
	for _, body := range []string{
		`{"action":"play"}`,
		`{"action":"pause"}`,
		`{"action":"step"}`,
		`{"action":"rate","rate":20}`,
	} {
		f := newFakeAutomaton()
		f.locked = true
		srv := testServer(f)

		res, err := http.Post(srv.URL+"/api/control", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		srv.Close()

		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s returned %d, want %d", body, res.StatusCode, http.StatusForbidden)
		}
		if driven := f.drivenCalls(); len(driven) != 0 {
			t.Errorf("%s reached the engine (%v); a locked deployment must refuse before it", body, driven)
		}
	}
}

// Unlocked, the same requests must still drive the engine — the lock is a
// deployment choice, not a removal of the feature.
func TestUnlockedControlsStillReachTheEngine(t *testing.T) {
	f := newFakeAutomaton()
	srv := testServer(f)
	defer srv.Close()

	res, err := http.Post(srv.URL+"/api/control", "application/json", strings.NewReader(`{"action":"play"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("control returned %d, want 200", res.StatusCode)
	}
	if driven := f.drivenCalls(); len(driven) != 1 || driven[0] != "SetMode" {
		t.Errorf("play drove %v, want one SetMode", driven)
	}
}

// TestEventsDoesNotResendAnUnchangedTail is the guard for what a public viewer
// actually costs.
//
// The engine coalesces the CONTENT of a publish but not the notifications:
// notify() fires on every recorded cell, every status batch and every clock
// tick, while PublishTails rebuilds at most once per PublishInterval. The
// subscriber loop wakes on each notification, so without a check it wrote a
// byte-identical frame every time — hundreds of kilobytes each, at 256 cells,
// many times a second, per connected browser.
func TestEventsDoesNotResendAnUnchangedTail(t *testing.T) {
	f := newFakeAutomaton()
	// Publish BEFORE connecting. handleEvents writes no response headers until
	// its first flush, so against an engine that has published nothing the
	// client's Do() blocks until the 15s keepalive — which is a slow test, not a
	// stranded frame, but it hides everything after it.
	f.publish(`{"generation":1}`)

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
	if got := nextFrame(t, frames, 5*time.Second, "no initial frame"); got != `{"generation":1}` {
		t.Fatalf("first frame = %q", got)
	}

	// Wake the subscriber repeatedly with no new publish in between, SPACED so
	// the handler completes a loop for each one. Firing them back to back is not
	// the same test: a closed channel wakes the select once, so a tight burst
	// collapses into a single pass and would not resend anything even without
	// the check below. A settling generation notifies steadily, not instantly.
	for range 10 {
		f.notifyOnly()
		time.Sleep(2 * time.Millisecond)
	}

	// Nothing new was published, so nothing may have been sent.
	select {
	case dup := <-frames:
		t.Fatalf("the stream re-sent %q after notifications that published nothing; "+
			"every connected browser pays for this frame again on each notify()", dup)
	case <-time.After(50 * time.Millisecond):
	}

	// And a real publish still gets through — the check must suppress
	// duplicates, not updates.
	f.publish(`{"generation":2}`)
	if got := nextFrame(t, frames, 5*time.Second, "the next publish was never sent"); got != `{"generation":2}` {
		t.Errorf("frame after the second publish = %q, want it", got)
	}
}

// A viewer must never be left running client code from an earlier deploy.
//
// Embedded files carry a zero modification time, so net/http omits
// Last-Modified, and http.FileServer sets no ETag. With neither validator nor
// Cache-Control a browser decides for itself when to ask again, and can serve a
// previous build's app.js indefinitely with nothing to indicate it — which
// turns "is this deployed yet?" into guesswork.
func TestStaticAssetsCarryAValidator(t *testing.T) {
	h := staticHandler()

	get := func(path string, ifNoneMatch string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	first := get("/app.js", "")
	if first.Code != http.StatusOK {
		t.Fatalf("GET /app.js = %d, want 200", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on app.js: a browser has nothing to revalidate against")
	}
	if got := first.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache (revalidate before reuse)", got)
	}

	// The validator's payoff: a repeat visit costs a round trip, not the asset.
	again := get("/app.js", etag)
	if again.Code != http.StatusNotModified {
		t.Fatalf("conditional GET = %d, want 304", again.Code)
	}
	if again.Body.Len() != 0 {
		t.Fatalf("304 carried %d bytes of body", again.Body.Len())
	}

	// And the point of the whole change: a tag from a previous build is refused,
	// so a deploy is picked up on the next load rather than whenever the browser
	// happens to lose interest.
	stale := get("/app.js", `"0000000000000000000000000000000000000000"`)
	if stale.Code != http.StatusOK {
		t.Fatalf("GET with a stale ETag = %d, want 200 with the new asset", stale.Code)
	}
	if stale.Body.Len() == 0 {
		t.Fatal("a stale ETag got an empty body; the client would keep the old build")
	}

	// Distinct contents must not share a tag, or one asset's cache entry
	// validates another's bytes.
	if css := get("/style.css", "").Header().Get("ETag"); css == etag {
		t.Fatalf("style.css and app.js share the ETag %s", css)
	}

	// "/" is index.html, and needs the validator just as much.
	if root := get("/", "").Header().Get("ETag"); root == "" {
		t.Fatal("the page itself carries no ETag")
	}
}

// What a viewer actually gets, as opposed to what the renderer tests assume.
//
// Those drive a hand-written DOM whose checkbox stub defaults to checked, so
// they cannot see the shipped default at all; only the file can say.
func TestFollowShipsOffWithAWayBackToTheEnd(t *testing.T) {
	b, err := fs.ReadFile(staticRoot, "index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	page := string(b)

	tag := regexp.MustCompile(`<input[^>]*id="follow"[^>]*>`).FindString(page)
	if tag == "" {
		t.Fatal(`no <input id="follow"> in the page`)
	}
	if strings.Contains(tag, "checked") {
		t.Errorf("follow ships checked (%s); the view would be dragged to the "+
			"newest row while someone is reading history", tag)
	}

	// Following is off, so there has to be a deliberate way back to the end.
	if !strings.Contains(page, `id="toBottom"`) {
		t.Error("no go-to-bottom control: with follow off, someone who scrolls " +
			"into history has no way back to the live edge but dragging")
	}
}

// The page is the one response no cache is allowed to keep, and the only
// pointer a browser follows to the rest of the build. So everything it names
// has to carry a version, or that asset is the one a stale cache answers with
// the previous build's bytes — which is exactly how a four-hour-old app.js went
// on throwing a ReferenceError at every click while the origin served the fix.
//
// This walks the page rather than a list of filenames on purpose. A list is a
// thing to forget to update, and explain.js — added a release after the caching
// headers were — is the proof that it does get forgotten.
func TestEveryAssetThePageLoadsCarriesItsVersion(t *testing.T) {
	rec := httptest.NewRecorder()
	staticHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}

	refs := regexp.MustCompile(`(?:src|href)="([^"]+)"`).FindAllStringSubmatch(rec.Body.String(), -1)
	if len(refs) == 0 {
		t.Fatal("the page references no assets at all; the page or this regexp changed shape")
	}

	checked := 0
	for _, m := range refs {
		u, err := url.Parse(m[1])
		if err != nil {
			continue
		}
		p := u.Path
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		tag, ok := staticETags[p]
		if !ok || p == "/index.html" {
			continue // an external URL, or the page itself
		}
		checked++
		want := strings.Trim(tag, `"`)
		if got := u.Query().Get("v"); got != want {
			t.Errorf("%s carries v=%q, want %q (its content hash): a cache keyed on this URL "+
				"has no reason to ever ask for the new build", m[1], got, want)
		}
	}
	if checked == 0 {
		t.Fatal("no embedded asset was checked; the page and the embed have drifted apart")
	}
}

// The version is a cache key, not a lookup key. Whatever it says — this build's
// hash, a previous one, nonsense — the current bytes come back. Anything else
// would turn a stale bookmark into a 404 on the page's own scripts.
func TestAVersionedURLStillServesTheCurrentAsset(t *testing.T) {
	plain := httptest.NewRecorder()
	staticHandler().ServeHTTP(plain, httptest.NewRequest(http.MethodGet, "/app.js", nil))

	for _, q := range []string{"?v=" + strings.Trim(staticETags["/app.js"], `"`), "?v=0000", "?v="} {
		rec := httptest.NewRecorder()
		staticHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.js"+q, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /app.js%s = %d, want 200", q, rec.Code)
		}
		if rec.Body.String() != plain.Body.String() {
			t.Fatalf("GET /app.js%s served different bytes than /app.js", q)
		}
	}
}

// The page is rewritten on the way out, so its validator has to be a hash of
// what was actually sent. Tagging it with the hash of the embedded file instead
// would be a validator that lies: change app.js, and the browser revalidates
// the page, is told 304, and keeps a copy pointing at the old script.
func TestThePageIsTaggedForTheBytesItSends(t *testing.T) {
	rec := httptest.NewRecorder()
	staticHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	sum := sha256.Sum256(rec.Body.Bytes())
	want := `"` + hex.EncodeToString(sum[:16]) + `"`
	if got := rec.Header().Get("ETag"); got != want {
		t.Fatalf("ETag on / = %s, want %s (the hash of the body actually sent)", got, want)
	}
}
