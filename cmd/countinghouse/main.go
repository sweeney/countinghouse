// Command countinghouse runs the read-side energy cost/accounting HTTP service.
//
// Startup: load local config, build the outbound identity TokenSource and the
// remote-config Fetcher (refreshed once with a timeout), build the Influx query
// client, then serve the HTTP API until SIGINT/SIGTERM. A namespace that fetched
// nothing at startup aborts the boot — see requireWarmSnapshots — while every later
// refresh is fail-open. SIGHUP re-refreshes remote config in place without a restart.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sweeney/countinghouse/internal/collector"
	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/httpapi"
	"github.com/sweeney/countinghouse/internal/influx"
	"github.com/sweeney/countinghouse/internal/notify"
	"github.com/sweeney/countinghouse/internal/octopus"
	"github.com/sweeney/countinghouse/internal/prices"
	"github.com/sweeney/countinghouse/internal/testutil"
	"github.com/sweeney/identity/common/auth"
)

// version is set via -ldflags "-X main.version=...". "dev" when built plainly.
var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/countinghouse/config.yaml", "path to YAML config")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}
	// Legal but probably-unintended config. Not fatal: every one of these runs
	// correctly for the site deployed today, and the whole point of the site block
	// is that adding a second property must not be able to take down the first.
	for _, w := range cfg.Warnings() {
		logger.Warn("config: " + w)
	}

	// Build the outbound client_credentials token source and the remote-config
	// fetcher. The fetcher HOLDS the live device/tariff/floorplan snapshots that the
	// HTTP handlers query, so we always construct it (even when
	// remote_config.base_url is empty) to avoid a nil ConfigProvider in the
	// handlers — it then serves empty snapshots.
	tokens := &auth.TokenSource{
		BaseURL:      cfg.Identity.BaseURL,
		ClientID:     cfg.Identity.ClientID,
		ClientSecret: cfg.Identity.ClientSecret,
	}
	fetcher := &config.Fetcher{
		BaseURL: cfg.RemoteConfig.BaseURL,
		Tokens:  tokens,
		Logger:  logger,
		// The namespace pointers are NOT set here. They are resolved below from the
		// shared `sites` document — see ResolveNamespaces. The two local values are
		// fallbacks for partially-filled site entries; devices_namespace has no
		// local fallback at all, by design.
		FloorplanNamespace:        cfg.Site.FloorplanNamespace,
		EnergyAgreementsNamespace: cfg.Site.EnergyAgreementsNamespace,
	}
	if cfg.RemoteConfig.BaseURL == "" {
		// Explicit local-dev opt-out: nothing is fetched, so the cold check below is
		// skipped rather than failed. An operator who names no config service has
		// said they expect empty snapshots; one who names it has not.
		logger.Warn("remote config base_url is empty; serving empty device/tariff/floorplan snapshots")
	} else {
		// PHASE ONE: read `sites` to learn which namespaces this property uses.
		// Until this succeeds we cannot name the others, so there is nothing to
		// fail open onto — falling open would mean reading some other property's
		// data, or none at all. An error here aborts, consistent with the
		// cold-start rule.
		resolveCtx, cancelResolve := context.WithTimeout(context.Background(), 10*time.Second)
		warns, err := fetcher.ResolveNamespaces(resolveCtx, cfg.Site)
		cancelResolve()
		for _, w := range warns {
			logger.Warn("remote config: " + w)
		}
		if err != nil {
			logger.Error("resolving site namespaces", "error", err)
			os.Exit(1)
		}
		logger.Info("site namespaces resolved",
			"site", cfg.Site.ID,
			"devices", fetcher.DevicesNamespace,
			"floorplan", fetcher.FloorplanNamespace,
			"tariffs", fetcher.TariffNamespace())

		// PHASE TWO: fetch the namespaces just named.
		logger.Info("refreshing remote config", "url", cfg.RemoteConfig.BaseURL)
		refreshCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		fetcher.Refresh(refreshCtx)
		cancel()
		requireWarmSnapshots(fetcher, logger)
	}

	influxClient := influx.New(influx.Config{
		URL:    cfg.Influx.URL,
		Org:    cfg.Influx.Org,
		Bucket: cfg.Influx.Bucket,
		Token:  cfg.Influx.Token,
	})
	defer influxClient.Close()

	location := cfg.House.Location()

	// The price archive and its collectors. Built after the fetcher is warm,
	// because which tariffs need archiving is a property of the agreements
	// document rather than of local config.
	collectors, store, err := startCollectors(cfg, fetcher, location, logger)
	if err != nil {
		logger.Error("price archive", "error", err)
		os.Exit(1)
	}
	if store != nil {
		defer store.Close() //nolint:errcheck
	}

	ctx, cancel := signalContext()
	defer cancel()

	// Offsite backup of the archive, started before the server so /healthz reports a
	// real state from the first request rather than a nil provider for a moment.
	// Nil when unconfigured, which omits the block entirely.
	backups := startBackups(ctx, cfg, logger)

	server := &httpapi.Server{
		Listen:       cfg.HTTP.Listen,
		Influx:       influxClient,
		Bucket:       cfg.Influx.Bucket,
		Clock:        testutil.RealClock{},
		Loc:          location,
		Config:       fetcher,
		RemoteConfig: fetcher,
		IdentityURL:  cfg.Identity.BaseURL,
		PublicURL:    cfg.HTTP.PublicURL,
		Version:      version,
		SiteID:       cfg.Site.ID,
		// The RESOLVED namespaces, not the local config values — /healthz must
		// report what is actually being read, which is now usually what `sites`
		// said rather than anything in this file.
		DevicesNamespace:   fetcher.DevicesNamespace,
		FloorplanNamespace: fetcher.FloorplanNamespace,
		Floorplan:          fetcher,
		Logger:             logger,
		Prices:             priceHealth(collectors),
		// The same store the collector writes, handed over read-only: PriceReader
		// deliberately omits Put, so the HTTP layer cannot write to the archive even
		// by accident. Nil when no archive is configured, and the /prices routes
		// then answer 503.
		PriceReader: priceReader(store),
		Backups:     backups,
	}

	logger.Info("starting", "config", *configPath, "http", cfg.HTTP.Listen,
		"influx", cfg.Influx.URL, "timezone", cfg.House.Timezone, "version", version,
		"site", cfg.Site.ID, "devices_namespace", fetcher.DevicesNamespace,
		"floorplan_namespace", cfg.Site.FloorplanNamespace)

	go watchSIGHUP(fetcher, logger)

	for _, c := range collectors {
		// Each collector owns one tariff. Run blocks until ctx is cancelled, and is
		// fail-open internally: a sync failure is logged and the loop continues, so
		// a supplier outage costs freshness rather than the goroutine.
		go c.Run(ctx)
	}

	logger.Info("ready")
	if err := server.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("http server", "error", err)
	}

	logger.Info("shutting down")
}

// requireWarmSnapshots aborts startup when any configured namespace has never been
// fetched: BOOT NEEDS TRUTH, RUNNING KEEPS THE LAST TRUTH.
//
// Refresh is fail-open, which is right for every refresh after the first: there is a
// last-known snapshot, so a config-service outage costs freshness rather than
// correctness. At startup there is nothing to fall back to, and fail-open falls open
// onto emptiness — no devices, no tariffs, no floor names. The service then boots
// "successfully" and answers every question with zero kWh, no tariff, or ids where
// names belong, in the confident shape of a correct response. That is a wrong answer
// wearing the shape of a right one, which is the failure mode this service refuses
// everywhere else: it is why both site namespaces must be NAMED in config, and this is
// the same refusal one layer later, where the name turns out to fetch nothing.
//
// Refusing costs availability during a config-service outage that coincides with a
// restart, and that trade is deliberate: countinghouse is a read-side service, so being
// visibly down is strictly better than being invisibly wrong. systemd's restart loop
// then recovers the instant the config service returns, and the failure is legible in
// the unit's status rather than in a chart legend somebody eventually squints at.
//
// A SIGHUP reload deliberately does NOT go through here — see watchSIGHUP.
func requireWarmSnapshots(fetcher *config.Fetcher, logger *slog.Logger) {
	cold := fetcher.Cold()
	if len(cold) == 0 {
		return
	}
	// Terse on purpose — see the config package's refusals. The why is in this
	// function's doc comment and README.md; the log says what never arrived.
	logger.Error("remote config: refusing to start, nothing was ever fetched for "+
		strings.Join(cold, ", "), "cold_namespaces", cold)
	os.Exit(1)
}

// signalContext returns a context cancelled on SIGINT or SIGTERM, triggering
// the HTTP server's graceful (5s) shutdown.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}

// watchSIGHUP re-refreshes the remote-config snapshots on each SIGHUP, without
// restarting the process. Refresh is fail-open: a failed reload keeps the
// last-known-good snapshots.
//
// It does NOT apply the startup cold check, and the asymmetry is the point: by the
// time a SIGHUP arrives the process holds a snapshot for every namespace, so a failed
// reload is STALE, not cold. Killing a healthy instance over a transient config-service
// blip would turn fail-open's whole purpose inside out. /healthz reports the failing
// namespace and degrades; that is the signal for a stale reload.
func watchSIGHUP(fetcher *config.Fetcher, logger *slog.Logger) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	for range ch {
		logger.Info("SIGHUP: refreshing remote config")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		// Namespace pointers are settled at startup and deliberately NOT adopted
		// here: repointing a running service at a different property's data would
		// swap the device inventory underneath every in-flight answer. A change is
		// reported for an explicit restart, which is cheap.
		for _, d := range fetcher.SiteNamespaceDrift(ctx) {
			logger.Warn("SIGHUP: site namespaces have drifted", "detail", d)
		}
		fetcher.Refresh(ctx)
		cancel()
		logger.Info("SIGHUP: remote config refresh complete")
	}
}

// startCollectors opens the price archive and builds one collector per tariff that
// needs archiving.
//
// One collector per tariff, rather than one that handles a set, because every
// distinct half-hourly tariff code in the agreements document needs its own prices
// — including a SUPERSEDED one, whose window is still billable. Today that is
// exactly one collector; the shape costs nothing and means a tariff change does
// not silently strand the old prices.
//
// Returns no collectors and a nil store when no archive is configured, which is
// correct for a deployment whose agreements are all flat-rate: there are no
// half-hourly prices to keep. The combination that would fail SILENTLY — a
// half-hourly agreement with no archive — is refused here rather than allowed to
// boot and then be unable to price anything after the switchover.
func startCollectors(cfg config.Config, fetcher *config.Fetcher, loc *time.Location,
	logger *slog.Logger) ([]*collector.Collector, *prices.SQLiteStore, error) {

	agreements := fetcher.Agreements()
	if err := config.CheckArchiveRequired(cfg.Prices.DBPath, agreements); err != nil {
		return nil, nil, err
	}

	codes := agreements.VariableTariffCodes()
	if !cfg.Prices.Enabled() {
		logger.Info("price archive disabled (no prices.db_path); no collector will run")
		return nil, nil, nil
	}
	if len(codes) == 0 {
		// An archive is configured but nothing needs it yet. Not an error — it is
		// what a deployment preparing to switch tariffs looks like — but worth
		// saying, because an operator who expected prices to arrive should not have
		// to guess why they have not.
		logger.Warn("price archive configured but no agreement is half-hourly; nothing to collect",
			"db_path", cfg.Prices.DBPath)
		return nil, nil, nil
	}

	store, err := prices.Open(cfg.Prices.DBPath)
	if err != nil {
		return nil, nil, err
	}

	client, err := octopus.New(octopus.Options{
		BaseURL:    cfg.Prices.OctopusBaseURL,
		Clock:      testutil.RealClock{},
		MaxRetries: 3,
		Logger:     logger,
	})
	if err != nil {
		store.Close() //nolint:errcheck
		return nil, nil, err
	}

	// slog is the only transport, and it cannot fail. Throttled so a condition that
	// stays true across many five-minute ticks is reported once rather than dozens
	// of times — the reliable outcome of the latter being a log nobody reads.
	notifier := notify.NewThrottle(notify.NewSlogNotifier(logger), time.Hour, testutil.RealClock{})

	// VAT comes from the agreement covering each SLOT, since it is a property of the
	// tariff in force when that price applied rather than of the collector.
	//
	// Previously this took the rate from the agreement in force at boot and handed
	// one number to every collector, which was wrong three ways: frozen for the
	// process lifetime so a SIGHUP'd VAT change or a midnight agreement rollover was
	// never picked up; sourced from `now` rather than from the agreement owning each
	// tariff code, so backfilling a superseded tariff whose VAT differed used today's
	// rate; and — with no else branch — silently 0 when no agreement covered `now`,
	// which at the time meant Gate A rejecting 100% of slots for vat_mismatch while
	// /healthz reported fetches succeeding.
	vatAt := vatRateAt(fetcher)

	out := make([]*collector.Collector, 0, len(codes))
	for _, code := range codes {
		c, err := collector.New(collector.Options{
			Fetcher:    client,
			Store:      store,
			Notifier:   notifier,
			Clock:      testutil.RealClock{},
			Location:   loc,
			TariffCode: code,
			VATRateAt:  vatAt,
			Logger:     logger,
		})
		if err != nil {
			store.Close() //nolint:errcheck
			return nil, nil, err
		}
		out = append(out, c)
		logger.Info("price collector ready", "tariff_code", code, "db_path", cfg.Prices.DBPath)
	}
	return out, store, nil
}

// priceHealth adapts the collectors' Status to the HTTP layer's own type, so
// internal/httpapi need not import the collector.
//
// Returns nil when there are no collectors, which is what makes /healthz and
// /metrics omit the block rather than render an empty one — a zeroed block would
// read as a broken archive rather than as no archive.
func priceHealth(collectors []*collector.Collector) httpapi.PricesProvider {
	if len(collectors) == 0 {
		return nil
	}
	return priceHealthFunc(func() []httpapi.PriceHealth {
		out := make([]httpapi.PriceHealth, 0, len(collectors))
		for _, c := range collectors {
			st := c.Status()
			out = append(out, httpapi.PriceHealth{
				TariffCode:  st.TariffCode,
				KnownTo:     st.KnownTo,
				CompleteTo:  st.CompleteTo,
				LastAttempt: st.LastAttempt,
				LastSuccess: st.LastSuccess,
				LastError:   st.LastError,
				Syncs:       st.Syncs,
				Failures:    st.Failures,
				Inserted:    st.Inserted,
				Restated:    st.Restated,
				Rejected:    st.Rejected,
				Warnings:    st.Warnings,
			})
		}
		return out
	})
}

// priceHealthFunc lets a plain function satisfy httpapi.PricesProvider.
type priceHealthFunc func() []httpapi.PriceHealth

func (f priceHealthFunc) PriceHealth() []httpapi.PriceHealth { return f() }

// priceReader hands the archive to the HTTP layer read-only.
//
// Returns a nil interface when there is no store, rather than a non-nil interface
// holding a nil pointer — the handlers check for nil to decide between serving and
// answering 503, and a typed nil would pass that check and then panic.
func priceReader(store *prices.SQLiteStore) httpapi.PriceReader {
	if store == nil {
		return nil
	}
	return store
}

// vatRateAt returns a resolver for the VAT rate in force at an instant, reading the
// LIVE agreements each call.
//
// Live, not captured: the snapshot is re-read per slot, so a VAT change adopted by
// SIGHUP and an agreement rolling over at midnight are both picked up without a
// restart. Returning false when nothing covers the instant is the whole point — the
// caller must express no opinion rather than check against a rate of zero, because
// zero is a legal VAT rate and therefore cannot double as "I don't know".
func vatRateAt(src interface {
	Tariffs() config.TariffSource
}) func(time.Time) (float64, bool) {
	return func(at time.Time) (float64, bool) {
		t, ok := src.Tariffs().TariffFor(at)
		if !ok {
			return 0, false
		}
		return t.VATRate, true
	}
}
