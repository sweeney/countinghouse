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
	// Tariffs returns the authoritative tariff document as a TariffSource.
	//
	// An interface, not a concrete document: countinghouse can be configured
	// against the legacy `energy_tariffs` single rate or the dated
	// `energy_agreements` blocks, and no handler should know which. Both answer
	// TariffFor(t) and PeriodsBetween identically.
	Tariffs() config.TariffSource

	// Agreements returns the same document in the dated-block shape, so /tariffs
	// serves one response shape either way. A legacy document is presented as a
	// single open-ended fixed agreement.
	Agreements() config.EnergyAgreements

	// TariffNamespace names whichever namespace is authoritative, so a consumer
	// need not deduce it from the shape of the answer.
	TariffNamespace() string
}

// FloorplanProvider supplies the floorplan snapshot behind /floors and /rooms,
// and the display names grouped series are labelled with. The Fetcher satisfies
// it; tests inject a fake.
//
// Server.Floorplan may be nil — tests wire it that way, and so would a Server
// built by hand — and both catalogs then still list everything that holds a
// metered device, with names, storey order and category reported as unknown, and
// grouped series stay labelled by id. That degradation is what keeps a floorplan
// outage from becoming a billing outage; it is NOT an invitation to run without
// one, which config.Load refuses.
//
// One interface rather than two because both collections come from one document
// in one namespace: splitting them would let a caller hold half a floorplan and
// suggest the halves can be configured independently, which they cannot.
type FloorplanProvider interface {
	// Floors returns the current floor records keyed by floor id.
	Floors() map[string]config.FloorConfig
	// Rooms returns the current room records keyed by floorplan room id.
	Rooms() map[string]config.RoomConfig
}

// ConfigStatus surfaces the remote-config fetcher's per-namespace status for
// /healthz. The Fetcher satisfies it; tests inject a fake. May be nil (then
// /healthz omits remote_config).
type ConfigStatus interface {
	Statuses() map[string]config.NamespaceStatus
}

// Server hosts the JSON HTTP API.
type Server struct {
	// Listen is the bind address, e.g. ":8585".
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

	// SiteID and DevicesNamespace are the resolved site config, reported on
	// /healthz so an operator can see which property this instance believes it
	// serves rather than inferring it from whether the numbers look plausible.
	// Both empty on an instance predating the per-site split, which is not a fault.
	SiteID           string
	DevicesNamespace string

	// FloorplanNamespace is the floorplan namespace this instance reads floor and
	// room records from, reported on /healthz beside the devices one so an
	// operator can see which property's floorplan it believes it serves. Load
	// requires it, so it is empty only on a Server built by hand.
	FloorplanNamespace string

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

	// Floorplan supplies floor and room records for /floors, /rooms and the
	// labels on grouped series. The real impl is the Fetcher; tests inject a fake
	// or leave it nil — the catalogs then report names, order and category as
	// unknown, and grouped series stay labelled by id, rather than failing.
	Floorplan FloorplanProvider

	// Prices reports price-archive health for /healthz and /metrics. Nil when no
	// collector runs, which is the normal case for a deployment whose tariff
	// agreements are all flat-rate: both blocks are then omitted rather than
	// rendered empty, since a zeroed block would read as a broken archive rather
	// than as no archive.
	Prices PricesProvider

	// PriceReader serves the /prices endpoints from the archive. Nil when no
	// archive is configured, and those routes then answer 503 — the route exists
	// and would work elsewhere, so it is a deployment state rather than a bad
	// request or a missing endpoint.
	PriceReader PriceReader

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

	// driftBuckets accumulates C3 negative-residual drift buckets (meter below
	// monitored beyond the 0.1 kWh counter quantum) observed across served series
	// requests. A non-zero, growing value means the unmonitored decomposition is
	// being distorted — investigate via the WARN logs / unclamped=true.
	driftBuckets atomic.Int64

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

// floors returns the current floor records, or nil when no floorplan provider is
// configured. A nil map reads as empty, so /floors degrades to "every floor is
// unknown" rather than panicking on an instance with no floorplan namespace.
func (s *Server) floors() map[string]config.FloorConfig {
	if s.Floorplan == nil {
		return nil
	}
	return s.Floorplan.Floors()
}

// rooms returns the current room records, nil-safe for the same reason as floors.
func (s *Server) rooms() map[string]config.RoomConfig {
	if s.Floorplan == nil {
		return nil
	}
	return s.Floorplan.Rooms()
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
	mux.Handle("GET /floors", auth(http.HandlerFunc(s.handleFloors)))
	mux.Handle("GET /rooms", auth(http.HandlerFunc(s.handleRooms)))
	mux.Handle("GET /devices/{id}/energy", auth(http.HandlerFunc(s.handleDeviceEnergy)))
	mux.Handle("GET /devices/{id}/cost", auth(http.HandlerFunc(s.handleDeviceCost)))
	mux.Handle("GET /devices/{id}/series", auth(http.HandlerFunc(s.handleDeviceSeries)))
	mux.Handle("GET /devices/{id}/events", auth(http.HandlerFunc(s.handleDeviceEvents)))
	mux.Handle("GET /devices/{id}/intervals", auth(http.HandlerFunc(s.handleDeviceIntervals)))
	mux.Handle("GET /events", auth(http.HandlerFunc(s.handleEvents)))
	mux.Handle("GET /series", auth(http.HandlerFunc(s.handleSeries)))
	mux.Handle("GET /bill", auth(http.HandlerFunc(s.handleBill)))
	mux.Handle("GET /tariffs", auth(http.HandlerFunc(s.handleTariffs)))
	mux.Handle("GET /prices", auth(http.HandlerFunc(s.handlePrices)))
	mux.Handle("GET /prices/upcoming", auth(http.HandlerFunc(s.handleUpcomingPrices)))
	mux.Handle("GET /prices/cheapest", auth(http.HandlerFunc(s.handleCheapestPrice)))
	mux.Handle("GET /prices/stats", auth(http.HandlerFunc(s.handlePriceStats)))
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

// siteHealth is the resolved site config as reported by /healthz. It is the answer to
// "which property is this instance serving, and where is it reading that property's
// devices from" — a question that was previously only answerable by reading the host's
// config file, or by noticing the numbers were wrong.
type siteHealth struct {
	ID               string `json:"id,omitempty"`
	DevicesNamespace string `json:"devices_namespace,omitempty"`
	// FloorplanNamespace answers a question the remote_config block cannot: that
	// block distinguishes "configured and failing" from "configured and fine"
	// only AFTER a fetch attempt, so an operator seeing blank room names cannot
	// otherwise tell "first fetch hasn't landed" from "the records are genuinely
	// unnamed upstream". omitempty covers a Server built without one.
	FloorplanNamespace string `json:"floorplan_namespace,omitempty"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	type health struct {
		Status          string                            `json:"status"`
		Version         string                            `json:"version,omitempty"`
		StartedAt       time.Time                         `json:"started_at"`
		StartedAgo      int                               `json:"started_ago"`
		Goroutines      int                               `json:"goroutines"`
		InfluxReachable bool                              `json:"influx_reachable"`
		Site            *siteHealth                       `json:"site,omitempty"`
		RemoteConfig    map[string]config.NamespaceStatus `json:"remote_config,omitempty"`
		Prices          []PriceHealth                     `json:"prices,omitempty"`
	}
	h := health{
		Version:    s.Version,
		StartedAt:  s.started,
		StartedAgo: int((time.Since(s.started) + 500*time.Millisecond) / time.Second),
		Goroutines: runtime.NumGoroutine(),
	}
	if s.SiteID != "" || s.DevicesNamespace != "" || s.FloorplanNamespace != "" {
		h.Site = &siteHealth{
			ID:                 s.SiteID,
			DevicesNamespace:   s.DevicesNamespace,
			FloorplanNamespace: s.FloorplanNamespace,
		}
	}
	if s.Influx != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		h.InfluxReachable = s.Influx.Ping(ctx)
	}
	if s.RemoteConfig != nil {
		h.RemoteConfig = s.RemoteConfig.Statuses()
	}
	if s.Prices != nil {
		h.Prices = s.Prices.PriceHealth()
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
		// A price problem degrades on the same reasoning as a config namespace: the
		// archive still holds what it held, so historical windows still price, but
		// we are either not keeping up or cannot price TODAY — and the top-level
		// status is what a monitor actually watches.
		if degraded, _ := priceVerdict(h.Prices, s.clock().Now()); degraded {
			h.Status = "degraded"
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
