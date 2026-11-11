// Package server exposes /metrics, /healthz and /readyz with graceful
// shutdown. Provider health never gates readiness: a failed collection must
// not stop Prometheus from scraping the last valid snapshot.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server owns the HTTP listener and its readiness state.
type Server struct {
	addr            string
	httpServer      *http.Server
	ready           atomic.Bool
	shuttingDown    atomic.Bool
	logger          *log.Logger
	shutdownTimeout time.Duration

	addrMu    sync.Mutex
	boundAddr string
}

// New creates a server. The listener is opened by Run, which marks the server
// ready as soon as it is accepting connections.
func New(addr string, gatherer prometheus.Gatherer, logger *log.Logger) *Server {
	s := &Server{
		addr:            addr,
		logger:          logger,
		shutdownTimeout: 10 * time.Second,
	}
	s.httpServer = &http.Server{
		Handler:           NewHandler(gatherer, s.ready.Load, s.shuttingDown.Load),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s
}

// NewHandler builds the HTTP routes. ready and shutting are functions so tests
// can exercise transitions without a listener.
func NewHandler(gatherer prometheus.Gatherer, ready, shutting func() bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", getOnly(promhttp.HandlerFor(gatherer, promhttp.HandlerOpts{
		ErrorHandling: promhttp.HTTPErrorOnError,
	})))
	mux.Handle("/healthz", getOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if shutting() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "shutting_down"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})))
	mux.Handle("/readyz", getOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case shutting():
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "shutting_down"})
		case ready():
			writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		default:
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "starting"})
		}
	})))
	mux.Handle("/", getOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})))
	return mux
}

// Run listens, serves, and shuts down gracefully when ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.addrMu.Lock()
	s.boundAddr = listener.Addr().String()
	s.addrMu.Unlock()
	s.ready.Store(true)
	if s.logger != nil {
		s.logger.Printf("listening on %s", listener.Addr())
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.httpServer.Serve(listener)
	}()

	select {
	case err := <-serveErr:
		s.ready.Store(false)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		s.shuttingDown.Store(true)
		s.ready.Store(false)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-serveErr
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// BoundAddr returns the actual listener address, or an empty string before
// Run has opened it. Useful for tests and for logs that must show the port.
func (s *Server) BoundAddr() string {
	s.addrMu.Lock()
	defer s.addrMu.Unlock()
	return s.boundAddr
}

func getOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
