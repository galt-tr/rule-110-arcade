// Package fakearcade is a scriptable stand-in for an arcade deployment,
// speaking arcade's HTTP and SSE wire protocol over httptest so the real
// toolbox client can be driven with no network.
//
// It exists because the failures worth testing are the ones a healthy arcade
// never produces: a rejection storm, a stream that goes quiet, events that are
// dropped and never replayed. Those are precisely what took 248 of 256 cells
// down on 2026-08-13, and none of them can be provoked against a live instance
// on demand.
//
// The toolbox maintains an equivalent (internal/testenv/mockarcade) which is
// under internal/ and therefore unreachable from here. This follows the same
// wire shapes; if that package is ever promoted, delete this one.
package fakearcade

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"
)

// Server is a fake arcade. The zero value is not usable; call New.
type Server struct {
	http *httptest.Server

	mu sync.Mutex
	// records is arcade's view of every transaction it has been given.
	records map[string]arcade.TxRecord
	// pending is the queue of events not yet written to a connected stream.
	pending []arcade.StatusEvent
	// seq numbers events; arcade uses nanosecond timestamps, and monotonicity
	// is the only property the client relies on.
	seq int64

	// rejectNext, when positive, makes that many subsequent broadcasts return a
	// tx-level rejection.
	rejectNext int
	// dropEvents discards emitted events instead of queueing them, modelling
	// arcade's fan-out dropping frames it cannot deliver — the failure whose
	// catch-up truncates and loses them for good.
	dropEvents bool
	// stall holds the stream open but silent, modelling a connection that is
	// established and delivering nothing.
	stall bool

	// height is the chain tip the fake chaintracks reports.
	height uint64
}

// New starts a fake arcade and registers cleanup.
func New(t interface {
	Cleanup(func())
	Helper()
}) *Server {
	t.Helper()
	s := &Server{records: map[string]arcade.TxRecord{}, height: 800_000}
	mux := http.NewServeMux()

	// --- arcade ---
	mux.HandleFunc("POST /tx", s.handleBroadcast)
	mux.HandleFunc("GET /tx/{txid}", s.handleGetTx)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /events", s.handleEvents)

	// --- chaintracks ---
	mux.HandleFunc("GET /chaintracks/v2/height", s.handleHeight)
	mux.HandleFunc("GET /chaintracks/v2/tip", s.handleTip)
	mux.HandleFunc("GET /chaintracks/v2/header/height/{n}", s.handleHeader)
	mux.HandleFunc("GET /chaintracks/v2/tip/stream", s.handleQuietStream)
	mux.HandleFunc("GET /chaintracks/v2/reorg/stream", s.handleQuietStream)

	s.http = httptest.NewServer(mux)
	t.Cleanup(s.http.Close)
	return s
}

// URL is the base URL to point a toolbox client at.
func (s *Server) URL() string { return s.http.URL }

// TxIDFor is the id this fake will assign to a broadcast of these bytes, so a
// test can name a transaction before sending it.
func TxIDFor(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// --- scripting ---------------------------------------------------------------

// RejectNext makes the next n broadcasts fail with a tx-level rejection, the
// shape arcade uses for "failed to validate".
func (s *Server) RejectNext(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectNext = n
}

// DropEvents stops queueing emitted events. Anything emitted while this is on
// is gone: arcade's own catch-up truncates at a frame cap, so beyond it the
// events are not late, they are lost.
func (s *Server) DropEvents(drop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropEvents = drop
}

// Stall holds the event stream open but silent — a connection that is
// established, healthy by every local check, and delivering nothing.
func (s *Server) Stall(stall bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stall = stall
}

// Advance moves a transaction to a new status and emits an event for it, as
// arcade's pipeline does.
func (s *Server) Advance(txid string, status arcade.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.records[txid]
	rec.TxID, rec.Status, rec.Timestamp = txid, status, time.Now()
	s.records[txid] = rec
	s.emitLocked(rec)
}

// AdvanceAll moves every transaction arcade knows about to a status. This is
// the block sweep: one event per transaction, all at once.
func (s *Server) AdvanceAll(status arcade.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for txid, rec := range s.records {
		rec.Status, rec.Timestamp = status, time.Now()
		s.records[txid] = rec
		s.emitLocked(rec)
	}
}

// Status is arcade's own view of a transaction, for assertions.
func (s *Server) Status(txid string) (arcade.Status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[txid]
	return rec.Status, ok
}

// Known is how many transactions arcade has been given.
func (s *Server) Known() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

func (s *Server) emitLocked(rec arcade.TxRecord) {
	if s.dropEvents {
		return
	}
	s.seq++
	s.pending = append(s.pending, arcade.StatusEvent{
		ID:     strconv.FormatInt(time.Now().UnixNano()+s.seq, 10),
		Record: rec,
	})
}

// --- handlers ----------------------------------------------------------------

func (s *Server) handleBroadcast(w http.ResponseWriter, r *http.Request) {
	// Real arcade derives the txid from the transaction bytes. The fake does the
	// same thing with a cheaper hash: the identity only has to be deterministic
	// and collision-free for distinct bodies, and duplicating Extended Format
	// parsing here would add a second implementation of something the SDK
	// already owns. Callers use TxIDFor to predict it.
	body, _ := io.ReadAll(r.Body)
	txid := TxIDFor(body)

	s.mu.Lock()
	reject := s.rejectNext > 0
	if reject {
		s.rejectNext--
	}
	status := arcade.StatusReceived
	if reject {
		status = arcade.StatusRejected
	}
	rec := arcade.TxRecord{TxID: txid, Status: status, Timestamp: time.Now()}
	if reject {
		rec.ExtraInfo = "PROCESSING (4): [ProcessTransaction][fake] failed to validate transaction"
	}
	s.records[txid] = rec
	s.emitLocked(rec)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if reject {
		w.WriteHeader(http.StatusBadRequest)
		rec.StatusCode = http.StatusBadRequest
	} else {
		w.WriteHeader(http.StatusAccepted)
		rec.StatusCode = http.StatusAccepted
	}
	_ = json.NewEncoder(w).Encode(rec)
}

func (s *Server) handleGetTx(w http.ResponseWriter, r *http.Request) {
	txid := r.PathValue("txid")
	s.mu.Lock()
	rec, ok := s.records[txid]
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		return
	}
	_ = json.NewEncoder(w).Encode(rec)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"healthy": true, "version": "fake", "blockHeight": s.tipHeight(),
	})
}

// handleEvents is the SSE stream. It writes queued events, sends keepalives
// while idle — arcade's client watchdog treats silence as a dead peer — and
// honours Stall by sending neither.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()

	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	keepalive := time.NewTicker(2 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			s.mu.Lock()
			stalled := s.stall
			s.mu.Unlock()
			if stalled {
				continue // silence, deliberately: not even a keepalive
			}
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-tick.C:
			s.mu.Lock()
			if s.stall || len(s.pending) == 0 {
				s.mu.Unlock()
				continue
			}
			batch := s.pending
			s.pending = nil
			s.mu.Unlock()

			for _, ev := range batch {
				body, err := json.Marshal(ev.Record)
				if err != nil {
					continue
				}
				_, _ = fmt.Fprintf(w, "id: %s\ndata: %s\n\n", ev.ID, body)
			}
			flusher.Flush()
		}
	}
}

// --- chaintracks -------------------------------------------------------------

func (s *Server) tipHeight() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.height
}

func (s *Server) handleHeight(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]uint64{"height": s.tipHeight()})
}

func (s *Server) handleTip(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(fakeHeader(s.tipHeight()))
}

func (s *Server) handleHeader(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseUint(r.PathValue("n"), 10, 64)
	w.Header().Set("Content-Type", "application/json")
	if err != nil || n > s.tipHeight() {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(fakeHeader(n))
}

// handleQuietStream keeps a chaintracks SSE stream open without ever producing
// a tip or reorg. The headers client requires the connection; nothing in these
// tests depends on its contents.
func (s *Server) handleQuietStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	<-r.Context().Done()
}

func fakeHeader(height uint64) map[string]any {
	h := strings.Repeat("0", 64-len(strconv.FormatUint(height, 16))) + strconv.FormatUint(height, 16)
	return map[string]any{
		"version": 536870912, "previousHash": strings.Repeat("1", 64),
		"merkleRoot": strings.Repeat("2", 64), "time": time.Now().Unix(),
		"bits": 545259519, "nonce": 0, "height": height, "hash": h,
	}
}
