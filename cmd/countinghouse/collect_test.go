package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Collect-only mode.
//
// The mode exists so the archive can start filling on any machine with nothing but a
// tariff code and a path — no credentials, no Influx, no remote config — because
// several open questions on this work are measurements waiting on weeks of real
// prices rather than decisions waiting on thought.
//
// Which makes its failure modes matter more than usual: it will be left running
// unattended, so every way it can decline to do its job has to be loud at the moment
// it is started rather than discovered later from an archive that never grew.
// ---------------------------------------------------------------------------

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// countSlots opens a database file read-only and counts archived slots.
//
// Lived in snapshot_test.go until the local snapshot was deleted in favour of the
// library's; kept here because the collect-mode tests still need to see what landed.
func countSlots(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer db.Close() //nolint:errcheck
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM unit_price").Scan(&n); err != nil {
		t.Fatalf("count in %s: %v", path, err)
	}
	return n
}

// fakeOctopus serves a recorded unit-rates page, so a collect run can be exercised
// end to end without touching the live API.
func fakeOctopus(t *testing.T) *httptest.Server {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "octopus", "testdata", "unit_rates_mixed_sign_day.json"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	// The recorded page carries a null `next`, so one page is the whole answer.
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

// collectTo builds flags pointing at a fresh archive in a temp dir.
func collectTo(t *testing.T, srv *httptest.Server) *collectFlags {
	t.Helper()
	return &collectFlags{
		dbPath:  filepath.Join(t.TempDir(), "prices.db"),
		tariff:  "E-1R-AGILE-24-10-01-A",
		baseURL: srv.URL,
		once:    true,
	}
}

// Every required flag, refused by name. An unattended collector that starts and then
// quietly has nowhere to write is the failure this prevents.
func TestCollectRefusesIncompleteFlags(t *testing.T) {
	srv := fakeOctopus(t)
	defer srv.Close()

	tests := []struct {
		name   string
		mutate func(*collectFlags)
		want   string
	}{
		{"no archive path", func(c *collectFlags) { c.dbPath = "" }, "prices-db"},
		{"no tariff code", func(c *collectFlags) { c.tariff = "" }, "tariff"},
		// Parsed before the archive file is created, so a typo fails cleanly rather
		// than leaving an empty database behind.
		{"an unparseable tariff code", func(c *collectFlags) { c.tariff = "AGILE" }, "tariff"},
		{"a tariff code with a traversal attempt", func(c *collectFlags) { c.tariff = "E-1R-../../x-A" }, "tariff"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := collectTo(t, srv)
			tc.mutate(c)
			err := runCollect(context.Background(), c, quietLogger())
			if err == nil {
				t.Fatalf("accepted flags missing %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name %q: %v", tc.want, err)
			}
		})
	}
}

// A bad tariff code must not leave a database behind. An empty archive file is worse
// than none: the next run opens it happily and the operator sees a file and assumes
// prices are arriving.
func TestCollectDoesNotCreateAnArchiveForABadTariff(t *testing.T) {
	srv := fakeOctopus(t)
	defer srv.Close()

	c := collectTo(t, srv)
	c.tariff = "nonsense"
	if err := runCollect(context.Background(), c, quietLogger()); err == nil {
		t.Fatal("expected a refusal")
	}
	if _, err := os.Stat(c.dbPath); err == nil {
		t.Error("an archive file was created despite the tariff code being rejected")
	}
}

// The happy path: one sync against a served fixture lands real slots in a real
// archive, through the same store, gates and migrations production uses.
func TestCollectOnceStoresSlots(t *testing.T) {
	srv := fakeOctopus(t)
	defer srv.Close()

	c := collectTo(t, srv)
	if err := runCollect(context.Background(), c, quietLogger()); err != nil {
		t.Fatalf("runCollect: %v", err)
	}

	if n := countSlots(t, c.dbPath); n != 48 {
		t.Errorf("archive holds %d slots, want the fixture's 48", n)
	}
	// Created by the real store, so the mode cannot drift from production's file
	// permissions either.
	info, err := os.Stat(c.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("archive mode = %04o, want 0600", perm)
	}
}

// Running twice must be a no-op, not a duplicate. This is the property that makes it
// safe to leave on a timer and safe to re-run by hand.
func TestCollectIsIdempotent(t *testing.T) {
	srv := fakeOctopus(t)
	defer srv.Close()

	c := collectTo(t, srv)
	if err := runCollect(context.Background(), c, quietLogger()); err != nil {
		t.Fatal(err)
	}
	first := countSlots(t, c.dbPath)
	if err := runCollect(context.Background(), c, quietLogger()); err != nil {
		t.Fatal(err)
	}
	if second := countSlots(t, c.dbPath); second != first {
		t.Errorf("slot count went %d -> %d across two runs; the archive must be idempotent",
			first, second)
	}
}

// -back-to takes a plain date. A malformed one is refused before any fetching, with
// the expected format in the message.
func TestCollectBackToDateValidation(t *testing.T) {
	srv := fakeOctopus(t)
	defer srv.Close()

	c := collectTo(t, srv)
	c.backTo = "13/09/2026"
	err := runCollect(context.Background(), c, quietLogger())
	if err == nil {
		t.Fatal("a malformed -back-to was accepted")
	}
	if !strings.Contains(err.Error(), "YYYY-MM-DD") {
		t.Errorf("error does not state the expected format: %v", err)
	}
}

// A supplier that is down must fail cleanly, not leave a half-written archive or
// panic. Collect mode is the thing most likely to meet a cold API, since it is what
// somebody runs by hand.
func TestCollectSurvivesAnUnreachableSupplier(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := collectTo(t, srv)
	if err := runCollect(context.Background(), c, quietLogger()); err == nil {
		t.Error("a 503 from the supplier was reported as success")
	}
	// The archive exists (it was opened) but holds nothing, which is honest.
	if n := countSlots(t, c.dbPath); n != 0 {
		t.Errorf("archive holds %d slots after a failed fetch, want 0", n)
	}
}

// The VAT flag is optional, and omitting it means "do not check" rather than "check
// against 0%" — the distinction that, when conflated, rejected whole backfills.
func TestCollectWithoutAVATRateStillStores(t *testing.T) {
	srv := fakeOctopus(t)
	defer srv.Close()

	c := collectTo(t, srv)
	c.vat = 0 // unset
	if err := runCollect(context.Background(), c, quietLogger()); err != nil {
		t.Fatalf("runCollect: %v", err)
	}
	if n := countSlots(t, c.dbPath); n != 48 {
		t.Errorf("archive holds %d slots with no VAT rate given, want 48 — an unset rate "+
			"must express no opinion, not an opinion of zero", n)
	}
}
