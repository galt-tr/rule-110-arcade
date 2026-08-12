// Package web serves the automaton's visualisation.
//
// Updates are pushed over Server-Sent Events rather than polled: the engine
// already signals every change, and SSE needs no dependency and no handshake.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/dymurray/rule-110-arcade/internal/engine"
)

//go:embed static
var staticFS embed.FS

// staticRoot serves the embedded assets at the URL root rather than under
// /static/. Resolved once at startup; a failure here means the embed directive
// and the directory disagree, which is a build mistake, not a runtime one.
var staticRoot = func() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return sub
}()

// Automaton is what the HTTP layer needs from the engine.
//
// Narrow deliberately, and an interface rather than the concrete type so the
// handlers can be tested without standing up a wallet, a store and a chain. The
// engine satisfies it.
type Automaton interface {
	// Snapshot is the full in-memory history, for the initial page load.
	Snapshot() engine.Snapshot
	// SnapshotTail is the recent window, which the client merges by generation
	// number.
	SnapshotTail() engine.Snapshot
	// Stats is the scalars with no history, for metrics and readiness.
	Stats() engine.Snapshot
	// PublishedTail is the tail already marshalled, shared by every subscriber. The
	// pointer is the identity of a publish: unchanged means the subscriber has
	// already sent this exact frame.
	PublishedTail() (*[]byte, bool)
	// Changed is closed the next time the snapshot changes.
	Changed() <-chan struct{}

	SetMode(engine.Mode)
	SetRate(float64)
	Step()
}

// Server exposes the engine over HTTP.
type Server struct {
	engine Automaton
	logger *slog.Logger
}

// New builds the HTTP handler set.
func New(e Automaton, logger *slog.Logger) *Server {
	return &Server{engine: e, logger: logger}
}

// Handler returns the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /", http.FileServer(http.FS(staticRoot)))
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/control", s.handleControl)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	return mux
}

// handleHealth is liveness: the process is up and serving. It deliberately
// says nothing about the automaton — restarting a pod does not conjure coin or
// mine a block, so tying liveness to progress would just add a crash loop to
// whatever was already wrong.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleReady is readiness: this instance can serve.
//
// Neither starved nor read-only is unready. A starved automaton is waiting for
// a payment and its UI is exactly where an operator finds the address to send
// it to; an instance without the writer lease is serving correct history and is
// the standby that takes over. Failing readiness in either case would remove
// the endpoint that explains the situation.
func (s *Server) handleReady(w http.ResponseWriter, _ *http.Request) {
	snap := s.engine.Stats()
	writeJSON(w, map[string]any{
		"status":  "ok",
		"leader":  snap.Leader,
		"starved": snap.Starved,
	})
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.engine.Snapshot())
}

// handleEvents streams snapshots as they change.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx := r.Context()

	// Write the tail the engine has already built and marshalled. Every
	// subscriber writes the same bytes: the work of a push is done once for the
	// process, on the publisher's goroutine, rather than once per client here.
	// The client merges the tail into the full history it fetched from
	// /api/state.
	//
	// `sent` is what stops this sending the SAME tail over and over. The engine
	// coalesces the CONTENT of a publish — PublishTails throttles rebuilding to
	// PublishInterval — but it does not coalesce the notifications, and
	// notify() fires on every recorded cell, every status batch and every clock
	// tick. So between two publishes this loop wakes many times per second and,
	// without the check below, wrote a byte-identical frame every time. At 256
	// cells that frame is hundreds of kilobytes and the duplicates were the
	// dominant cost of a connected browser.
	//
	// The comparison is on the slice header, not the bytes: PublishTail stores a
	// new pointer for each rebuild and never mutates a published slice, so
	// pointer identity is exactly "this is the tail I already sent".
	var sent *[]byte
	send := func() error {
		data, ok := s.engine.PublishedTail()
		if !ok {
			return nil // nothing published yet; the next change will bring one
		}
		if data == sent {
			return nil
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", *data); err != nil {
			return err
		}
		flusher.Flush()
		sent = data
		return nil
	}

	// Subscribe BEFORE sending, here and on every pass below. Acquiring the
	// channel afterwards leaves a window in which a change closes a channel
	// nobody holds, stranding that update until some later change happens to
	// fire — which, once the automaton goes quiet, is never. That showed up as a
	// stale final frame after pause, because the 15s keepalive carries no data.
	changed := s.engine.Changed()
	if err := send(); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-changed:
			changed = s.engine.Changed()
			// No wait: bursts are already coalesced by the engine's publisher,
			// so this sends on the leading edge rather than delaying every
			// update by a fixed interval to coalesce the few that need it.
			if err := send(); err != nil {
				return
			}
		case <-time.After(15 * time.Second):
			// Keepalive: proxies drop idle event streams. Deliberately not a
			// data frame — nothing has changed, and re-sending the tail every
			// 15s to an idle browser is bandwidth for no information.
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// controlRequest is the UI's command envelope.
type controlRequest struct {
	Action string  `json:"action"`
	Rate   float64 `json:"rate,omitempty"`
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	// Refuse before reading the body, before touching the engine, and for every
	// action alike. This endpoint has no authentication, so on a public
	// deployment it is one HTTP request from anyone; hiding the buttons in the
	// browser is presentation, not a lock. The engine's own setters refuse too —
	// see engine.Options.LockControls — and neither layer relies on the other.
	if s.engine.Stats().Locked {
		http.Error(w, "the clock is fixed for this deployment", http.StatusForbidden)
		return
	}
	var req controlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	switch req.Action {
	case "play":
		s.engine.SetMode(engine.ModeRunning)
	case "pause":
		s.engine.SetMode(engine.ModePaused)
	case "step":
		s.engine.SetMode(engine.ModePaused)
		s.engine.Step()
	case "rate":
		s.engine.SetRate(req.Rate)
	default:
		http.Error(w, "unknown action "+strconv.Quote(req.Action), http.StatusBadRequest)
		return
	}
	// The tail, not the whole history: the client merges by generation number,
	// so a tail carries everything a control response can usefully say. Returning
	// the full history meant every button click copied all of maxHistory.
	writeJSON(w, s.engine.SnapshotTail())
}

// Serve runs the HTTP server until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: s.Handler(),
		// No write timeout: the SSE stream is deliberately long-lived.
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("web: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, "encode error", http.StatusInternalServerError)
	}
}

// handleMetrics exposes the automaton in Prometheus text format.
//
// Hand-rolled rather than pulling in a client library: it is a dozen gauges off
// a snapshot the engine already builds, and the exposition format is stable.
//
// The choice of what to expose is deliberate. Every number here is one this
// session actually needed to diagnose something: a stalled automaton with no
// errors turned out to be 127 cells queueing for one coin, and a frontier that
// would not move turned out to be the depth governor doing its job. Those are
// indistinguishable from the outside without waiting_on_coin and depth.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	// Stats, not Snapshot: every number below is a scalar, and a scrape happens
	// on a timer whether or not anyone is watching the diagram.
	snap := s.engine.Stats()
	bool01 := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}

	metrics := []struct {
		name, help, kind string
		value            float64
	}{
		{"rule110_generation", "Newest generation every cell has proved.", "gauge", float64(snap.Generation)},
		{"rule110_transactions_total", "Cell transitions broadcast since startup.", "counter", float64(snap.TotalTx)},
		{"rule110_cells", "Cells in the ring.", "gauge", float64(snap.Cells)},
		{"rule110_halted_cells", "Cells that can never advance again until their tip is recovered.", "gauge", float64(snap.HaltedCells)},
		{"rule110_proved_cells", "Cells of the newest generation whose transition the network accepted. Not a measure of cross-cell agreement, which Script does not check.", "gauge", float64(snap.ProvedCells)},
		{"rule110_failed_cells", "Failures in the newest generation.", "gauge", float64(snap.FailedCells)},
		{"rule110_lag_generations", "How far the clock has run ahead of the slowest cell.", "gauge", float64(snap.Lag)},
		{"rule110_unconfirmed_depth", "Deepest unconfirmed chain, against the mempool ancestor limit.", "gauge", float64(snap.Depth)},
		{"rule110_waiting_on_coin", "Cells retrying a funding shortfall.", "gauge", float64(snap.WaitingOnCoin)},
		{"rule110_spendable_satoshis", "Satoshis the funder can actually claim right now.", "gauge", float64(snap.Balance)},
		{"rule110_reserve_satoshis", "Satoshis left to mint more spendable coin from.", "gauge", float64(snap.Reserve)},
		{"rule110_pool_coins", "Claimable coins backing the spendable balance; one funds one transition.", "gauge", float64(snap.PoolCoins)},
		{"rule110_starved", "1 when stopped for want of funding.", "gauge", float64(bool01(snap.Starved))},
		{"rule110_leader", "1 when this instance holds the single-writer lease.", "gauge", float64(bool01(snap.Leader))},
		{"rule110_rate_generations_per_second", "Configured clock rate.", "gauge", snap.Rate},
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	for _, m := range metrics {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %g\n", m.name, m.help, m.name, m.kind, m.name, m.value)
	}
}
