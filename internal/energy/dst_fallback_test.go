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
	sim := influx.NewCounterSim(time.UTC).AddSamples("steady_plug", at, kwh)

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
