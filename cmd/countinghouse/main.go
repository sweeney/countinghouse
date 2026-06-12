// Command countinghouse runs the read-side energy cost/accounting HTTP service.
//
// Startup: load local config, build the outbound identity TokenSource and the
// remote-config Fetcher (refreshed once with a timeout, fail-open), build the
// Influx query client, then serve the HTTP API until SIGINT/SIGTERM. SIGHUP
// re-refreshes remote config in place without a restart.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
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

	// Build the outbound client_credentials token source and the remote-config
	// fetcher. The fetcher HOLDS the live device/tariff snapshots that the HTTP
	// handlers query, so we always construct it (even when remote_config.base_url
	// is empty) to avoid a nil ConfigProvider in the handlers — it then serves
	// empty snapshots. Refresh is fail-open: a config-service outage at startup
	// leaves empty snapshots rather than aborting the process.
	tokens := &auth.TokenSource{
		BaseURL:      cfg.Identity.BaseURL,
		ClientID:     cfg.Identity.ClientID,
		ClientSecret: cfg.Identity.ClientSecret,
	}
	fetcher := &config.Fetcher{
		BaseURL: cfg.RemoteConfig.BaseURL,
		Tokens:  tokens,
		Logger:  logger,
	}
	if cfg.RemoteConfig.BaseURL == "" {
		logger.Warn("remote config base_url is empty; serving empty device/tariff snapshots")
	} else {
		logger.Info("refreshing remote config", "url", cfg.RemoteConfig.BaseURL)
		refreshCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		fetcher.Refresh(refreshCtx)
		cancel()
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
		Logger:       logger,
	}

	logger.Info("starting", "config", *configPath, "http", cfg.HTTP.Listen,
		"influx", cfg.Influx.URL, "timezone", cfg.House.Timezone, "version", version)

	ctx, cancel := signalContext()
	defer cancel()

	go watchSIGHUP(fetcher, logger)

	logger.Info("ready")
	if err := server.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("http server", "error", err)
	}

	logger.Info("shutting down")
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
