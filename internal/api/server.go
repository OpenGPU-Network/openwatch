// Package api exposes the OpenWatch HTTP API. A single *http.Server
// serves /health, /metrics, /api/v1/containers, POST /api/v1/update,
// and POST /api/v1/update/:name. The server is optional: main.go
// only constructs it when OPENWATCH_HTTP_API=true.
//
// The server is deliberately thin — it holds no state of its own.
// Container state comes from a updater.StateStore, metrics come from
// a metrics.Metrics handler, and update triggers are dispatched back
// to a Watcher through the Trigger interface defined below. This
// separation keeps the HTTP layer decoupled from the watcher
// internals and makes the handlers trivial to unit test.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/openwatch/openwatch/internal/metrics"
	"github.com/openwatch/openwatch/internal/updater"
)

// DefaultAddr is the listen address used when the caller does not
// override it. Port 8080 matches the PRD.
const DefaultAddr = ":8080"

// shutdownTimeout bounds how long Shutdown waits for in-flight
// requests to finish during graceful termination. Ten seconds is
// enough for any healthy handler to complete while still capping
// how long a hanging client can hold the daemon in shutdown.
const shutdownTimeout = 10 * time.Second

// Trigger is the surface area the HTTP handlers need from the
// watcher. Expressed as an interface so api_test can pass a fake
// implementation without spinning up a real Docker client.
type Trigger interface {
	TriggerAll(ctx context.Context)
	TriggerByName(ctx context.Context, name string) bool
	State() *updater.StateStore
}

// Server bundles the HTTP server, the trigger interface, and the
// metrics handler behind a single Serve method. One instance per
// daemon; constructed by main.go only when OPENWATCH_HTTP_API=true.
type Server struct {
	addr    string
	trigger Trigger
	metrics *metrics.Metrics
	version string
	log     zerolog.Logger

	srv *http.Server
}

// Config bundles the knobs Server needs. Using a struct keeps the
// constructor signature stable as more optional fields accrete.
type Config struct {
	Addr    string
	Trigger Trigger
	Metrics *metrics.Metrics
	Version string
	Log     zerolog.Logger
}

// New constructs a Server with the provided configuration. The
// returned instance has not yet begun serving; call Serve to bind
// and start handling requests.
func New(cfg Config) *Server {
	addr := cfg.Addr
	if addr == "" {
		addr = DefaultAddr
	}
	version := cfg.Version
	if version == "" {
		version = "dev"
	}
	s := &Server{
		addr:    addr,
		trigger: cfg.Trigger,
		metrics: cfg.Metrics,
		version: version,
		log:     cfg.Log,
	}
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Serve starts the HTTP listener and blocks until ctx is cancelled.
// On cancellation it runs a bounded graceful shutdown — in-flight
// requests are given shutdownTimeout to complete before the process
// moves on. The returned error is nil on clean shutdown and
// non-nil only if the listener itself crashed unexpectedly.
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}

	errCh := make(chan error, 1)
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	s.log.Info().Str("addr", s.addr).Msg("http api listening")

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			s.log.Warn().Err(err).Msg("http api shutdown error")
		}
		s.log.Info().Msg("http api stopped")
		return nil
	case err := <-errCh:
		return err
	}
}

// routes wires URL → handler bindings onto a single ServeMux.
// Using the standard library mux keeps the package zero-external-deps
// for routing; the endpoint count is small enough that a heavier
// router adds nothing.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/health", s.handleHealth)
	if s.metrics != nil {
		mux.Handle("/metrics", s.metrics.Handler())
	}
	mux.HandleFunc("/api/v1/containers", s.handleContainers)

	// POST /api/v1/update triggers a full tick. POST /api/v1/update/<name>
	// triggers a single container. One path prefix covers both because
	// the standard mux does not natively support path parameters.
	mux.HandleFunc("/api/v1/update", s.handleUpdateAll)
	mux.HandleFunc("/api/v1/update/", s.handleUpdateByName)

	return mux
}

// writeJSON renders any value as a JSON response with the standard
// content type. Errors from the encoder are logged but cannot be
// turned into HTTP error responses once headers have been sent — we
// just drop them into the log at warn level.
func (s *Server) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		s.log.Warn().Err(err).Msg("encode json response failed")
	}
}

// writeError emits a sanitized JSON error body. The `error` field
// is a short human-readable string — we never return raw Go errors
// to clients because they may contain internal paths or Docker
// API details that leak infrastructure information.
func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	s.writeJSON(w, status, map[string]string{"error": message})
}

// handleHealth is the liveness endpoint used by orchestrators and
// humans. It always returns 200 — if the process is alive enough to
// answer HTTP it is alive enough to report healthy. Readiness
// concerns (Docker socket reachability, etc.) could live in a
// separate endpoint later.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": s.version,
	})
}

// containerDTO is the wire format for /api/v1/containers. We rewrap
// updater.ContainerState so we can present last_updated as a JSON
// null when the container has not been updated yet in this process,
// rather than the "0001-01-01T00:00:00Z" you'd get from a zero
// time.Time.
type containerDTO struct {
	Name        string  `json:"name"`
	Image       string  `json:"image"`
	Status      string  `json:"status"`
	LastChecked string  `json:"last_checked"`
	LastUpdated *string `json:"last_updated"`
}

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	state := s.trigger.State()
	if state == nil {
		s.writeJSON(w, http.StatusOK, []containerDTO{})
		return
	}

	snapshot := state.Snapshot()
	out := make([]containerDTO, 0, len(snapshot))
	for _, c := range snapshot {
		dto := containerDTO{
			Name:        c.Name,
			Image:       c.Image,
			Status:      string(c.Status),
			LastChecked: c.LastChecked.Format(time.RFC3339),
		}
		if !c.LastUpdated.IsZero() {
			ts := c.LastUpdated.Format(time.RFC3339)
			dto.LastUpdated = &ts
		}
		out = append(out, dto)
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleUpdateAll triggers a full tick. This endpoint ONLY serves
// POST /api/v1/update — the ServeMux matches both /api/v1/update
// and /api/v1/update/ under a trailing slash route, so we explicitly
// reject anything that isn't the exact path.
func (s *Server) handleUpdateAll(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/update" {
		s.writeError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Run the tick against the server's long-lived context so it
	// survives the HTTP request-scoped ctx cancellation. Using
	// context.Background here rather than r.Context is intentional:
	// 202 Accepted means "we have taken responsibility for this
	// work" and aborting mid-flight on client disconnect would
	// violate that contract. Daemon shutdown is handled separately
	// by Serve which calls Shutdown on the http.Server.
	s.trigger.TriggerAll(context.Background())
	s.writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (s *Server) handleUpdateByName(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/v1/update/")
	name = strings.Trim(name, "/")
	if name == "" {
		s.writeError(w, http.StatusBadRequest, "container name is required")
		return
	}

	if !s.trigger.TriggerByName(context.Background(), name) {
		s.writeError(w, http.StatusNotFound, "container not found")
		return
	}
	s.writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted":  true,
		"container": name,
	})
}
