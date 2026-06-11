package energy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/influx"
)

var (
	start = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	stop  = time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
)

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

func TestDeviceWindowKWhMultipleRowsTakesLast(t *testing.T) {
	f := &influx.FakeQuerier{Responses: map[string][]influx.Row{
		"multi": {{Value: 1.0}, {Value: 2.0}, {Value: 3.5}},
	}}
	kwh, _, err := DeviceWindowKWh(context.Background(), f, "statehouse",
		"multi", "energy_meter", start, stop)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if kwh != 3.5 {
		t.Fatalf("kwh = %v, want last row 3.5", kwh)
	}
}
