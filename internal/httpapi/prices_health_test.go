package httpapi

import (
	"net/http"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The /healthz and /metrics price surfaces.
//
// "Do we have prices, and how far do they run?" must be answerable without
// running a query — that is the whole point of reporting it here. The two fields
// that matter are deliberately different:
//
//	known_to     the end of the newest slot held        "prices are arriving"
//	complete_to  the end of the newest FULL local day   "we can bill this far"
//
// A publication can advance known_to across a whole day while leaving that day
// short of slots, so a monitor reading known_to alone would be told yes when the
// answer is no.
// ---------------------------------------------------------------------------

// fakePrices is a PricesProvider returning a fixed snapshot.
type fakePrices struct{ health []PriceHealth }

func (f fakePrices) PriceHealth() []PriceHealth { return f.health }

const healthTariff = "E-1R-AGILE-24-10-01-A"

// now in these tests is the Server's injected clock instant, so staleness is
// deterministic rather than dependent on when the suite runs.
func healthNow(t *testing.T) time.Time {
	t.Helper()
	return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
}

func TestHealthzOmitsPricesWhenNoCollectorRuns(t *testing.T) {
	s, _ := dataSetup(t)
	// No Prices provider: a flat-rate deployment runs no collector, and an empty
	// or zeroed block would suggest a broken archive rather than no archive.
	w := doGET(t, s, "/healthz")
	m := decode(t, w)
	if _, present := m["prices"]; present {
		t.Errorf("prices block present with no collector configured: %v", m["prices"])
	}
}

func TestHealthzReportsPriceHorizons(t *testing.T) {
	now := healthNow(t)
	s, _ := dataSetup(t)
	s.Clock = fixedClock{now}
	// Tomorrow is complete, which is the healthy steady state after a publication.
	s.Prices = fakePrices{health: []PriceHealth{{
		TariffCode:  healthTariff,
		KnownTo:     now.Add(35 * time.Hour),
		CompleteTo:  now.Add(35 * time.Hour),
		LastAttempt: now.Add(-5 * time.Minute),
		LastSuccess: now.Add(-5 * time.Minute),
		Syncs:       12, Inserted: 48,
	}}}

	w := doGET(t, s, "/healthz")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	m := decode(t, w)

	block, ok := m["prices"].([]any)
	if !ok {
		t.Fatalf("prices is not an array: %v", m["prices"])
	}
	if len(block) != 1 {
		t.Fatalf("got %d price entries, want 1", len(block))
	}
	entry := block[0].(map[string]any)
	for _, field := range []string{"tariff_code", "known_to", "complete_to", "last_success"} {
		if _, present := entry[field]; !present {
			t.Errorf("entry missing %q: %v", field, entry)
		}
	}
	if entry["tariff_code"] != healthTariff {
		t.Errorf("tariff_code = %v", entry["tariff_code"])
	}
	if m["status"] != "ok" {
		t.Errorf("status = %v, want ok — a complete archive is healthy", m["status"])
	}
}

// The aggregate verdict has to move, or a monitor watching only the top-level
// status (the obvious thing to alert on) learns nothing.
func TestHealthzStatusReflectsPriceProblems(t *testing.T) {
	now := healthNow(t)

	for _, tc := range []struct {
		name       string
		health     PriceHealth
		wantStatus string
	}{
		{
			name: "complete through tomorrow is healthy",
			health: PriceHealth{
				TariffCode: healthTariff,
				KnownTo:    now.Add(35 * time.Hour), CompleteTo: now.Add(35 * time.Hour),
				LastSuccess: now.Add(-time.Minute),
			},
			wantStatus: "ok",
		},
		{
			// A fetch failure degrades but does not take the service down: the
			// archive still holds everything it held before, so past windows still
			// price. Same treatment as a failing config namespace.
			name: "a fetch error degrades",
			health: PriceHealth{
				TariffCode: healthTariff,
				KnownTo:    now.Add(35 * time.Hour), CompleteTo: now.Add(35 * time.Hour),
				LastSuccess: now.Add(-2 * time.Hour), LastError: "503 from the supplier",
			},
			wantStatus: "degraded",
		},
		{
			// complete_to in the PAST means today cannot be priced in full. This is
			// the condition worth alerting on, and it cannot flap: in the healthy
			// state complete_to is the end of today or tomorrow, always ahead of now.
			name: "complete_to behind now degrades",
			health: PriceHealth{
				TariffCode: healthTariff,
				KnownTo:    now.Add(-2 * time.Hour), CompleteTo: now.Add(-14 * time.Hour),
				LastSuccess: now.Add(-time.Minute),
			},
			wantStatus: "degraded",
		},
		{
			// An archive that has never been filled. Legitimate for about a minute
			// at first boot, and a problem after that — it must not read as healthy.
			name: "an empty archive degrades",
			health: PriceHealth{
				TariffCode: healthTariff, LastSuccess: now.Add(-time.Minute),
			},
			wantStatus: "degraded",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := dataSetup(t)
			s.Clock = fixedClock{now}
			s.Prices = fakePrices{health: []PriceHealth{tc.health}}

			m := decode(t, doGET(t, s, "/healthz"))
			if m["status"] != tc.wantStatus {
				t.Errorf("status = %v, want %v", m["status"], tc.wantStatus)
			}
			// Degraded is not unavailable: the data routes still work.
			if doGET(t, s, "/healthz").Code != http.StatusOK {
				t.Error("/healthz should still answer 200 when degraded")
			}
		})
	}
}

// An unreachable Influx outranks a price problem. Without Influx no data route can
// answer at all, so it must not be downgraded to "degraded" by a healthy archive
// or masked by a broken one.
func TestHealthzInfluxOutranksPrices(t *testing.T) {
	now := healthNow(t)
	s, q := dataSetup(t)
	s.Clock = fixedClock{now}
	q.PingOK = false
	s.Prices = fakePrices{health: []PriceHealth{{
		TariffCode: healthTariff,
		KnownTo:    now.Add(35 * time.Hour), CompleteTo: now.Add(35 * time.Hour),
	}}}

	m := decode(t, doGET(t, s, "/healthz"))
	if m["status"] != "unavailable" {
		t.Errorf("status = %v, want unavailable", m["status"])
	}
}

// /metrics carries the counters, so the archive's behaviour is graphable over time
// rather than only inspectable at this instant.
func TestMetricsReportsPriceCounters(t *testing.T) {
	now := healthNow(t)
	s, _ := dataSetup(t)
	s.Clock = fixedClock{now}
	s.Prices = fakePrices{health: []PriceHealth{{
		TariffCode: healthTariff,
		KnownTo:    now.Add(35 * time.Hour), CompleteTo: now.Add(35 * time.Hour),
		Syncs: 288, Failures: 3, Inserted: 1488, Restated: 1, Rejected: 2, Warnings: 5,
	}}}

	m := decode(t, doGET(t, s, "/metrics"))
	block, ok := m["prices"].([]any)
	if !ok {
		t.Fatalf("metrics has no prices array: %v", m)
	}
	entry := block[0].(map[string]any)
	for field, want := range map[string]float64{
		"syncs": 288, "failures": 3, "inserted": 1488,
		"restated": 1, "rejected": 2, "warnings": 5,
	} {
		got, present := entry[field]
		if !present {
			t.Errorf("missing counter %q", field)
			continue
		}
		if got.(float64) != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}
}

func TestMetricsOmitsPricesWhenNoCollectorRuns(t *testing.T) {
	s, _ := dataSetup(t)
	m := decode(t, doGET(t, s, "/metrics"))
	if _, present := m["prices"]; present {
		t.Errorf("prices present in /metrics with no collector: %v", m["prices"])
	}
}

// fixedClock pins the Server's notion of now.
type fixedClock struct{ t time.Time }

func (f fixedClock) Now() time.Time { return f.t }
