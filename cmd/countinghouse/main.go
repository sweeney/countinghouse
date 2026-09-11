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

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/httpapi"
	"github.com/sweeney/countinghouse/internal/influx"
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
		BaseURL:            cfg.RemoteConfig.BaseURL,
		Tokens:             tokens,
		Logger:             logger,
		DevicesNamespace:   cfg.Site.DevicesNamespace,
		FloorplanNamespace: cfg.Site.FloorplanNamespace,
		// Empty means the legacy energy_tariffs document stays authoritative, so
		// this binary can be deployed with no config change and behave as before.
		AgreementsNamespace: cfg.RemoteConfig.AgreementsNamespace,
	}
	if cfg.RemoteConfig.BaseURL == "" {
		// Explicit local-dev opt-out: nothing is fetched, so the cold check below is
		// skipped rather than failed. An operator who names no config service has
		// said they expect empty snapshots; one who names it has not.
		logger.Warn("remote config base_url is empty; serving empty device/tariff/floorplan snapshots")
	} else {
		logger.Info("tariff document in force", "namespace", fetcher.TariffNamespace())
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

	server := &httpapi.Server{
		Listen:             cfg.HTTP.Listen,
		Influx:             influxClient,
		Bucket:             cfg.Influx.Bucket,
		Clock:              testutil.RealClock{},
		Loc:                location,
		Config:             fetcher,
		RemoteConfig:       fetcher,
		IdentityURL:        cfg.Identity.BaseURL,
		PublicURL:          cfg.HTTP.PublicURL,
		Version:            version,
		SiteID:             cfg.Site.ID,
		DevicesNamespace:   cfg.Site.DevicesNamespace,
		FloorplanNamespace: cfg.Site.FloorplanNamespace,
		Floorplan:          fetcher,
		Logger:             logger,
	}

	logger.Info("starting", "config", *configPath, "http", cfg.HTTP.Listen,
		"influx", cfg.Influx.URL, "timezone", cfg.House.Timezone, "version", version,
		"site", cfg.Site.ID, "devices_namespace", cfg.Site.DevicesNamespace,
		"floorplan_namespace", cfg.Site.FloorplanNamespace)

	ctx, cancel := signalContext()
	defer cancel()

	go watchSIGHUP(fetcher, logger)

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
		fetcher.Refresh(ctx)
		cancel()
		logger.Info("SIGHUP: remote config refresh complete")
	}
}
