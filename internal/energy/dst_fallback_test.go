package energy

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/influx"
)

// ---------------------------------------------------------------------------
// The autumn clock change in /series (issue #49).
//
// Europe/London, 2026-10-25: 02:00 BST → 01:00 GMT, so 01:00–02:00 local
// happens twice.
//
// Influx's location-aware aggregateWindow lays SUB-DAY windows on LOCAL wall
// clock. The window that opens at 01:30 BST therefore runs to the next wall
// time that only happens once, 02:00 GMT: 90 minutes, swallowing the repeated
// hour. Nothing is stamped at 01:00Z or 01:30Z. The Go axis (BucketStarts)
// steps a fixed 30m in absolute time and has buckets there, so the repeated
// hour's energy landed in the 00:30Z bucket and the two buckets after it read
// zero. That was observed in prod on the night; the fixture's flux_rows are
// those rows, reconstructed for a synthetic steady 400 W load.
//
// The fix stops asking Influx for local sub-day windows at all: sub-day
// queries are windowed on the UTC grid, which for a whole-hour zone is the
// same grid at ≤1h and has no repeated or missing hour to mishandle. 6h is
// built in Go from a 1h query. Only 1d keeps location:, where wall-clock
// stretching (a 25h day) is exactly what we want.
// ---------------------------------------------------------------------------

// dstFixture is testdata/dst_fallback_2026-10-25.json; see
// testdata/gen_dst_fallback.py for how it is made.
type dstFixture struct {
	Window   struct{ From, To time.Time }
	Interval string
	Watts    float64
	Readings []struct {
		Time      time.Time `json:"time"`
		EnergyKWh float64   `json:"energy_kwh"`
	} `json:"readings"`
	FluxRows []struct {
		DeviceID string    `json:"device_id"`
		Field    string    `json:"field"`
		Time     time.Time `json:"time"`
		Value    float64   `json:"value"`
	} `json:"flux_rows"`
}

func loadDSTFixture(t *testing.T) dstFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/dst_fallback_2026-10-25.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx dstFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

// fallBackQuerier answers the way prod Influx does for whichever query it is
// handed, so the test can tell the fixed query from the broken one:
//
//   - a sub-day aggregateWindow with a NAMED location gets the stretched rows
//     prod actually returned that night (the fixture's flux_rows);
//   - anything else is answered from the fixture's raw readings by CounterSim,
//     which models the UTC grid (and the calendar-day grid for 1d) exactly.
//
// The issue's original test fed the stretched rows to every query, which
// proves the bug but could never go green: the fix CHANGES the query, so the
// double has to notice what it was asked.
func fallBackQuerier(fx dstFixture) *influx.FakeQuerier {
	var stretched []influx.Row
	for _, r := range fx.FluxRows {
		stretched = append(stretched, influx.Row{DeviceID: r.DeviceID, Field: r.Field, Time: r.Time, Value: r.Value})
	}
	at := make([]time.Time, len(fx.Readings))
	kwh := make([]float64, len(fx.Readings))
	for i, r := range fx.Readings {
		at[i], kwh[i] = r.Time, r.EnergyKWh
	}
	sim := influx.NewCounterSim().AddSamples("steady_plug", at, kwh)

	return &influx.FakeQuerier{QueryFunc: func(flux string) ([]influx.Row, error) {
		if strings.Contains(flux, `"energy_kwh"`) && strings.Contains(flux, "aggregateWindow") &&
			strings.Contains(flux, "timezone.location(name:") && !strings.Contains(flux, "every: 1d,") {
			return stretched, nil
		}
		return sim.Answer(flux)
	}}
}

// TestSeriesDSTFallBackRepeatedHour is the issue's reproduction: a steady load
// across the repeated hour must fill every half hour equally, not dump three
// half hours into one bucket and leave two at zero.
func TestSeriesDSTFallBackRepeatedHour(t *testing.T) {
	fx := loadDSTFixture(t)
	q := fallBackQuerier(fx)

	iv, ok := lookupInterval(fx.Interval)
	if !ok {
		t.Fatalf("unknown interval %q", fx.Interval)
	}
	win := Window{Start: fx.Window.From, Stop: fx.Window.To, Label: WindowCustom}
	devices := map[string]config.DeviceConfig{"steady_plug": {Class: "continuous_power_device"}}

	resp, err := BuildSeries(context.Background(), q, "b", win, iv, GroupByDevice,
		false, false, devices, testPricer(), nil, mustLondon(t))
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	if len(resp.Series) != 1 {
		t.Fatalf("series = %d, want 1", len(resp.Series))
	}
	s := resp.Series[0]

	// A steady load fills every half hour equally. Bucket 0 is a minute short
	// (increase() re-bases at the first reading), so allow a minute's energy.
	want := fx.Watts * 0.5 / 1000
	slack := fx.Watts / 60 / 1000
	if len(s.KWh) != 8 {
		t.Fatalf("buckets = %d, want 8 (4h of 30m): %v", len(s.KWh), resp.Buckets)
	}
	for i, got := range s.KWh {
		if math.Abs(got-want) > slack+0.0005 {
			t.Errorf("bucket %d (%s): %.3f kWh, want %.3f",
				i, resp.Buckets[i].Format(time.RFC3339), got, want)
		}
	}
	// Re-arranging energy between buckets must never change how much there is.
	wantTotal := fx.Readings[len(fx.Readings)-2].EnergyKWh - fx.Readings[0].EnergyKWh // range() stop is exclusive
	if math.Abs(s.TotalKWh-wantTotal) > 0.001 {
		t.Errorf("total = %.4f kWh, want %.4f", s.TotalKWh, wantTotal)
	}
}

// The cost side of the same bug. /series and /bill (counter_slot) price each
// half hour at its own Octopus slot, and that night the prices moved fast. The
// stretched 01:30 BST window put the repeated hour's 0.4 kWh into the 01:30 BST
// bucket, so it was all priced at −6.28p instead of at the two GMT slots it
// was actually drawn in: over-crediting about 1p on the house meter that night,
// every October. The slot prices here are the real ones from the issue for the
// three slots that matter, and a flat 10p elsewhere.
func TestSeriesDSTFallBackPricesEachSlotAtItsOwnRate(t *testing.T) {
	fx := loadDSTFixture(t)
	q := fallBackQuerier(fx)

	// One slot per half hour from 00:00 BST (23:00Z) to 03:00 GMT.
	pence := []float64{10, 10, 10,
		-6.28, // 01:30 BST (00:30Z)
		-4.14, // 01:00 GMT (01:00Z) — the first half of the repeated hour
		-3.26, // 01:30 GMT (01:30Z) — the second half
		10, 10}
	rates := make([]*float64, len(pence))
	for i := range pence {
		rates[i] = fp(pence[i])
	}
	pricer := slotRates{start: fx.Window.From, rates: rates}

	iv, _ := lookupInterval(fx.Interval)
	win := Window{Start: fx.Window.From, Stop: fx.Window.To, Label: WindowCustom}
	devices := map[string]config.DeviceConfig{"steady_plug": {Class: "continuous_power_device"}}
	resp, err := BuildSeries(context.Background(), q, "b", win, iv, GroupByDevice,
		false, false, devices, pricer, nil, mustLondon(t))
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	if len(resp.Series) != 1 || len(resp.Series[0].Cost) != len(pence) {
		t.Fatalf("want 1 series of %d buckets, got %+v", len(pence), resp.Series)
	}
	s := resp.Series[0]

	// Each bucket costs its own energy at its own slot's rate.
	var wantTotal float64
	for i, p := range pence {
		want := s.KWh[i] * p / 100
		wantTotal += want
		if math.Abs(s.Cost[i]-want) > 0.0001 {
			t.Errorf("bucket %d (%s): cost £%.4f, want £%.4f (%.3f kWh at %.2fp)",
				i, resp.Buckets[i].Format(time.RFC3339), s.Cost[i], want, s.KWh[i], p)
		}
	}
	// The repeated hour is billed at −4.14p and −3.26p, not all at −6.28p.
	half := fx.Watts * 0.5 / 1000
	if want := half * (-6.28 - 4.14 - 3.26) / 100; math.Abs(s.Cost[3]+s.Cost[4]+s.Cost[5]-want) > 0.0002 {
		t.Errorf("01:30 BST..02:00 GMT costs £%.4f, want £%.4f", s.Cost[3]+s.Cost[4]+s.Cost[5], want)
	}
	if math.Abs(s.TotalCost-wantTotal) > 0.0005 {
		t.Errorf("total cost £%.4f, want £%.4f", s.TotalCost, wantTotal)
	}
}
