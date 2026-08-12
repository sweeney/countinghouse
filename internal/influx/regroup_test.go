package influx

import (
	"strings"
	"testing"
	"time"
)

// Influx starts a NEW TABLE whenever a point's tag set changes, and statehouse's
// location -> site migration changed it mid-window. Every builder here then operated on
// fragments of one device's series instead of the series, and each reducer broke in its
// own way:
//
//   - increase()|>last() accumulated only within a fragment, so summing the fragments
//     lost the delta across each boundary (small: ~0.003 kWh/day for the meter)
//   - integral() integrates over the GROUP KEY's _start/_stop — the whole window — so a
//     fragment holding a third of the samples was extrapolated across all 24h. Three
//     fragments each returned ~the full day's energy, and summing them TRIPLED it
//   - difference() needs two consecutive populated buckets, so a fragment with no prior
//     bucket yielded null and its opening delta vanished (issue #17)
//
// Measured on production data for 2026-08-11, network-ups: the three fragments hold
// 2065 + 177 + 638 = 2880 samples, one per 30s for 24h — a partition of one sensor, not
// duplicates. Each averages ~76.5 W, so the day is 76.5 W x 24h = 1.837 kWh. Summing the
// per-fragment integrals gave 5.533 kWh, 3.01x the truth.
//
// The fix is upstream of every reducer: collapse a device's fragments into one table
// before any maths runs. keep() first, because grouping tables whose column sets differ
// (one has `site`, one has `location`, one has both) panics Influx with
// "arrow/array: index out of range". sort() because merged tables are not time-ordered
// and every reducer here is order-sensitive.

func regroupWants(groupKey string) []string {
	return []string{
		`|> keep(columns:`,
		`"device_id"`,
		`|> group(columns: [` + groupKey,
		`|> sort(columns: ["_time"])`,
	}
}

// assertOrder pins that the regrouping happens BEFORE the reducer. Getting the lines in
// the wrong order is a silent no-op: the reducer still sees fragments.
func assertOrder(t *testing.T, flux, reducer string) {
	t.Helper()
	g := strings.Index(flux, "|> group(columns:")
	s := strings.Index(flux, `|> sort(columns: ["_time"])`)
	r := strings.Index(flux, reducer)
	if g < 0 || s < 0 || r < 0 {
		t.Fatalf("missing group/sort/%s in:\n%s", reducer, flux)
	}
	if !(g < s && s < r) {
		t.Errorf("regrouping must precede %s (group=%d sort=%d reducer=%d):\n%s",
			reducer, g, s, r, flux)
	}
}

func TestCounterSeriesFluxRegroupsBeforeIncrease(t *testing.T) {
	flux := BuildCounterSeriesFlux("statehouse", []string{"winefridge"},
		time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), "1h", "Europe/London")
	for _, w := range regroupWants(`"device_id"]`) {
		if !strings.Contains(flux, w) {
			t.Errorf("counter series flux missing %q\n---\n%s", w, flux)
		}
	}
	assertOrder(t, flux, "increase()")
}

func TestPowerMeanSeriesFluxRegroupsBeforeAggregate(t *testing.T) {
	flux := BuildPowerMeanSeriesFlux("statehouse", []string{"network-ups"},
		time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), "1h", "Europe/London")
	for _, w := range regroupWants(`"device_id"]`) {
		if !strings.Contains(flux, w) {
			t.Errorf("power mean series flux missing %q\n---\n%s", w, flux)
		}
	}
	assertOrder(t, flux, "aggregateWindow(")
}

func TestCounterWindowFluxRegroupsBeforeIncrease(t *testing.T) {
	flux := BuildCounterFlux("statehouse", "electricity_meter",
		time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	for _, w := range regroupWants(`"device_id"`) {
		if !strings.Contains(flux, w) {
			t.Errorf("counter window flux missing %q\n---\n%s", w, flux)
		}
	}
	assertOrder(t, flux, "increase()")
}

// integral() reads its bounds from the group key, so _start and _stop must survive keep()
// AND stay in the group key — otherwise Influx rejects the query outright with
// "integral: integral function needs _start column to be part of group key".
func TestIntegralWindowFluxKeepsStartInTheGroupKey(t *testing.T) {
	flux := BuildIntegralFlux("statehouse", "network-ups",
		time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC))
	for _, w := range regroupWants(`"device_id"`) {
		if !strings.Contains(flux, w) {
			t.Errorf("integral flux missing %q\n---\n%s", w, flux)
		}
	}
	for _, w := range []string{`"_start"`, `"_stop"`} {
		if !strings.Contains(flux, w) {
			t.Errorf("integral flux must retain %s for the group key\n---\n%s", w, flux)
		}
	}
	assertOrder(t, flux, "integral(")
}

// The series builders must NOT put _start/_stop in the group key: aggregateWindow rewrites
// them per bucket, which would split every bucket into its own table.
func TestSeriesFluxDoesNotGroupOnStart(t *testing.T) {
	for name, flux := range map[string]string{
		"counter": BuildCounterSeriesFlux("statehouse", []string{"winefridge"},
			time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), "1h", "Europe/London"),
		"power": BuildPowerMeanSeriesFlux("statehouse", []string{"network-ups"},
			time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC),
			time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), "1h", "Europe/London"),
	} {
		i := strings.Index(flux, "|> group(columns: [")
		if i < 0 {
			t.Fatalf("%s: no group()", name)
		}
		line := flux[i:]
		if j := strings.Index(line, "\n"); j > 0 {
			line = line[:j]
		}
		if strings.Contains(line, "_start") || strings.Contains(line, "_stop") {
			t.Errorf("%s: aggregateWindow rewrites _start/_stop per bucket, so grouping on "+
				"them splits every bucket into its own table; got %q", name, line)
		}
	}
}
