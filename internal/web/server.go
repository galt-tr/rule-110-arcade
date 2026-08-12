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

// Server exposes the engine over HTTP.
type Server struct {
	engine *engine.Engine
	logger *slog.Logger
}

// New builds the HTTP handler set.
func New(e *engine.Engine, logger *slog.Logger) *Server {
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
	snap := s.engine.Snapshot()
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
	// Coalesce bursts: a generation of 128 cells produces 128 updates, and the
	// UI only needs the resulting state, not every intermediate one.
	const minInterval = 100 * time.Millisecond

	send := func() error {
		// Stream the tail only; the client merges it into the full history it
		// fetched from /api/state.
		data, err := json.Marshal(s.engine.SnapshotTail())
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	if err := send(); err != nil {
		return
	}
	for {
		changed := s.engine.Changed()
		select {
		case <-ctx.Done():
			return
		case <-changed:
			select {
			case <-ctx.Done():
				return
			case <-time.After(minInterval):
			}
			if err := send(); err != nil {
				return
			}
		case <-time.After(15 * time.Second):
			// Keepalive: proxies drop idle event streams.
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
	writeJSON(w, s.engine.Snapshot())
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
	snap := s.engine.Snapshot()
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
