package influx

import (
	"strings"
	"testing"
	"time"
)

var (
	seriesStart = time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	seriesStop  = time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
)

func TestBuildCounterSeriesFlux(t *testing.T) {
	flux := BuildCounterSeriesFlux("statehouse", []string{"winefridge", "freezer"}, seriesStart, seriesStop, "1h", "Europe/London")

	wants := []string{
		`import "timezone"`,
		`from(bucket: "statehouse")`,
		`r._measurement == "device_power"`,
		`r._field == "energy_kwh"`,
		`contains(value: r.device_id, set: ["winefridge", "freezer"])`,
		`increase()`,
		`aggregateWindow(every: 1h, fn: last, location: timezone.location(name: "Europe/London"), createEmpty: true)`,
		`difference()`,
		`stop: 2026-06-12T00:00:00Z`,
	}
	for _, w := range wants {
		if !strings.Contains(flux, w) {
			t.Errorf("counter series flux missing %q\n---\n%s", w, flux)
		}
	}

	// Range must be padded one interval (1h) BEFORE start (23:00 prior day).
	if !strings.Contains(flux, `start: 2026-06-10T23:00:00Z`) {
		t.Errorf("counter series flux not padded one interval before start\n---\n%s", flux)
	}

	// Ordering: increase() BEFORE aggregateWindow BEFORE difference().
	iInc := strings.Index(flux, "increase()")
	iAgg := strings.Index(flux, "aggregateWindow")
	iDiff := strings.Index(flux, "difference()")
	if !(iInc < iAgg && iAgg < iDiff) {
		t.Errorf("counter ordering wrong: increase=%d aggregate=%d difference=%d\n---\n%s", iInc, iAgg, iDiff, flux)
	}

	// Counter path must not touch power_w / integral / mean.
	for _, bad := range []string{`power_w`, `integral(`, `fn: mean`} {
		if strings.Contains(flux, bad) {
			t.Errorf("counter series flux unexpectedly contains %q", bad)
		}
	}
}

func TestBuildCounterSeriesFluxPadByInterval(t *testing.T) {
	cases := []struct {
		interval  string
		wantStart string
	}{
		{"5m", "2026-06-10T23:55:00Z"},
		{"15m", "2026-06-10T23:45:00Z"},
		{"30m", "2026-06-10T23:30:00Z"},
		{"1h", "2026-06-10T23:00:00Z"},
		{"6h", "2026-06-10T18:00:00Z"},
		{"1d", "2026-06-10T00:00:00Z"},
	}
	for _, c := range cases {
		flux := BuildCounterSeriesFlux("statehouse", []string{"x"}, seriesStart, seriesStop, c.interval, "Europe/London")
		if !strings.Contains(flux, "start: "+c.wantStart) {
			t.Errorf("interval %s: want padded start %s\n---\n%s", c.interval, c.wantStart, flux)
		}
		if !strings.Contains(flux, "every: "+c.interval+",") {
			t.Errorf("interval %s: aggregateWindow every token missing\n---\n%s", c.interval, flux)
		}
	}
}

func TestBuildPowerMeanSeriesFlux(t *testing.T) {
	flux := BuildPowerMeanSeriesFlux("statehouse", []string{"network-ups", "office-ups"}, seriesStart, seriesStop, "15m", "Europe/London")

	wants := []string{
		`import "timezone"`,
		`from(bucket: "statehouse")`,
		`r._measurement == "device_power"`,
		`r._field == "power_w"`,
		`contains(value: r.device_id, set: ["network-ups", "office-ups"])`,
		`aggregateWindow(every: 15m, fn: mean, location: timezone.location(name: "Europe/London"), createEmpty: true)`,
		// No pad for the mean series: range starts AT the window start.
		`start: 2026-06-11T00:00:00Z`,
		`stop: 2026-06-12T00:00:00Z`,
	}
	for _, w := range wants {
		if !strings.Contains(flux, w) {
			t.Errorf("power mean series flux missing %q\n---\n%s", w, flux)
		}
	}

	// Mean path must not touch energy_kwh / increase / difference.
	for _, bad := range []string{`energy_kwh`, `increase()`, `difference()`} {
		if strings.Contains(flux, bad) {
			t.Errorf("power mean series flux unexpectedly contains %q", bad)
		}
	}
}

func TestDeviceSet(t *testing.T) {
	if got := deviceSet([]string{"a", "b", "c"}); got != `["a", "b", "c"]` {
		t.Errorf("deviceSet = %q", got)
	}
	if got := deviceSet([]string{"only"}); got != `["only"]` {
		t.Errorf("deviceSet single = %q", got)
	}
	if got := deviceSet(nil); got != `[]` {
		t.Errorf("deviceSet empty = %q", got)
	}
}
