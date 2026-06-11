package influx

import (
	"context"
	"strings"
	"testing"
)

func TestBuildActivityFlux(t *testing.T) {
	flux := BuildActivityFlux("statehouse", []string{"hot_water", "boiler"}, testStart, testStop)

	wants := []string{
		`from(bucket: "statehouse")`,
		`r._measurement == "device_activity"`,
		`contains(value: r.device_id, set: ["hot_water", "boiler"])`,
		`start: 2026-06-01T00:00:00Z`,
		`stop: 2026-06-08T00:00:00Z`,
	}
	for _, w := range wants {
		if !strings.Contains(flux, w) {
			t.Errorf("activity flux missing %q\n---\n%s", w, flux)
		}
	}
	// No pivot, no power/energy concerns — this is the raw from/to source.
	for _, bad := range []string{`pivot(`, `device_power`, `increase()`, `integral(`} {
		if strings.Contains(flux, bad) {
			t.Errorf("activity flux unexpectedly contains %q", bad)
		}
	}
}

func TestBuildActivityCarryInFlux(t *testing.T) {
	flux := BuildActivityCarryInFlux("statehouse", []string{"hot_water"}, testStop)

	wants := []string{
		`from(bucket: "statehouse")`,
		`r._measurement == "device_activity"`,
		`contains(value: r.device_id, set: ["hot_water"])`,
		// Reaches arbitrarily far back, stops at the window start.
		`range(start: 1970-01-01T00:00:00Z, stop: 2026-06-08T00:00:00Z)`,
		`group(columns: ["device_id", "_field"])`,
		`last()`,
	}
	for _, w := range wants {
		if !strings.Contains(flux, w) {
			t.Errorf("carry-in flux missing %q\n---\n%s", w, flux)
		}
	}
}

// TestRowCarriesText proves the FakeQuerier round-trips string-valued rows via
// the new Row.Text field while numeric rows still use Value. (The real Client's
// record mapping is exercised indirectly; this guards the wire-shape the events
// package depends on.)
func TestRowCarriesText(t *testing.T) {
	rows := []Row{
		{DeviceID: "hot_water", Field: "from", Text: "idle"},
		{DeviceID: "hot_water", Field: "to", Text: "active"},
		{DeviceID: "winefridge", Field: "energy_kwh", Value: 12.5},
	}
	f := &FakeQuerier{Responses: map[string][]Row{"device_activity": rows}}
	got, err := f.Query(context.Background(), BuildActivityFlux("statehouse", []string{"hot_water"}, testStart, testStop))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3", len(got))
	}
	if got[0].Text != "idle" || got[1].Text != "active" {
		t.Errorf("string rows did not carry Text: %+v", got[:2])
	}
	if got[0].Value != 0 {
		t.Errorf("string row should have zero Value, got %v", got[0].Value)
	}
	if got[2].Value != 12.5 || got[2].Text != "" {
		t.Errorf("numeric row malformed: %+v", got[2])
	}
}
