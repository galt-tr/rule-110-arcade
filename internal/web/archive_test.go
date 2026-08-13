package web

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeArchive is an Archive with no database behind it.
type fakeArchive struct {
	oldest, newest uint64
	empty          bool
	err            error

	lastFrom  uint64
	lastCount int
	lastCells int
}

func (f *fakeArchive) Window(_ context.Context, from uint64, count, cells int) ([]CompactGeneration, error) {
	f.lastFrom, f.lastCount, f.lastCells = from, count, cells
	if f.err != nil {
		return nil, f.err
	}
	var out []CompactGeneration
	for i := range count {
		n := from + uint64(i)
		if n > f.newest {
			break
		}
		out = append(out, CompactGeneration{
			Number: n,
			RowHex: strings.Repeat("aa", cells/8),
			States: strings.Repeat("m", cells),
		})
	}
	return out, nil
}

func (f *fakeArchive) Extent(context.Context) (uint64, uint64, bool, error) {
	if f.err != nil {
		return 0, 0, false, f.err
	}
	return f.oldest, f.newest, !f.empty, nil
}

func (f *fakeArchive) TxID(_ context.Context, generation uint64, cell int) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	if generation > f.newest || cell < 0 {
		return "", false, nil
	}
	return "abc123", true, nil
}

func archiveServer(t *testing.T, a Archive) *httptest.Server {
	t.Helper()
	var opts []Option
	if a != nil {
		opts = append(opts, WithArchive(a))
	}
	srv := httptest.NewServer(
		New(newFakeAutomaton(), slog.New(slog.NewTextHandler(io.Discard, nil)), opts...).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// TestArchiveWindowCarriesNoTransactionIDs is the guard on the number that
// makes the whole archive viewable.
//
// The live deployment served 25.5 MB per page load, 62% of it transaction ids.
// If one reappears in this payload the page silently goes back to being
// unusable at scale, and nothing else would catch it.
func TestArchiveWindowCarriesNoTransactionIDs(t *testing.T) {
	srv := archiveServer(t, &fakeArchive{newest: 5000})

	code, body := getBody(t, srv.URL+"/api/history?from=100&count=10")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if strings.Contains(body, "txid") {
		t.Error("the archive window carries transaction ids")
	}

	var got archiveResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Generations) != 10 {
		t.Fatalf("got %d generations, want 10", len(got.Generations))
	}
	for i, g := range got.Generations {
		if want := uint64(100 + i); g.Number != want {
			t.Errorf("generation[%d] = %d, want %d ascending from the requested start", i, g.Number, want)
		}
		if len(g.States) != 128 {
			t.Errorf("generation %d carries %d state characters, want one per cell", g.Number, len(g.States))
		}
	}
}

// The window is bounded because this endpoint is public and its cost is a
// database range scan. Without the cap one request could ask for the whole
// archive and undo the point of having an endpoint.
func TestArchiveWindowIsCapped(t *testing.T) {
	f := &fakeArchive{newest: 1_000_000}
	srv := archiveServer(t, f)

	if code, _ := getBody(t, srv.URL+"/api/history?from=0&count=999999"); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if f.lastCount != maxWindow {
		t.Errorf("count reached the store as %d, want it capped at %d", f.lastCount, maxWindow)
	}

	// No count at all is a screenful, not everything.
	_, _ = getBody(t, srv.URL+"/api/history?from=0")
	if f.lastCount != defaultWindow {
		t.Errorf("unparameterised count = %d, want %d", f.lastCount, defaultWindow)
	}
}

// A scrollbar can ask for a window the frontier has not reached. That is empty,
// not an error.
func TestArchiveWindowPastTheEndIsEmpty(t *testing.T) {
	srv := archiveServer(t, &fakeArchive{newest: 10})

	code, body := getBody(t, srv.URL+"/api/history?from=9999&count=10")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	var got archiveResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Generations) != 0 {
		t.Errorf("got %d generations past the end", len(got.Generations))
	}
	if !strings.Contains(body, "[]") {
		t.Error("an empty window encoded as null rather than an empty array")
	}
}

// The extent is what sizes the scrollbar, so an empty archive has to say so
// rather than reporting a range of [0,0] that looks like one generation.
func TestExtentSizesTheScrollbar(t *testing.T) {
	srv := archiveServer(t, &fakeArchive{oldest: 12, newest: 4011})
	_, body := getBody(t, srv.URL+"/api/extent")
	var got extentResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Oldest != 12 || got.Newest != 4011 || got.Count != 4000 {
		t.Errorf("extent = %+v, want oldest 12 newest 4011 count 4000", got)
	}

	empty := archiveServer(t, &fakeArchive{empty: true})
	_, body = getBody(t, empty.URL+"/api/extent")
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Empty {
		t.Error("an empty archive did not say so")
	}
}

func TestTxIDIsServedOnDemand(t *testing.T) {
	srv := archiveServer(t, &fakeArchive{newest: 100})

	code, body := getBody(t, srv.URL+"/api/tx?generation=5&cell=3")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if !strings.Contains(body, "abc123") {
		t.Errorf("body = %s, want the transaction id", body)
	}

	if code, _ := getBody(t, srv.URL+"/api/tx?generation=99999&cell=0"); code != http.StatusNotFound {
		t.Errorf("a cell with no transaction returned %d, want 404", code)
	}
	if code, _ := getBody(t, srv.URL+"/api/tx?cell=0"); code != http.StatusBadRequest {
		t.Errorf("a request with no generation returned %d, want 400", code)
	}
}

// A deployment with no archive configured has no archive routes, rather than
// routes that fail.
func TestArchiveRoutesAbsentWithoutAnArchive(t *testing.T) {
	srv := archiveServer(t, nil)
	for _, p := range []string{"/api/history", "/api/extent", "/api/tx?generation=1&cell=1"} {
		if code, _ := getBody(t, srv.URL+p); code != http.StatusNotFound {
			t.Errorf("%s = %d with no archive, want 404", p, code)
		}
	}
}

// A store failure must not leak its message to a stranger.
func TestArchiveErrorsStayOpaque(t *testing.T) {
	srv := archiveServer(t, &fakeArchive{err: errors.New("pq: relation does not exist")})
	code, body := getBody(t, srv.URL+"/api/history?from=0")
	if code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", code)
	}
	if strings.Contains(body, "pq:") {
		t.Error("a database error was handed to the caller")
	}
}

// TestStateNoLongerShipsTheWholeRing is the 25.5 MB regression guard.
//
// /api/state returned every generation the engine held. Deep history now comes
// from the archive, so this must carry only enough to draw the live edge.
func TestStateNoLongerShipsTheWholeRing(t *testing.T) {
	f := newFakeAutomaton()
	srv := archiveServer(t, &fakeArchive{newest: 10})

	_, body := getBody(t, srv.URL+"/api/state")
	var snap struct {
		History []struct{} `json:"history"`
	}
	if err := json.Unmarshal([]byte(body), &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(snap.History) > 64 {
		t.Errorf("/api/state carried %d generations; it is meant to be the live edge, "+
			"not the archive — this is the 25.5 MB page load", len(snap.History))
	}
	if snapshots, _, _ := f.counts(); snapshots != 0 {
		t.Error("/api/state built a full snapshot")
	}
}

// The archive compresses roughly an order of magnitude because a settled
// generation's state string is one character repeated. That ratio is the
// difference between a screenful costing 88 kB and 10 kB.
func TestArchiveIsCompressed(t *testing.T) {
	srv := archiveServer(t, &fakeArchive{newest: 5000})

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/history?from=0&count=256", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", res.Header.Get("Content-Encoding"))
	}
	packed, _ := io.ReadAll(res.Body)
	zr, err := gzip.NewReader(strings.NewReader(string(packed)))
	if err != nil {
		t.Fatalf("body is not gzip: %v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("inflate: %v", err)
	}
	if ratio := float64(len(plain)) / float64(len(packed)); ratio < 5 {
		t.Errorf("compressed %d -> %d, ratio %.1fx; expected roughly an order of magnitude "+
			"from the repeated state characters", len(plain), len(packed), ratio)
	}
}
