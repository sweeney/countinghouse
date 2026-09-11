package energy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/influx"
)

// ---------------------------------------------------------------------------
// Issue #32, the decoder half: a null _value is not a reading of zero.
//
// BuildPowerMeanSeriesFlux asks for createEmpty: true, so Influx returns a row
// for every bucket a device did not report in — carrying a null. Client.Query's
// type switch matched neither the float64 nor the string arm and appended the
// row anyway, leaving Value at 0.0, so "said nothing" and "drew nothing" were
// one number. Row.Null now separates them and demux drops the absent ones.
//
// The visible consequence is C13 staleness. A device that reported NOTHING used
// to arrive as a full slice of plausible zeroes, so it had an entry in
// powerByDevice and computeHouseStats could not see it was silent — the exact
// masking that flag exists to catch.
// ---------------------------------------------------------------------------

// nullRowQuerier answers the power query with a null row per bucket for the
// devices in nullIDs, and a real mean for everything else. The counter query
// gets per-bucket running totals so the house decomposition has a meter.
func nullRowQuerier(buckets []time.Time, meanW map[string]float64, counterPerBucket map[string]float64, nullIDs ...string) *influx.FakeQuerier {
	null := map[string]bool{}
	for _, id := range nullIDs {
		null[id] = true
	}
	return &influx.FakeQuerier{QueryFunc: func(flux string) ([]influx.Row, error) {
		var rows []influx.Row
		if strings.Contains(flux, `r._field == "energy_kwh"`) {
			for id, v := range counterPerBucket {
				if !strings.Contains(flux, `"`+id+`"`) {
					continue
				}
				for i := range buckets {
					rows = append(rows, influx.Row{DeviceID: id, Field: "energy_kwh",
						Time: buckets[i], Value: v * float64(i+1)})
				}
			}
			return rows, nil
		}
		for id, v := range meanW {
			if !strings.Contains(flux, `"`+id+`"`) {
				continue
			}
			for i := range buckets {
				r := influx.Row{DeviceID: id, Field: "power_w", Time: buckets[i]}
				if null[id] {
					r.Null = true // createEmpty filled a bucket the device never reported
				} else {
					r.Value = v
				}
				rows = append(rows, r)
			}
		}
		return rows, nil
	}}
}

// A device whose every power bucket is null has no telemetry at all, and C13 must
// say so. Before Row.Null the nulls decoded to 0.0, gave it a full powerByDevice
// entry, and the staleness check — which looks for an ABSENT entry — saw a device
// dutifully reporting zero watts all window.
func TestNullPowerBucketsMakeADeviceStaleRatherThanZero(t *testing.T) {
	loc := mustLondon(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	win := Window{Start: start, Stop: start.Add(3 * time.Hour), Label: WindowToday}
	iv, _ := lookupInterval("1h")
	buckets := BucketStarts(win, iv, loc)

	devices := map[string]config.DeviceConfig{
		"winefridge":        {Class: "continuous_power_device", DisplayName: "Wine Fridge"},
		"freezer":           {Class: "continuous_power_device", DisplayName: "Freezer"},
		"electricity_meter": {Class: EnergyMeterClass, DisplayName: "Meter"},
	}
	q := nullRowQuerier(buckets,
		map[string]float64{"winefridge": 52, "freezer": 0, "electricity_meter": 500},
		map[string]float64{"winefridge": 0.05, "freezer": 0.02, "electricity_meter": 0.5},
		"freezer", // silent: every power bucket is a null
	)

	resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv,
		GroupByHouse, false, false, devices, testTariff(), nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	if resp.StaleMonitoredCount == nil {
		t.Fatal("stale_monitored_count missing")
	}
	if *resp.StaleMonitoredCount != 1 {
		t.Errorf("stale_monitored_count = %d, want 1 — freezer reported nothing but nulls",
			*resp.StaleMonitoredCount)
	}
	if len(resp.StaleMonitoredIDs) != 1 || resp.StaleMonitoredIDs[0] != "freezer" {
		t.Errorf("stale_monitored_ids = %v, want [freezer]", resp.StaleMonitoredIDs)
	}
}

// The flag must stay quiet for a device that genuinely reported 0 W. "Idle" and
// "silent" are different facts and the null flag is what keeps them apart — a
// fix that simply dropped every zero-valued row would fail here.
func TestARealZeroReadingIsNotStale(t *testing.T) {
	loc := mustLondon(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	win := Window{Start: start, Stop: start.Add(3 * time.Hour), Label: WindowToday}
	iv, _ := lookupInterval("1h")
	buckets := BucketStarts(win, iv, loc)

	devices := map[string]config.DeviceConfig{
		"winefridge":        {Class: "continuous_power_device", DisplayName: "Wine Fridge"},
		"freezer":           {Class: "continuous_power_device", DisplayName: "Freezer"},
		"electricity_meter": {Class: EnergyMeterClass, DisplayName: "Meter"},
	}
	// freezer reports a real 0 W in every bucket — switched off at the wall, but
	// present and talking.
	q := nullRowQuerier(buckets,
		map[string]float64{"winefridge": 52, "freezer": 0, "electricity_meter": 500},
		map[string]float64{"winefridge": 0.05, "freezer": 0.02, "electricity_meter": 0.5},
	)

	resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv,
		GroupByHouse, false, false, devices, testTariff(), nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	if resp.StaleMonitoredCount == nil {
		t.Fatal("stale_monitored_count missing")
	}
	if *resp.StaleMonitoredCount != 0 {
		t.Errorf("stale_monitored_count = %d (%v), want 0 — a real 0 W reading is data",
			*resp.StaleMonitoredCount, resp.StaleMonitoredIDs)
	}
}

// A null bucket in the MIDDLE of an otherwise reporting device must not be folded
// as 0 W either. The bucket still reads 0 on the dense axis — that part needs a
// nullable avg_w to fix properly — but the device keeps its entry, so it is not
// mistaken for silent, and the zero comes from the axis rather than from a
// fabricated reading.
func TestASingleNullBucketDoesNotMakeADeviceStale(t *testing.T) {
	loc := mustLondon(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	win := Window{Start: start, Stop: start.Add(3 * time.Hour), Label: WindowToday}
	iv, _ := lookupInterval("1h")
	buckets := BucketStarts(win, iv, loc)

	devices := map[string]config.DeviceConfig{
		"winefridge": {Class: "continuous_power_device", DisplayName: "Wine Fridge"},
	}
	rows := []influx.Row{
		{DeviceID: "winefridge", Field: "power_w", Time: buckets[0], Value: 52},
		{DeviceID: "winefridge", Field: "power_w", Time: buckets[1], Null: true},
		{DeviceID: "winefridge", Field: "power_w", Time: buckets[2], Value: 60},
	}
	q := &influx.FakeQuerier{QueryFunc: func(flux string) ([]influx.Row, error) {
		if strings.Contains(flux, `r._field == "power_w"`) {
			return rows, nil
		}
		return nil, nil
	}}

	resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv,
		GroupByDevice, false, false, devices, testTariff(), nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	got := resp.Series[0].AvgW
	if got[0] != 52 || got[2] != 60 {
		t.Errorf("avg_w = %v, want the real readings in buckets 0 and 2", got)
	}
	if got[1] != 0 {
		t.Errorf("avg_w[1] = %v, want 0 — the axis is dense; the honest 'unknown' needs a nullable field", got[1])
	}
}
