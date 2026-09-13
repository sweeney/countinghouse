package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"time"

	"github.com/sweeney/countinghouse/internal/collector"
	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/notify"
	"github.com/sweeney/countinghouse/internal/octopus"
	"github.com/sweeney/countinghouse/internal/prices"
	"github.com/sweeney/countinghouse/internal/testutil"
)

// ---------------------------------------------------------------------------
// Collect-only mode: fill the price archive and nothing else.
//
// The supplier's rate endpoints need NO AUTHENTICATION — they are public product
// data — so collecting prices needs neither an API key, nor Influx, nor the remote
// config service, nor identity. That is the whole reason this mode can exist: the
// archive is the one part of countinghouse that can run with a tariff code and a
// file path and no other dependency at all.
//
// It exists for three jobs:
//
//   - start accumulating real half-hourly prices NOW, on any machine, before the
//     full service is deployed. Several open questions on this work — the
//     counter-lag phase bias in docs/per-device-attribution.md most of all — are
//     measurements waiting on data rather than decisions waiting on thought, and
//     they need weeks of real prices that only exist if something is collecting.
//   - a one-shot backfill, which was a deferred plan item.
//   - reproducing a collector problem against the live API without standing up the
//     service around it.
//
// Deliberately NOT a second binary: it is the same collector, the same store, the
// same validation gates and the same migrations as production. A separate tool would
// be free to drift from them, and the data it wrote would then be a different
// archive wearing the same schema.
// ---------------------------------------------------------------------------

// collectFlags is the collect-only flag set.
type collectFlags struct {
	dbPath  string
	tariff  string
	baseURL string
	vat     float64
	once    bool
	backTo  string
}

// registerCollectFlags adds the collect-only flags to fs.
func registerCollectFlags(fs *flag.FlagSet) *collectFlags {
	c := &collectFlags{}
	fs.StringVar(&c.dbPath, "prices-db", "", "path to the price archive (created if absent)")
	fs.StringVar(&c.tariff, "tariff", "", "half-hourly tariff code, e.g. E-1R-AGILE-24-10-01-X")
	fs.StringVar(&c.baseURL, "octopus-url", config.DefaultOctopusBaseURL, "supplier API root")
	// No default: 0 means "no opinion", which is what the gate wants when we cannot
	// name the rate. Passing it is optional precisely because pricing never uses it —
	// it only checks the supplier's inc/exc relationship.
	fs.Float64Var(&c.vat, "vat", 0, "expected VAT rate for the inc/exc check (0 = do not check)")
	fs.BoolVar(&c.once, "once", false, "sync once and exit, instead of running continuously")
	fs.StringVar(&c.backTo, "back-to", "", "also backfill from this date (YYYY-MM-DD) before starting")
	return c
}

// runCollect runs the collect-only mode. Returns an error rather than exiting, so the
// caller owns the process's fate.
func runCollect(ctx context.Context, c *collectFlags, logger *slog.Logger) error {
	if c.dbPath == "" {
		return fmt.Errorf("-prices-db is required: collect mode writes an archive and " +
			"has nowhere to put one")
	}
	if c.tariff == "" {
		return fmt.Errorf("-tariff is required: the archive is keyed by tariff code, and " +
			"guessing one would file prices under a tariff this house is not on")
	}
	// Parsed here as well as inside the collector so a typo fails before the file is
	// created, rather than after.
	if _, err := octopus.ParseTariffCode(c.tariff); err != nil {
		return fmt.Errorf("-tariff %q: %w", c.tariff, err)
	}

	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		return fmt.Errorf("load Europe/London: %w", err)
	}

	store, err := prices.Open(c.dbPath)
	if err != nil {
		return fmt.Errorf("open archive at %s: %w", c.dbPath, err)
	}
	defer store.Close() //nolint:errcheck

	client, err := octopus.New(octopus.Options{BaseURL: c.baseURL, Logger: logger})
	if err != nil {
		return fmt.Errorf("octopus client: %w", err)
	}

	var vatAt func(time.Time) (float64, bool)
	if c.vat > 0 {
		rate := c.vat
		vatAt = func(time.Time) (float64, bool) { return rate, true }
	}

	col, err := collector.New(collector.Options{
		Fetcher:    client,
		Store:      store,
		Notifier:   notify.NewSlogNotifier(logger),
		Clock:      testutil.RealClock{},
		Location:   loc,
		TariffCode: c.tariff,
		VATRateAt:  vatAt,
		Logger:     logger,
	})
	if err != nil {
		return err
	}

	logger.Info("collect mode", "archive", c.dbPath, "tariff", c.tariff,
		"url", c.baseURL, "continuous", !c.once)

	if c.backTo != "" {
		from, err := time.ParseInLocation("2006-01-02", c.backTo, loc)
		if err != nil {
			return fmt.Errorf("-back-to %q: want YYYY-MM-DD: %w", c.backTo, err)
		}
		logger.Info("backfilling", "from", from.Format(time.RFC3339))
		res, err := col.Backfill(ctx, from, time.Now().In(loc))
		if err != nil {
			return fmt.Errorf("backfill: %w", err)
		}
		logSync(logger, "backfill complete", res)
		logRestatements(ctx, logger, store, c.tariff)
	}

	if c.once {
		res, err := col.Sync(ctx)
		if err != nil {
			return fmt.Errorf("sync: %w", err)
		}
		logSync(logger, "sync complete", res)
		logRestatements(ctx, logger, store, c.tariff)
		logStatus(logger, col)
		return nil
	}

	// Continuous. Run is fail-open internally: a supplier outage costs freshness
	// rather than the process, which is what makes this safe to leave running.
	col.Run(ctx)
	logStatus(logger, col)
	return nil
}

// logSync reports one sync's outcome at a glance.
func logSync(logger *slog.Logger, msg string, res collector.SyncResult) {
	logger.Info(msg,
		"inserted", res.Stored.Inserted,
		"unchanged", res.Stored.Unchanged,
		"restated", res.Stored.Restated,
		"rejected", len(res.Rejected),
		"warnings", len(res.Warnings),
	)
	// Rejections individually: each one is data we refused, and a run that refuses
	// everything must not look the same as a quiet one.
	for _, r := range res.Rejected {
		logger.Warn("slot rejected", "valid_from", r.Slot.ValidFrom.Format(time.RFC3339),
			"reason", r.Reason, "detail", r.Detail)
	}
	for _, w := range res.Warnings {
		logger.Warn("slot flagged", "valid_from", w.Slot.ValidFrom.Format(time.RFC3339),
			"kind", w.Kind, "detail", w.Detail)
	}
}

// logRestatements prints the archive's most recent restatements.
//
// This is the first reader Store.Restatements has ever had. The table was being
// written and never read, which made "has any price we billed ever changed?" — the
// question the data-model doc says the archive exists to answer later — answerable
// only by opening sqlite3. A collect run is the right place for it: somebody watching
// prices arrive is exactly the person who wants to know one of them moved.
func logRestatements(ctx context.Context, logger *slog.Logger, store prices.Store, tariff string) {
	rs, err := store.Restatements(ctx, tariff, 10)
	if err != nil {
		logger.Warn("could not read the restatement log", "error", err)
		return
	}
	if len(rs) == 0 {
		return
	}
	logger.Warn("prices have been restated since we first stored them", "count", len(rs))
	for _, r := range rs {
		logger.Warn("price restated",
			"valid_from", r.ValidFrom.Format(time.RFC3339),
			"was_inc_pence", r.OldIncVATPence,
			"now_inc_pence", r.NewIncVATPence,
			"first_seen", r.PreviousRetrievedAt.Format(time.RFC3339),
			"detected", r.DetectedAt.Format(time.RFC3339),
		)
	}
}

// logStatus prints what /healthz would show, so a collect run is self-describing.
func logStatus(logger *slog.Logger, col *collector.Collector) {
	st := col.Status()
	logger.Info("archive status",
		"tariff", st.TariffCode,
		"known_to", st.KnownTo,
		"complete_to", st.CompleteTo,
		"syncs", st.Syncs, "failures", st.Failures,
		"inserted", st.Inserted, "restated", st.Restated, "rejected", st.Rejected,
	)
}
