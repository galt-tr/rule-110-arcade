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
	return mux
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
		data, err := json.Marshal(s.engine.Snapshot())
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
