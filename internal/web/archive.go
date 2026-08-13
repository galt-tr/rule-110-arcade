package web

import (
	"compress/gzip"
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Archive is the durable history, as the HTTP layer needs it.
//
// Narrow, and an interface for the same reason Automaton and Funder are: these
// handlers are the difference between a page that loads in kilobytes and one
// that loads in megabytes, and that is worth being able to test without a
// database.
//
// It is deliberately separate from Automaton. The engine holds a small live
// ring; this reaches the whole archive, which is the only place a scrollbar
// over 100,000 generations can be served from.
type Archive interface {
	// Window returns generations in [from, from+count) without transaction ids.
	Window(ctx context.Context, from uint64, count int, cells int) ([]CompactGeneration, error)
	// Extent is the oldest and newest generation recorded.
	Extent(ctx context.Context) (oldest, newest uint64, ok bool, err error)
	// TxID is one cell's transaction, for a click.
	TxID(ctx context.Context, generation uint64, cell int) (string, bool, error)
}

// CompactGeneration mirrors history.CompactGeneration, declared here so a test
// fake needs no history import — the same instinct as the other seams.
type CompactGeneration struct {
	Number uint64 `json:"n"`
	RowHex string `json:"row"`
	States string `json:"s"`
}

const (
	// maxWindow bounds one archive request.
	//
	// A screenful at the default zoom is ~250 generations, so this is several
	// screens of overscan and no more. The cap matters because the handler is
	// public and the cost is a database range scan: without it one request could
	// ask for the entire archive and undo the point of the endpoint.
	maxWindow = 1024

	// defaultWindow is what an unparameterised request gets.
	defaultWindow = 256

	// settledMargin is how far behind the frontier a generation must be before
	// its window is treated as immutable and allowed to be cached forever.
	//
	// A cell's status still moves for a long time after it is broadcast: seen
	// arrives in seconds, but MINED lands roughly rate x block-interval
	// generations behind, which on this network measured about 378. 2,000 is
	// comfortably past that, so a window below it will not change again.
	//
	// This is what makes many viewers free rather than merely survivable. There
	// is a CDN in front of this deployment, so an immutable window is served
	// from the edge to everyone after the first request and never reaches
	// PostgreSQL at all.
	settledMargin = 2000
)

// archiveResponse is the wire shape. `from` echoes the resolved start so a
// client that asked for something out of range knows what it actually got.
type archiveResponse struct {
	From        uint64              `json:"from"`
	Generations []CompactGeneration `json:"generations"`
}

type extentResponse struct {
	Oldest uint64 `json:"oldest"`
	Newest uint64 `json:"newest"`
	Count  uint64 `json:"count"`
	Empty  bool   `json:"empty"`
}

type txResponse struct {
	Generation uint64 `json:"generation"`
	Cell       int    `json:"cell"`
	TxID       string `json:"txid"`
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if s.archive == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	from, _ := strconv.ParseUint(q.Get("from"), 10, 64)

	count := defaultWindow
	if n, err := strconv.Atoi(q.Get("count")); err == nil && n > 0 {
		count = min(n, maxWindow)
	}

	// The ring size comes from the engine rather than the request: it is a
	// property of the deployment, and a client-supplied width would let a caller
	// ask for state strings of any length.
	cells := s.engine.Stats().Cells

	gens, err := s.archive.Window(r.Context(), from, count, cells)
	if err != nil {
		s.logger.Error("read history window", "from", from, "count", count, "err", err)
		http.Error(w, "could not read history", http.StatusInternalServerError)
		return
	}
	if gens == nil {
		gens = []CompactGeneration{}
	}

	// Settled history is immutable, and saying so is the difference between a
	// popular page costing one database read per viewer and one read in total.
	// The frontier is the only thing that makes a window mutable.
	frontier := s.engine.Stats().Generation
	if last := from + uint64(count) - 1; frontier > settledMargin && last < frontier-settledMargin {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// Near the frontier statuses are still moving; a stale window here would
		// show a settled row as pending for a year.
		w.Header().Set("Cache-Control", "no-cache")
	}
	s.writeJSONGzip(w, r, archiveResponse{From: from, Generations: gens})
}

func (s *Server) handleExtent(w http.ResponseWriter, r *http.Request) {
	if s.archive == nil {
		http.NotFound(w, r)
		return
	}
	oldest, newest, ok, err := s.archive.Extent(r.Context())
	if err != nil {
		s.logger.Error("read history extent", "err", err)
		http.Error(w, "could not read history extent", http.StatusInternalServerError)
		return
	}
	if !ok {
		writeJSON(w, extentResponse{Empty: true})
		return
	}
	writeJSON(w, extentResponse{Oldest: oldest, Newest: newest, Count: newest - oldest + 1})
}

func (s *Server) handleTx(w http.ResponseWriter, r *http.Request) {
	if s.archive == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	generation, err := strconv.ParseUint(q.Get("generation"), 10, 64)
	if err != nil {
		http.Error(w, "generation is required", http.StatusBadRequest)
		return
	}
	cell, err := strconv.Atoi(q.Get("cell"))
	if err != nil || cell < 0 {
		http.Error(w, "cell is required", http.StatusBadRequest)
		return
	}

	txid, ok, err := s.archive.TxID(r.Context(), generation, cell)
	if err != nil {
		s.logger.Error("read transaction id", "generation", generation, "cell", cell, "err", err)
		http.Error(w, "could not read the transaction", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, txResponse{Generation: generation, Cell: cell, TxID: txid})
}

// gzipPool reuses writers: the archive endpoint is called on every scroll, and
// a fresh flate writer per request allocates ~800 kB of window.
var gzipPool = sync.Pool{New: func() any { return gzip.NewWriter(nil) }}

// writeJSONGzip compresses when the client will take it.
//
// Worth it here specifically: a settled generation's state string is the same
// character 256 times over, so the archive payload compresses by roughly an
// order of magnitude. This is the one response where the ratio is that good.
func (s *Server) writeJSONGzip(w http.ResponseWriter, r *http.Request, v any) {
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		writeJSON(w, v)
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("Content-Type", "application/json")

	zw, _ := gzipPool.Get().(*gzip.Writer)
	zw.Reset(w)
	defer func() {
		_ = zw.Close()
		gzipPool.Put(zw)
	}()
	encodeJSON(zw, v)
}
