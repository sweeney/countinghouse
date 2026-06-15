// Package httpapi hosts countinghouse's read-side JSON HTTP API.
//
// It mirrors statehouse's server conventions: handlers are methods on Server,
// routes are centralised in newMux (so tests exercise exactly the running
// routes), and Start(ctx) runs an http.Server with a 5s graceful shutdown.
// Two paths are public (/healthz, /openapi.json); data routes (added in a later
// milestone) are wrapped by authMiddleware, which accepts both user and service
// tokens.
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/influx"
	"github.com/sweeney/countinghouse/internal/testutil"
	"github.com/sweeney/identity/common/auth"
	"github.com/sweeney/identity/common/spec"
)

// ConfigProvider supplies the current remote-config snapshots the data
// handlers need. The real implementation is milestone 7's Fetcher (which
// refreshes both namespaces on SIGHUP); tests inject a fake. Both methods
// return the latest snapshot and must be safe for concurrent use.
type ConfigProvider interface {
	// Devices returns the current statehouse_devices snapshot keyed by
	// device_id. Used for class-based query routing and bill grouping.
	Devices() map[string]config.DeviceConfig
	// Tariffs returns the current energy_tariffs snapshot.
	Tariffs() config.EnergyTariffs
}

// ConfigStatus surfaces the remote-config fetcher's per-namespace status for
// /healthz. The Fetcher satisfies it; tests inject a fake. May be nil (then
// /healthz omits remote_config).
type ConfigStatus interface {
	Statuses() map[string]config.NamespaceStatus
}

// Server hosts the JSON HTTP API.
type Server struct {
	// Listen is the bind address, e.g. ":8080".
	Listen string

	// Influx is the read-side query client. Used by /healthz for a reachability
	// ping; data handlers (later milestone) query through it too. May be nil in
	// tests that don't exercise Influx.
	Influx influx.Querier

	// Logger receives structured output. May be nil.
	Logger *slog.Logger

	// IdentityURL is the base URL of the identity service (e.g.
	// "https://id.swee.net"). When set, data routes require a valid Bearer JWT
	// (user OR service token). When empty, auth is disabled (local dev/tests).
	IdentityURL string

	// PublicURL is the externally-reachable base URL of this server. When set it
	// is substituted into the OpenAPI spec's servers list; empty leaves the
	// placeholder as-is.
	PublicURL string

	// Version is the build commit set via -ldflags; empty when running outside a
	// tagged deploy.
	Version string

	// Bucket is the Influx bucket the data handlers query (e.g. "statehouse").
	// main.go sets it from config.
	Bucket string

	// Clock sources the current time for window resolution. Logic must never
	// call time.Now() directly. Defaults to testutil.RealClock{} when nil.
	Clock testutil.Clock

	// Loc is the timezone calendar window boundaries are computed in. Defaults
	// to time.UTC when nil; main.go sets Europe/London from config.
	Loc *time.Location

	// Config supplies the current device + tariff snapshots. The real impl is
	// milestone 7's Fetcher; tests inject a fake. May be nil only for the
	// public-route tests (data handlers require it).
	Config ConfigProvider

	// RemoteConfig surfaces per-namespace remote-config fetch status on
	// /healthz. The real impl is the Fetcher (which satisfies ConfigStatus);
	// tests may inject a fake or leave it nil (then /healthz omits the field).
	RemoteConfig ConfigStatus

	started time.Time

	// Atomic counters surfaced by /metrics. queryCount/queryErrors count Influx
	// queries issued by the data handlers; influxNanos accumulates their total
	// latency so /metrics can report an average.
	queryCount  atomic.Int64
	queryErrors atomic.Int64
	influxNanos atomic.Int64

	srv           *http.Server
	verifier      *auth.JWKSVerifier
	specConverter *spec.Converter
}

// clock returns the configured Clock, defaulting to a real clock.
func (s *Server) clock() testutil.Clock {
	if s.Clock != nil {
		return s.Clock
	}
	return testutil.RealClock{}
}

// loc returns the configured timezone, defaulting to UTC.
func (s *Server) loc() *time.Location {
	if s.Loc != nil {
		return s.Loc
	}
	return time.UTC
}

// New returns a configured Server. Optional fields (IdentityURL, PublicURL,
// Version, Logger) are set by the caller after construction, mirroring
// statehouse.
func New(listen string, querier influx.Querier, logger *slog.Logger) *Server {
	return &Server{
		Listen:  listen,
		Influx:  querier,
		Logger:  logger,
		started: time.Now().UTC(),
	}
}

// newMux builds and returns the ServeMux used by both Start and tests.
// Centralising route registration here means tests always exercise the same
// routes as the running server. Only the public routes exist now; data routes
// are registered (wrapped by the auth middleware) in a later milestone.
func newMux(s *Server) *http.ServeMux {
	s.specConverter = buildSpecConverter(s.PublicURL)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/openapi.json", s.handleOpenAPIJSON)

	// auth wraps every data route: a valid Bearer JWT (user OR service token)
	// is required when IdentityURL is set, and it is a no-op otherwise (dev/
	// tests). Building it here also wires s.verifier.
	auth := s.authMiddleware()
	mux.Handle("GET /devices", auth(http.HandlerFunc(s.handleDevices)))
	mux.Handle("GET /devices/{id}/energy", auth(http.HandlerFunc(s.handleDeviceEnergy)))
	mux.Handle("GET /devices/{id}/cost", auth(http.HandlerFunc(s.handleDeviceCost)))
	mux.Handle("GET /devices/{id}/series", auth(http.HandlerFunc(s.handleDeviceSeries)))
	mux.Handle("GET /devices/{id}/events", auth(http.HandlerFunc(s.handleDeviceEvents)))
	mux.Handle("GET /devices/{id}/intervals", auth(http.HandlerFunc(s.handleDeviceIntervals)))
	mux.Handle("GET /events", auth(http.HandlerFunc(s.handleEvents)))
	mux.Handle("GET /series", auth(http.HandlerFunc(s.handleSeries)))
	mux.Handle("GET /bill", auth(http.HandlerFunc(s.handleBill)))
	mux.Handle("GET /tariffs", auth(http.HandlerFunc(s.handleTariffs)))
	mux.Handle("GET /metrics", auth(http.HandlerFunc(s.handleMetrics)))
	return mux
}

// handler returns the fully-wrapped HTTP handler the server serves: the route
// mux behind the CORS middleware (so browser consumers can call the API).
func (s *Server) handler() http.Handler {
	return corsMiddleware(newMux(s))
}

// Start runs the HTTP server until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	// Stamp the start time if the Server was built via a struct literal
	// (main.go) rather than New(), so /healthz and /metrics report a real
	// started_at / uptime instead of deriving them from a zero time.
	if s.started.IsZero() {
		s.started = time.Now().UTC()
	}
	s.srv = &http.Server{
		Addr:              s.Listen,
		Handler:           s.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		err := s.srv.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	type health struct {
		Status          string                            `json:"status"`
		Version         string                            `json:"version,omitempty"`
		StartedAt       time.Time                         `json:"started_at"`
		StartedAgo      int                               `json:"started_ago"`
		Goroutines      int                               `json:"goroutines"`
		InfluxReachable bool                              `json:"influx_reachable"`
		RemoteConfig    map[string]config.NamespaceStatus `json:"remote_config,omitempty"`
	}
	h := health{
		Version:    s.Version,
		StartedAt:  s.started,
		StartedAgo: int((time.Since(s.started) + 500*time.Millisecond) / time.Second),
		Goroutines: runtime.NumGoroutine(),
	}
	if s.Influx != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		h.InfluxReachable = s.Influx.Ping(ctx)
	}
	if s.RemoteConfig != nil {
		h.RemoteConfig = s.RemoteConfig.Statuses()
	}

	// Derive the aggregated verdict so a monitor watching the top-level status
	// (the obvious thing to alert on) sees an outage. Influx is the hard
	// dependency: without it no data route can answer, so an unreachable Influx
	// is "unavailable". A failing config namespace is only "degraded" — we still
	// serve the last-known-good snapshot. Otherwise "ok".
	h.Status = "ok"
	if s.Influx != nil && !h.InfluxReachable {
		h.Status = "unavailable"
	} else {
		for _, ns := range h.RemoteConfig {
			if !ns.OK {
				h.Status = "degraded"
				break
			}
		}
	}

	// The status code stays 200 for degraded/unavailable: /healthz is a
	// liveness/readiness *report*, not itself failing.
	writeJSON(w, http.StatusOK, h)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	// Encode into a buffer FIRST so a marshal failure (e.g. a non-finite
	// float64 — encoding/json cannot marshal NaN/±Inf) becomes a real 500
	// instead of a 200 with a truncated/empty body. Writing the status header
	// before encoding would flush it irreversibly, leaving a broken response
	// that looks successful.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := enc.Encode(v); err != nil {
		// Keep the error body JSON-typed too (http.Error would force
		// text/plain), matching the JSON error shape writeError uses.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal: response encoding failed"}`))
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
