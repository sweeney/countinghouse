package influx

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	testStart = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	testStop  = time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
)

func TestBuildCounterFlux(t *testing.T) {
	flux := BuildCounterFlux("statehouse", "winefridge", testStart, testStop)

	wants := []string{
		`from(bucket: "statehouse")`,
		`r._measurement == "device_power"`,
		`r._field == "energy_kwh"`,
		`r.device_id == "winefridge"`,
		`increase()`,
		`last()`,
		`start: 2026-06-01T00:00:00Z`,
		`stop: 2026-06-08T00:00:00Z`,
	}
	for _, w := range wants {
		if !strings.Contains(flux, w) {
			t.Errorf("counter flux missing %q\n---\n%s", w, flux)
		}
	}
	// The counter path must not touch power_w or integral.
	for _, bad := range []string{`power_w`, `integral(`} {
		if strings.Contains(flux, bad) {
			t.Errorf("counter flux unexpectedly contains %q", bad)
		}
	}
}

func TestBuildIntegralFlux(t *testing.T) {
	flux := BuildIntegralFlux("statehouse", "network-ups", testStart, testStop)

	wants := []string{
		`from(bucket: "statehouse")`,
		`r._measurement == "device_power"`,
		`r._field == "power_w"`,
		`r.device_id == "network-ups"`,
		`integral(unit: 1h, interpolate: "linear")`,
		`_value: r._value / 1000.0`,
		`start: 2026-06-01T00:00:00Z`,
		`stop: 2026-06-08T00:00:00Z`,
	}
	for _, w := range wants {
		if !strings.Contains(flux, w) {
			t.Errorf("integral flux missing %q\n---\n%s", w, flux)
		}
	}
	// The integral path must not touch energy_kwh / increase.
	for _, bad := range []string{`energy_kwh`, `increase()`} {
		if strings.Contains(flux, bad) {
			t.Errorf("integral flux unexpectedly contains %q", bad)
		}
	}
}

func TestFluxTimeNormalisesToUTC(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	// 01:00 BST == 00:00 UTC.
	bst := time.Date(2026, 6, 1, 1, 0, 0, 0, loc)
	if got := fluxTime(bst); got != "2026-06-01T00:00:00Z" {
		t.Fatalf("fluxTime(%v) = %q, want UTC RFC3339", bst, got)
	}
}

func TestFakeQuerierRecordsAndResponds(t *testing.T) {
	rows := []Row{{DeviceID: "winefridge", Field: "energy_kwh", Value: 12.5}}
	f := &FakeQuerier{Responses: map[string][]Row{"winefridge": rows}}

	flux := BuildCounterFlux("statehouse", "winefridge", testStart, testStop)
	got, err := f.Query(context.Background(), flux)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 || got[0].Value != 12.5 {
		t.Fatalf("rows = %+v, want value 12.5", got)
	}
	if f.LastQuery() != flux {
		t.Fatalf("LastQuery did not record flux")
	}
	if len(f.Queries) != 1 {
		t.Fatalf("Queries len = %d, want 1", len(f.Queries))
	}
}

func TestFakeQuerierNoMatchEmpty(t *testing.T) {
	f := &FakeQuerier{Responses: map[string][]Row{"winefridge": {{Value: 1}}}}
	got, err := f.Query(context.Background(), `r.device_id == "other"`)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got != nil {
		t.Fatalf("rows = %+v, want nil", got)
	}
}

func TestFakeQuerierErr(t *testing.T) {
	sentinel := errors.New("boom")
	f := &FakeQuerier{Err: sentinel}
	if _, err := f.Query(context.Background(), "x"); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

func TestFakeQuerierQueryFunc(t *testing.T) {
	f := &FakeQuerier{QueryFunc: func(flux string) ([]Row, error) {
		return []Row{{Value: 99}}, nil
	}}
	got, err := f.Query(context.Background(), "anything")
	if err != nil || len(got) != 1 || got[0].Value != 99 {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestFakeQuerierPing(t *testing.T) {
	if (&FakeQuerier{PingOK: true}).Ping(context.Background()) != true {
		t.Fatal("Ping should be true")
	}
	if (&FakeQuerier{}).Ping(context.Background()) != false {
		t.Fatal("Ping should default false")
	}
}

func TestDisabledClient(t *testing.T) {
	c := New(Config{}) // empty -> disabled
	if _, err := c.Query(context.Background(), "x"); err == nil {
		t.Fatal("disabled Query should error")
	}
	if c.Ping(context.Background()) {
		t.Fatal("disabled Ping should be false")
	}
	c.Close() // must not panic
}
