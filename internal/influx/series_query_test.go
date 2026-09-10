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

// Both series builders MUST stamp buckets at their LEFT edge (timeSrc:"_start").
// Influx's aggregateWindow defaults to the right edge (_stop); with the canonical
// axis keyed on left edges, the default shifts every value one bucket late (the
// first bucket reads 0 and the last bucket's data is lost). This is invisible at
// fine intervals but glaring at coarse ones — caught via the breakdown demo.
func TestSeriesBuilders_StampLeftEdge(t *testing.T) {
	counter := BuildCounterSeriesFlux("b", []string{"d"}, seriesStart, seriesStop, "6h", "Europe/London")
	power := BuildPowerMeanSeriesFlux("b", []string{"d"}, seriesStart, seriesStop, "6h", "Europe/London")
	for name, flux := range map[string]string{"counter": counter, "power": power} {
		if !strings.Contains(flux, `timeSrc: "_start"`) {
			t.Errorf("%s builder must set timeSrc:\"_start\" (left-edge buckets); flux:\n%s", name, flux)
		}
	}
}

func TestBuildCounterSeriesFlux(t *testing.T) {
	flux := BuildCounterSeriesFlux("statehouse", []string{"winefridge", "freezer"}, seriesStart, seriesStop, "1h", "Europe/London")

	wants := []string{
		`import "timezone"`,
		`from(bucket: "statehouse")`,
		`r._measurement == "device_power"`,
		`r._field == "energy_kwh"`,
		`contains(value: r.device_id, set: ["winefridge", "freezer"])`,
		`increase()`,
		`aggregateWindow(every: 1h, fn: last, timeSrc: "_start", location: timezone.location(name: "Europe/London"), createEmpty: false)`,
		`stop: 2026-06-12T00:00:00Z`,
	}
	for _, w := range wants {
		if !strings.Contains(flux, w) {
			t.Errorf("counter series flux missing %q\n---\n%s", w, flux)
		}
	}

	// The range is the EXACT window: no pad. The pad is what used to anchor the
	// series at a reading taken before `from` (issue #29).
	if !strings.Contains(flux, `start: 2026-06-11T00:00:00Z`) {
		t.Errorf("counter series flux must range over the exact window, unpadded\n---\n%s", flux)
	}

	// increase() must precede aggregateWindow, so counter resets are absorbed
	// into the running total before it is bucketed.
	if strings.Index(flux, "increase()") > strings.Index(flux, "aggregateWindow") {
		t.Errorf("increase() must precede aggregateWindow\n---\n%s", flux)
	}

	// Differencing is the CALLER's job now (energy.demuxCounterTotals), so a
	// bucket with no reading can be told from a bucket worth zero.
	if strings.Contains(flux, "difference()") {
		t.Errorf("counter series must not difference() in Flux\n---\n%s", flux)
	}
	if !strings.Contains(flux, "createEmpty: false") {
		t.Errorf("counter series needs createEmpty:false; an empty bucket must be absent, "+
			"not a null decoding to a 0.0 running total\n---\n%s", flux)
	}

	// Counter path must not touch power_w / integral / mean.
	for _, bad := range []string{`power_w`, `integral(`, `fn: mean`} {
		if strings.Contains(flux, bad) {
			t.Errorf("counter series flux unexpectedly contains %q", bad)
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
		`aggregateWindow(every: 15m, fn: mean, timeSrc: "_start", location: timezone.location(name: "Europe/London"), createEmpty: true)`,
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
