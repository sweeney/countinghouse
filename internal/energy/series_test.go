package energy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/influx"
)

// ---- BucketStarts -----------------------------------------------------------

func TestBucketStartsTodayHourly(t *testing.T) {
	loc := mustLondon(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 12, 0, 0, 0, 0, loc) // full 24h day
	win := Window{Start: start, Stop: stop, Label: WindowToday}
	iv, _ := lookupInterval("1h")

	got := BucketStarts(win, iv, loc)
	if len(got) != 24 {
		t.Fatalf("hourly buckets = %d, want 24", len(got))
	}
	if !got[0].Equal(start) {
		t.Errorf("first bucket = %v, want %v", got[0], start)
	}
	// Each is one hour apart, on local boundaries.
	for i := 1; i < len(got); i++ {
		if got[i].Sub(got[i-1]) != time.Hour {
			t.Errorf("bucket %d gap = %v, want 1h", i, got[i].Sub(got[i-1]))
		}
	}
	if h, _, _ := got[5].Clock(); h != 5 {
		t.Errorf("bucket 5 local hour = %d, want 5", h)
	}
}

func TestBucketStartsWeekDaily(t *testing.T) {
	loc := mustLondon(t)
	// Monday → Monday, 7 calendar days.
	start := time.Date(2026, 6, 8, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 15, 0, 0, 0, 0, loc)
	win := Window{Start: start, Stop: stop, Label: WindowWeek}
	iv, _ := lookupInterval("1d")

	got := BucketStarts(win, iv, loc)
	if len(got) != 7 {
		t.Fatalf("weekly buckets = %d, want 7", len(got))
	}
	for i, b := range got {
		if h, m, s := b.Clock(); h != 0 || m != 0 || s != 0 {
			t.Errorf("bucket %d not at local midnight: %v", i, b)
		}
	}
}

func TestBucketStartsMonthDaily(t *testing.T) {
	loc := mustLondon(t)
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 14, 30, 0, 0, loc) // period-to-date
	win := Window{Start: start, Stop: stop, Label: WindowMonth}
	iv, _ := lookupInterval("1d")

	got := BucketStarts(win, iv, loc)
	// Days 1..11 each start before stop → 11 buckets (the 11th is partial).
	if len(got) != 11 {
		t.Fatalf("month-to-date buckets = %d, want 11", len(got))
	}
	if got[0].Day() != 1 || got[10].Day() != 11 {
		t.Errorf("first/last bucket days = %d/%d", got[0].Day(), got[10].Day())
	}
}

// DST: London clocks go BACK on 2025-10-26 (25-hour day) and FORWARD on
// 2026-03-29 (23-hour day). Calendar stepping must keep one bucket per local
// day regardless.
func TestBucketStartsDSTAutumnBack(t *testing.T) {
	loc := mustLondon(t)
	// Span the autumn-back changeover: 25-26 Oct 2025.
	start := time.Date(2025, 10, 25, 0, 0, 0, 0, loc)
	stop := time.Date(2025, 10, 28, 0, 0, 0, 0, loc)
	win := Window{Start: start, Stop: stop, Label: WindowCustom}
	iv, _ := lookupInterval("1d")

	got := BucketStarts(win, iv, loc)
	if len(got) != 3 {
		t.Fatalf("autumn-back buckets = %d, want 3 (25,26,27 Oct)", len(got))
	}
	// The 26 Oct bucket is a 25-hour day.
	day26 := got[1]
	if day26.Day() != 26 {
		t.Fatalf("second bucket day = %d, want 26", day26.Day())
	}
	hrs := got[2].Sub(got[1]).Hours()
	if hrs != 25 {
		t.Errorf("26 Oct bucket length = %vh, want 25h (autumn back)", hrs)
	}
	// Every bucket starts at local midnight.
	for i, b := range got {
		if h, m, s := b.Clock(); h != 0 || m != 0 || s != 0 {
			t.Errorf("bucket %d not local midnight: %v", i, b)
		}
	}
}

func TestBucketStartsDSTSpringForward(t *testing.T) {
	loc := mustLondon(t)
	// Span the spring-forward changeover: 29 Mar 2026 is a 23-hour day.
	start := time.Date(2026, 3, 28, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 3, 31, 0, 0, 0, 0, loc)
	win := Window{Start: start, Stop: stop, Label: WindowCustom}
	iv, _ := lookupInterval("1d")

	got := BucketStarts(win, iv, loc)
	if len(got) != 3 {
		t.Fatalf("spring-forward buckets = %d, want 3", len(got))
	}
	if got[1].Day() != 29 {
		t.Fatalf("second bucket day = %d, want 29", got[1].Day())
	}
	if hrs := got[2].Sub(got[1]).Hours(); hrs != 23 {
		t.Errorf("29 Mar bucket length = %vh, want 23h (spring forward)", hrs)
	}
}

// ---- AssembleSeries: device grouping ---------------------------------------

func testTariff() config.Tariff {
	return config.Tariff{UnitRate: 0.2089, VATRate: 0.05}
}

func threeBuckets(loc *time.Location) []time.Time {
	return []time.Time{
		time.Date(2026, 6, 11, 0, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 1, 0, 0, 0, loc),
		time.Date(2026, 6, 11, 2, 0, 0, 0, loc),
	}
}

func TestAssembleByDeviceZeroFillAndCost(t *testing.T) {
	loc := mustLondon(t)
	buckets := threeBuckets(loc)
	devices := map[string]config.DeviceConfig{
		"winefridge": {Class: "continuous_power_device", DisplayName: "Wine Fridge", Location: "kitchen"},
	}
	// Bucket 1 is idle (zero) — must be zero-filled.
	energy := map[string][]float64{"winefridge": {0.05, 0, 0.04}}
	power := map[string][]float64{"winefridge": {52.1, 0, 41.8}}

	out := AssembleSeries(buckets, devices, energy, power, testTariff(), GroupByDevice)
	if len(out) != 1 {
		t.Fatalf("series count = %d, want 1", len(out))
	}
	s := out[0]
	if s.Key != "winefridge" || s.Label != "Wine Fridge" || s.Location != "kitchen" || s.Class != "continuous_power_device" {
		t.Errorf("series metadata wrong: %+v", s)
	}
	if len(s.KWh) != 3 || len(s.Cost) != 3 || len(s.AvgW) != 3 {
		t.Fatalf("arrays not aligned to 3 buckets: %+v", s)
	}
	// Zero-fill of the idle bucket.
	if s.KWh[1] != 0 || s.Cost[1] != 0 || s.AvgW[1] != 0 {
		t.Errorf("idle bucket not zero: kwh=%v cost=%v w=%v", s.KWh[1], s.Cost[1], s.AvgW[1])
	}
	// cost = kwh × rate × (1+vat). 0.05 × 0.2089 × 1.05 = 0.01096725 → 0.011.
	if s.Cost[0] != 0.011 {
		t.Errorf("cost[0] = %v, want 0.011", s.Cost[0])
	}
	// Rounding: kWh 3dp, W 1dp.
	if s.KWh[0] != 0.05 || s.AvgW[0] != 52.1 {
		t.Errorf("rounding wrong: kwh=%v w=%v", s.KWh[0], s.AvgW[0])
	}
	// Totals.
	if s.TotalKWh != 0.09 {
		t.Errorf("total kwh = %v, want 0.09", s.TotalKWh)
	}
	wantTotalCost := roundTo(0.011+0+roundTo(0.04*0.2089*1.05, 4), 4)
	if s.TotalCost != wantTotalCost {
		t.Errorf("total cost = %v, want %v", s.TotalCost, wantTotalCost)
	}
}

func TestAssembleByDeviceExcludesMeter(t *testing.T) {
	loc := mustLondon(t)
	buckets := threeBuckets(loc)
	devices := map[string]config.DeviceConfig{
		"winefridge":        {Class: "continuous_power_device", DisplayName: "Wine Fridge"},
		"electricity_meter": {Class: energyMeterClass, DisplayName: "Meter"},
	}
	energy := map[string][]float64{
		"winefridge":        {0.1, 0.1, 0.1},
		"electricity_meter": {5, 5, 5},
	}
	out := AssembleSeries(buckets, devices, energy, nil, testTariff(), GroupByDevice)
	if len(out) != 1 || out[0].Key != "winefridge" {
		t.Fatalf("meter not excluded from device grouping: %+v", out)
	}
}

func TestAssembleByDeviceFallbackLabel(t *testing.T) {
	loc := mustLondon(t)
	buckets := threeBuckets(loc)
	devices := map[string]config.DeviceConfig{
		"toaster": {Class: "short_burst_power_device"}, // no DisplayName
	}
	out := AssembleSeries(buckets, devices, nil, nil, testTariff(), GroupByDevice)
	if len(out) != 1 || out[0].Label != "toaster" {
		t.Errorf("label fallback wrong: %+v", out)
	}
	// Fully zero-filled.
	if out[0].TotalKWh != 0 {
		t.Errorf("missing device should be zero: %+v", out[0])
	}
}

// ---- AssembleSeries: location grouping -------------------------------------

func TestAssembleByLocationSumsKitchen(t *testing.T) {
	loc := mustLondon(t)
	buckets := threeBuckets(loc)
	devices := map[string]config.DeviceConfig{
		"winefridge": {Class: "continuous_power_device", Location: "kitchen"},
		"toaster":    {Class: "short_burst_power_device", Location: "kitchen"},
		"office_pc":  {Class: "media_power_device", Location: "office"},
	}
	energy := map[string][]float64{
		"winefridge": {0.05, 0.05, 0.05},
		"toaster":    {0.10, 0.00, 0.20},
		"office_pc":  {0.30, 0.30, 0.30},
	}
	power := map[string][]float64{
		"winefridge": {50, 50, 50},
		"toaster":    {100, 0, 200},
		"office_pc":  {300, 300, 300},
	}
	out := AssembleSeries(buckets, devices, energy, power, testTariff(), GroupByLocation)
	if len(out) != 2 {
		t.Fatalf("location series = %d, want 2 (kitchen, office)", len(out))
	}
	// Sorted: kitchen, office.
	kitchen := out[0]
	if kitchen.Key != "kitchen" {
		t.Fatalf("first key = %q, want kitchen", kitchen.Key)
	}
	// Kitchen bucket 0 kWh = 0.05 + 0.10 = 0.15; bucket-wise sum.
	if kitchen.KWh[0] != 0.15 || kitchen.KWh[1] != 0.05 || kitchen.KWh[2] != 0.25 {
		t.Errorf("kitchen kwh = %v, want [0.15 0.05 0.25]", kitchen.KWh)
	}
	// avg_w summed (power additive): 150, 50, 250.
	if kitchen.AvgW[0] != 150 || kitchen.AvgW[1] != 50 || kitchen.AvgW[2] != 250 {
		t.Errorf("kitchen avg_w = %v, want [150 50 250]", kitchen.AvgW)
	}
}

func TestAssembleByLocationExcludesMeter(t *testing.T) {
	loc := mustLondon(t)
	buckets := threeBuckets(loc)
	devices := map[string]config.DeviceConfig{
		"winefridge":        {Class: "continuous_power_device", Location: "kitchen"},
		"electricity_meter": {Class: energyMeterClass, Location: "utility"},
	}
	energy := map[string][]float64{
		"winefridge":        {1, 1, 1},
		"electricity_meter": {9, 9, 9},
	}
	out := AssembleSeries(buckets, devices, energy, nil, testTariff(), GroupByLocation)
	if len(out) != 1 || out[0].Key != "kitchen" {
		t.Errorf("meter location leaked into grouping: %+v", out)
	}
}

// ---- AssembleSeries: class grouping ----------------------------------------

func TestAssembleByClass(t *testing.T) {
	loc := mustLondon(t)
	buckets := threeBuckets(loc)
	devices := map[string]config.DeviceConfig{
		"a": {Class: "continuous_power_device"},
		"b": {Class: "continuous_power_device"},
		"c": {Class: "media_power_device"},
	}
	energy := map[string][]float64{
		"a": {0.1, 0.1, 0.1},
		"b": {0.2, 0.2, 0.2},
		"c": {0.5, 0.5, 0.5},
	}
	out := AssembleSeries(buckets, devices, energy, nil, testTariff(), GroupByClass)
	if len(out) != 2 {
		t.Fatalf("class series = %d, want 2", len(out))
	}
	// Sorted: continuous_power_device, media_power_device.
	if out[0].Key != "continuous_power_device" {
		t.Fatalf("first class key = %q", out[0].Key)
	}
	if out[0].KWh[0] != 0.3 {
		t.Errorf("continuous class bucket0 kwh = %v, want 0.3", out[0].KWh[0])
	}
}

// ---- AssembleSeries: house dual-series -------------------------------------

func TestAssembleHouseDualSeries(t *testing.T) {
	loc := mustLondon(t)
	buckets := threeBuckets(loc)
	devices := map[string]config.DeviceConfig{
		"winefridge":        {Class: "continuous_power_device", Location: "kitchen"},
		"office_pc":         {Class: "media_power_device", Location: "office"},
		"network-ups":       {Class: "ups_sensor", Location: "office"},
		"electricity_meter": {Class: energyMeterClass, Location: "utility"},
	}
	energy := map[string][]float64{
		"winefridge":        {0.1, 0.1, 0.1},
		"office_pc":         {0.2, 0.2, 0.2},
		"network-ups":       {0.05, 0.05, 0.05},
		"electricity_meter": {1.0, 1.0, 1.0},
	}
	power := map[string][]float64{
		"winefridge":        {100, 100, 100},
		"office_pc":         {200, 200, 200},
		"network-ups":       {50, 50, 50},
		"electricity_meter": {1000, 1000, 1000},
	}
	out := AssembleSeries(buckets, devices, energy, power, testTariff(), GroupByHouse)
	if len(out) != 2 {
		t.Fatalf("house series = %d, want 2 (monitored, meter)", len(out))
	}
	mon, meter := out[0], out[1]
	if mon.Key != houseMonitoredKey || meter.Key != houseMeterKey {
		t.Fatalf("house keys = %q,%q", mon.Key, meter.Key)
	}
	// monitored = sum of ALL non-meter devices incl. UPS: 0.1+0.2+0.05 = 0.35.
	if mon.KWh[0] != 0.35 {
		t.Errorf("monitored kwh[0] = %v, want 0.35", mon.KWh[0])
	}
	// monitored avg_w summed: 100+200+50 = 350.
	if mon.AvgW[0] != 350 {
		t.Errorf("monitored avg_w[0] = %v, want 350", mon.AvgW[0])
	}
	// meter is its OWN series.
	if meter.KWh[0] != 1.0 || meter.AvgW[0] != 1000 {
		t.Errorf("meter series wrong: kwh=%v w=%v", meter.KWh[0], meter.AvgW[0])
	}
	if meter.Class != energyMeterClass {
		t.Errorf("meter class = %q", meter.Class)
	}
}

func TestAssembleHouseNoMeter(t *testing.T) {
	loc := mustLondon(t)
	buckets := threeBuckets(loc)
	devices := map[string]config.DeviceConfig{
		"winefridge": {Class: "continuous_power_device"},
	}
	energy := map[string][]float64{"winefridge": {0.1, 0.1, 0.1}}
	out := AssembleSeries(buckets, devices, energy, nil, testTariff(), GroupByHouse)
	if len(out) != 1 || out[0].Key != houseMonitoredKey {
		t.Errorf("house without meter should be one monitored series: %+v", out)
	}
}

// ---- BuildSeries orchestrator (FakeQuerier) --------------------------------

// fluxRouter routes a flux script to the right canned rows by inspecting which
// builder produced it (energy_kwh vs power_w) and which device set it targets.
func TestBuildSeriesEndToEnd(t *testing.T) {
	loc := mustLondon(t)
	// Full 24h day, hourly → but use a short 3h window for compact fixtures.
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 3, 0, 0, 0, loc)
	win := Window{Start: start, Stop: stop, Label: WindowCustom}
	iv, _ := lookupInterval("1h")
	buckets := BucketStarts(win, iv, loc)
	if len(buckets) != 3 {
		t.Fatalf("expected 3 buckets, got %d", len(buckets))
	}

	devices := map[string]config.DeviceConfig{
		"winefridge":  {Class: "continuous_power_device", DisplayName: "Wine Fridge", Location: "kitchen"},
		"network-ups": {Class: "ups_sensor", DisplayName: "Network UPS", Location: "office"},
	}

	// counter rows: per-bucket energy for winefridge, stamped at LEFT edges.
	counterRows := []influx.Row{
		// A pad bucket (one interval before start) that MUST be dropped.
		{DeviceID: "winefridge", Field: "energy_kwh", Value: 999, Time: start.Add(-time.Hour)},
		{DeviceID: "winefridge", Field: "energy_kwh", Value: 0.05, Time: buckets[0]},
		{DeviceID: "winefridge", Field: "energy_kwh", Value: 0.04, Time: buckets[1]},
		{DeviceID: "winefridge", Field: "energy_kwh", Value: 0.06, Time: buckets[2]},
	}
	// UPS power rows (mean W): energy = mean × 1h / 1000.
	upsPowerRows := []influx.Row{
		{DeviceID: "network-ups", Field: "power_w", Value: 120, Time: buckets[0]}, // 0.12 kWh
		{DeviceID: "network-ups", Field: "power_w", Value: 100, Time: buckets[1]}, // 0.10 kWh
		{DeviceID: "network-ups", Field: "power_w", Value: 80, Time: buckets[2]},  // 0.08 kWh
	}
	// avg power rows for ALL metered (both devices).
	allPowerRows := []influx.Row{
		{DeviceID: "winefridge", Field: "power_w", Value: 50, Time: buckets[0]},
		{DeviceID: "winefridge", Field: "power_w", Value: 40, Time: buckets[1]},
		{DeviceID: "winefridge", Field: "power_w", Value: 60, Time: buckets[2]},
		{DeviceID: "network-ups", Field: "power_w", Value: 120, Time: buckets[0]},
		{DeviceID: "network-ups", Field: "power_w", Value: 100, Time: buckets[1]},
		{DeviceID: "network-ups", Field: "power_w", Value: 80, Time: buckets[2]},
	}

	q := &influx.FakeQuerier{
		QueryFunc: func(flux string) ([]influx.Row, error) {
			switch {
			case strings.Contains(flux, "energy_kwh"):
				return counterRows, nil
			case strings.Contains(flux, "power_w") && strings.Contains(flux, `"network-ups"`) && !strings.Contains(flux, `"winefridge"`):
				return upsPowerRows, nil
			case strings.Contains(flux, "power_w"):
				return allPowerRows, nil
			}
			return nil, nil
		},
	}

	resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv, GroupByDevice, devices, testTariff(), loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}

	// Exactly 3 queries regardless of device count.
	if len(q.Queries) != 3 {
		t.Fatalf("query count = %d, want 3", len(q.Queries))
	}

	if resp.Window != WindowCustom || resp.Interval != "1h" || resp.GroupBy != GroupByDevice {
		t.Errorf("response metadata wrong: %+v", resp)
	}
	if len(resp.Buckets) != 3 {
		t.Fatalf("response buckets = %d, want 3", len(resp.Buckets))
	}
	if len(resp.Series) != 2 {
		t.Fatalf("series count = %d, want 2", len(resp.Series))
	}

	byKey := map[string]Series{}
	for _, s := range resp.Series {
		byKey[s.Key] = s
	}

	wine := byKey["winefridge"]
	// Pad bucket (999) dropped; real per-bucket energy aligned.
	if wine.KWh[0] != 0.05 || wine.KWh[1] != 0.04 || wine.KWh[2] != 0.06 {
		t.Errorf("winefridge kwh = %v, want [0.05 0.04 0.06] (pad dropped)", wine.KWh)
	}
	if wine.AvgW[0] != 50 || wine.AvgW[2] != 60 {
		t.Errorf("winefridge avg_w = %v", wine.AvgW)
	}

	ups := byKey["network-ups"]
	// UPS energy = mean × hours / 1000: 0.12, 0.10, 0.08.
	if ups.KWh[0] != 0.12 || ups.KWh[1] != 0.1 || ups.KWh[2] != 0.08 {
		t.Errorf("ups kwh = %v, want [0.12 0.1 0.08]", ups.KWh)
	}
	if ups.AvgW[0] != 120 {
		t.Errorf("ups avg_w[0] = %v, want 120", ups.AvgW[0])
	}
}

func TestBuildSeriesQueryError(t *testing.T) {
	loc := mustLondon(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 3, 0, 0, 0, loc)
	win := Window{Start: start, Stop: stop, Label: WindowCustom}
	iv, _ := lookupInterval("1h")
	devices := map[string]config.DeviceConfig{
		"winefridge": {Class: "continuous_power_device"},
	}
	q := &influx.FakeQuerier{Err: context.DeadlineExceeded}
	if _, err := BuildSeries(context.Background(), q, "statehouse", win, iv, GroupByDevice, devices, testTariff(), loc); err == nil {
		t.Fatal("BuildSeries should propagate query error")
	}
}

func TestBuildSeriesNoMeteredDevices(t *testing.T) {
	loc := mustLondon(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 3, 0, 0, 0, loc)
	win := Window{Start: start, Stop: stop, Label: WindowCustom}
	iv, _ := lookupInterval("1h")
	// Only an unmetered/unknown class.
	devices := map[string]config.DeviceConfig{
		"doorbell": {Class: "binary_sensor"},
	}
	q := &influx.FakeQuerier{}
	resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv, GroupByDevice, devices, testTariff(), loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	if len(q.Queries) != 0 {
		t.Errorf("no metered devices → no queries, got %d", len(q.Queries))
	}
	if len(resp.Series) != 0 {
		t.Errorf("no metered devices → no series, got %d", len(resp.Series))
	}
}

func TestBuildSeriesDefaultGroupByReported(t *testing.T) {
	loc := mustLondon(t)
	win := Window{
		Start: time.Date(2026, 6, 11, 0, 0, 0, 0, loc),
		Stop:  time.Date(2026, 6, 11, 1, 0, 0, 0, loc),
		Label: WindowCustom,
	}
	iv, _ := lookupInterval("1h")
	q := &influx.FakeQuerier{}
	resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv, "", nil, testTariff(), loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	if resp.GroupBy != GroupByDevice {
		t.Errorf("empty group_by should report %q, got %q", GroupByDevice, resp.GroupBy)
	}
}

// bucketHours: the final period-to-date bucket is partial; UPS energy must use
// the real hours.
func TestBuildSeriesUPSPartialFinalBucket(t *testing.T) {
	loc := mustLondon(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	// Stop mid-way through the 3rd hour: final bucket is 0.5h.
	stop := time.Date(2026, 6, 11, 2, 30, 0, 0, loc)
	win := Window{Start: start, Stop: stop, Label: WindowToday}
	iv, _ := lookupInterval("1h")
	buckets := BucketStarts(win, iv, loc)
	if len(buckets) != 3 {
		t.Fatalf("buckets = %d, want 3", len(buckets))
	}

	devices := map[string]config.DeviceConfig{
		"network-ups": {Class: "ups_sensor", DisplayName: "UPS"},
	}
	upsPowerRows := []influx.Row{
		{DeviceID: "network-ups", Field: "power_w", Value: 100, Time: buckets[0]},
		{DeviceID: "network-ups", Field: "power_w", Value: 100, Time: buckets[1]},
		{DeviceID: "network-ups", Field: "power_w", Value: 100, Time: buckets[2]},
	}
	q := &influx.FakeQuerier{
		QueryFunc: func(flux string) ([]influx.Row, error) {
			if strings.Contains(flux, "power_w") {
				return upsPowerRows, nil
			}
			return nil, nil
		},
	}
	resp, err := BuildSeries(context.Background(), q, "statehouse", win, iv, GroupByDevice, devices, testTariff(), loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	ups := resp.Series[0]
	// Hours 0 and 1 are full (0.1 kWh each); the final bucket is 0.5h → 0.05.
	if ups.KWh[0] != 0.1 || ups.KWh[1] != 0.1 {
		t.Errorf("full-bucket ups kwh = %v", ups.KWh[:2])
	}
	if ups.KWh[2] != 0.05 {
		t.Errorf("partial final bucket ups kwh = %v, want 0.05", ups.KWh[2])
	}
}
