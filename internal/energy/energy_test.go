package energy

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/influx"
)

var (
	start = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	stop  = time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
)

// TestEnergyMeterClassConstant locks the exported single-source-of-truth meter
// class constant: its value, and that the energy package routes it via the
// counter path and includes it in counterClasses. Guards against the /bill
// handler and the energy package drifting on the magic "energy_meter" string
// (issue #7).
func TestEnergyMeterClassConstant(t *testing.T) {
	if EnergyMeterClass != "energy_meter" {
		t.Fatalf("EnergyMeterClass = %q, want energy_meter", EnergyMeterClass)
	}
	if path, ok := PathForClass(EnergyMeterClass); !ok || path != PathCounter {
		t.Fatalf("PathForClass(EnergyMeterClass) = (%q, %v), want (counter, true)", path, ok)
	}
	if !counterClasses[EnergyMeterClass] {
		t.Fatal("counterClasses must include EnergyMeterClass")
	}
}

func TestPathForClass(t *testing.T) {
	cases := []struct {
		class    string
		wantPath string
		wantOK   bool
	}{
		{"continuous_power_device", PathCounter, true},
		{"cycle_power_device", PathCounter, true},
		{"short_burst_power_device", PathCounter, true},
		{"media_power_device", PathCounter, true},
		{"energy_meter", PathCounter, true},
		{"ups_sensor", PathIntegral, true},
		{"environment_sensor", "", false},
		{"", "", false},
		{"unknown", "", false},
	}
	for _, c := range cases {
		t.Run(c.class, func(t *testing.T) {
			path, ok := PathForClass(c.class)
			if path != c.wantPath || ok != c.wantOK {
				t.Fatalf("PathForClass(%q) = (%q,%v), want (%q,%v)",
					c.class, path, ok, c.wantPath, c.wantOK)
			}
		})
	}
}

func TestDeviceWindowKWhCounter(t *testing.T) {
	f := &influx.FakeQuerier{Responses: map[string][]influx.Row{
		"winefridge": {{DeviceID: "winefridge", Field: "energy_kwh", Value: 8.25}},
	}}
	kwh, source, err := DeviceWindowKWh(context.Background(), f, "statehouse",
		"winefridge", "media_power_device", start, stop)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if kwh != 8.25 {
		t.Fatalf("kwh = %v, want 8.25", kwh)
	}
	if source != PathCounter {
		t.Fatalf("source = %q, want %q", source, PathCounter)
	}
	// Confirm it used the counter builder.
	if q := f.LastQuery(); !strings.Contains(q, "increase()") || !strings.Contains(q, "energy_kwh") {
		t.Fatalf("expected counter flux, got:\n%s", q)
	}
}

func TestDeviceWindowKWhIntegral(t *testing.T) {
	f := &influx.FakeQuerier{Responses: map[string][]influx.Row{
		"network-ups": {{DeviceID: "network-ups", Field: "power_w", Value: 1.732}},
	}}
	kwh, source, err := DeviceWindowKWh(context.Background(), f, "statehouse",
		"network-ups", "ups_sensor", start, stop)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if kwh != 1.732 {
		t.Fatalf("kwh = %v, want 1.732", kwh)
	}
	if source != PathIntegral {
		t.Fatalf("source = %q, want %q", source, PathIntegral)
	}
	if q := f.LastQuery(); !strings.Contains(q, "integral(") || !strings.Contains(q, "power_w") {
		t.Fatalf("expected integral flux, got:\n%s", q)
	}
}

func TestDeviceWindowKWhUnknownClass(t *testing.T) {
	f := &influx.FakeQuerier{}
	_, _, err := DeviceWindowKWh(context.Background(), f, "statehouse",
		"thing", "doorbell", start, stop)
	if err == nil {
		t.Fatal("expected error for unknown class")
	}
	if len(f.Queries) != 0 {
		t.Fatal("should not have queried for unknown class")
	}
}

func TestDeviceWindowKWhEmptyResult(t *testing.T) {
	f := &influx.FakeQuerier{} // no responses -> empty
	kwh, source, err := DeviceWindowKWh(context.Background(), f, "statehouse",
		"offline", "cycle_power_device", start, stop)
	if err != nil {
		t.Fatalf("empty result should be nil err, got %v", err)
	}
	if kwh != 0 {
		t.Fatalf("kwh = %v, want 0", kwh)
	}
	if source != PathCounter {
		t.Fatalf("source = %q, want %q", source, PathCounter)
	}
}

func TestDeviceWindowKWhQueryError(t *testing.T) {
	sentinel := errors.New("influx down")
	f := &influx.FakeQuerier{Err: sentinel}
	_, source, err := DeviceWindowKWh(context.Background(), f, "statehouse",
		"winefridge", "continuous_power_device", start, stop)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if source != PathCounter {
		t.Fatalf("source = %q, want %q even on error", source, PathCounter)
	}
}

// Both reducers — increase()|>last() and integral() — collapse to one row PER
// TABLE, and Influx starts a new table whenever a tag value changes inside the
// window: a room rename, a class correction, a new site tag. So a multi-row
// result is the normal shape for any window spanning such a change, not an
// anomaly, and every row is a distinct slice of the same device's energy.
//
// Taking only the last row discarded all the earlier slices. In production that
// made a longer window report LESS than a window inside it: electricity_meter
// read 3.357 kWh for 12 Aug, but 0.562 kWh for 2d, 7d and week alike — every
// window containing 11 Aug, the day the floorplan tags changed. A 7-day bill was
// smaller than one of its own days.
func TestDeviceWindowKWhCounterSumsEveryTable(t *testing.T) {
	f := &influx.FakeQuerier{Responses: map[string][]influx.Row{
		"electricity_meter": {
			{DeviceID: "electricity_meter", Field: "energy_kwh", Value: 5.817},
			{DeviceID: "electricity_meter", Field: "energy_kwh", Value: 0.562},
		},
	}}
	kwh, source, err := DeviceWindowKWh(context.Background(), f, "statehouse",
		"electricity_meter", EnergyMeterClass, start, stop)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if math.Abs(kwh-6.379) > 1e-9 {
		t.Fatalf("kwh = %v, want 6.379 (5.817 + 0.562): every table is real energy", kwh)
	}
	if source != PathCounter {
		t.Fatalf("source = %q, want %q", source, PathCounter)
	}
}

// The integral path must NOT sum, and this is the opposite of the counter path above.
//
// integral() reads its integration bounds from the group key's _start/_stop — the whole
// window — so a fragment holding a third of the samples is extrapolated across all of it.
// Fragments are therefore competing ESTIMATES OF THE SAME QUANTITY, not addends. Summing
// three of them tripled the UPS bill in production (5.533 kWh against a true 1.837).
//
// The Flux now returns one table per device so this cannot arise, but the reduction must
// not be the thing that re-creates it: if fragments ever reappear, one estimate is roughly
// right and their sum is a multiple of the truth.
func TestDeviceWindowKWhIntegralDoesNotSumFragments(t *testing.T) {
	f := &influx.FakeQuerier{Responses: map[string][]influx.Row{
		"network-ups": {
			// Two extrapolations of the same day, as fragmentation produced.
			{DeviceID: "network-ups", Field: "power_w", Value: 1.837},
			{DeviceID: "network-ups", Field: "power_w", Value: 1.848},
		},
	}}
	kwh, source, err := DeviceWindowKWh(context.Background(), f, "statehouse",
		"network-ups", "ups_sensor", start, stop)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if math.Abs(kwh-3.685) < 1e-9 {
		t.Fatal("integral fragments were summed: that is the 3x over-count this fix removes")
	}
	if math.Abs(kwh-1.848) > 1e-9 {
		t.Fatalf("kwh = %v, want a single estimate (1.848), not a sum", kwh)
	}
	if source != PathIntegral {
		t.Fatalf("source = %q, want %q", source, PathIntegral)
	}
}

// The single-row case is what the regrouped Flux actually returns, on both paths.
func TestDeviceWindowKWhIntegralSingleRow(t *testing.T) {
	f := &influx.FakeQuerier{Responses: map[string][]influx.Row{
		"network-ups": {{DeviceID: "network-ups", Field: "power_w", Value: 1.8366}},
	}}
	kwh, _, err := DeviceWindowKWh(context.Background(), f, "statehouse",
		"network-ups", "ups_sensor", start, stop)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if math.Abs(kwh-1.8366) > 1e-9 {
		t.Fatalf("kwh = %v, want 1.8366 unchanged", kwh)
	}
}

// A single-table result is the common case and must be unchanged by the sum.
func TestDeviceWindowKWhSingleRowIsUnchanged(t *testing.T) {
	f := &influx.FakeQuerier{Responses: map[string][]influx.Row{
		"solo": {{Value: 3.5}},
	}}
	kwh, _, err := DeviceWindowKWh(context.Background(), f, "statehouse",
		"solo", EnergyMeterClass, start, stop)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if kwh != 3.5 {
		t.Fatalf("kwh = %v, want 3.5", kwh)
	}
}
