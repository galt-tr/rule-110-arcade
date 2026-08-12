package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"runtime"
	"time"
)

// serveProfiles starts the profiling endpoints on their own listener.
//
// # Why not the UI's mux
//
// The UI server already carries an unauthenticated /api/control, and the
// deployment answers that by keeping the Service ClusterIP-only. Hanging
// /debug/pprof off the same mux would put a heap dump and a thirty-second CPU
// profile behind the same non-answer, on a port the manifests deliberately
// expose to the cluster. A separate address means the default of 127.0.0.1
// reaches nothing but a port-forward, and a deployment that wants it remotely
// has to say so.
//
// # Why the mutex and block profiles are turned on here
//
// The suspects in this application are lock acquisition frequency on one
// global RWMutex and a notify() that wakes 128 workers at once. Neither appears
// in a CPU profile — a goroutine blocked on a mutex is not on-CPU, so the
// profile of a run bottlenecked entirely on lock contention looks idle. The
// block and mutex profiles are the ones that name it, and they are off in Go
// unless something sets a rate.
//
// The rates are sampled rather than exhaustive because both are instrumented in
// the runtime's lock paths: SetMutexProfileFraction(N) reports one in N
// contention events, and SetBlockProfileRate(ns) samples blocking events
// against a nanosecond threshold. These values cost a few percent, which is
// worth paying only while measuring — hence the flag.
func serveProfiles(ctx context.Context, addr string, logger *slog.Logger) {
	// 1 in 5 contention events, and blocking events averaging one sample per
	// 10µs blocked. Fine enough to rank the contended locks against each other,
	// coarse enough not to become the bottleneck it is measuring.
	runtime.SetMutexProfileFraction(5)
	runtime.SetBlockProfileRate(10_000)

	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// No write timeout: /debug/pprof/profile holds the connection open for
		// its whole sampling window, which defaults to thirty seconds and is
		// routinely asked for longer.
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	go func() {
		logger.Warn("profiling endpoints are live; mutex and block profiling are on",
			"addr", addr, "url", "http://"+addr+"/debug/pprof/")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// Not fatal. Losing the profiler is not a reason to stop advancing
			// 128 chains, and the run it was started for is still worth having
			// with only /metrics to read.
			logger.Error("profiling server stopped", "err", err)
		}
	}()
}
