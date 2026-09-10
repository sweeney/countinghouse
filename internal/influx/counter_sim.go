package influx

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// CounterSim is a deterministic stand-in for InfluxDB over the cumulative
// energy_kwh counter, for use as a Querier (or as a FakeQuerier.QueryFunc via
// Answer) in tests that need the DATABASE's arithmetic rather than hand-written
// per-bucket fixtures.
//
// It holds each device's counter as a series of cumulative readings and answers
// BOTH Flux shapes countinghouse issues against that field, honouring range()
// bounds and the local-midnight aggregation grid the way Influx does:
//
//   - increase() |> last()                                  — BuildCounterFlux
//     and BuildCounterHeadFlux (the whole-window and partial-head reductions):
//     one row per device, the counter's rise between the first and last reading
//     inside the range. increase() re-bases at the first point in range, so a
//     range containing a single reading yields 0.
//   - increase() |> aggregateWindow(...) |> difference()    — BuildCounterSeriesFlux:
//     one row per grid window after the first, stamped at the window's LEFT edge
//     (timeSrc: "_start"), carrying that window's delta. The first window is
//     consumed as difference()'s seed, which is what the builder's padded range
//     exists to supply.
//
// Why it earns its place: assertions that two ENDPOINTS agree are only
// meaningful if both read one underlying counter through one model of the
// query engine. Two hand-tuned fixture sets agreeing proves nothing, and two
// separately-maintained simulators can drift into proving different things.
//
// Scope: energy_kwh only. A query for any other field (power_w) returns no rows,
// so a test needing power telemetry alongside it should compose this with its own
// rows. Counter RESETS are not modelled — supply a monotonic counter; the
// increase() that absorbs resets in production is exercised against real Influx.
type CounterSim struct {
	// Loc is the timezone whose local midnight anchors the aggregation grid,
	// matching aggregateWindow(location: timezone.location(...)). Defaults to
	// UTC when nil.
	Loc *time.Location

	ids    []string // registration order, so rows are returned deterministically
	series map[string]counterSeries
}

// counterSeries is one device's cumulative counter, ascending by time.
type counterSeries struct {
	at  []time.Time
	kwh []float64
}

// NewCounterSim returns an empty sim whose aggregation grid is anchored at local
// midnight in loc.
func NewCounterSim(loc *time.Location) *CounterSim {
	return &CounterSim{Loc: loc, series: map[string]counterSeries{}}
}

// AddSteady registers deviceID with a steady `watts` load sampled every
// `cadence` across [from, to] inclusive, its counter starting at 0. It returns
// the sim so registrations can be chained.
func (c *CounterSim) AddSteady(deviceID string, from, to time.Time, cadence time.Duration, watts float64) *CounterSim {
	var at []time.Time
	var kwh []float64
	for ts := from; !ts.After(to); ts = ts.Add(cadence) {
		at = append(at, ts)
		kwh = append(kwh, watts*ts.Sub(from).Hours()/1000)
	}
	return c.AddSamples(deviceID, at, kwh)
}

// AddSamples registers deviceID with explicit CUMULATIVE counter readings, for a
// load that is not steady. at must be ascending and the two slices the same
// length. It returns the sim so registrations can be chained.
func (c *CounterSim) AddSamples(deviceID string, at []time.Time, cumulativeKWh []float64) *CounterSim {
	if c.series == nil {
		c.series = map[string]counterSeries{}
	}
	if _, seen := c.series[deviceID]; !seen {
		c.ids = append(c.ids, deviceID)
	}
	c.series[deviceID] = counterSeries{at: at, kwh: cumulativeKWh}
	return c
}

// Query implements Querier.
func (c *CounterSim) Query(_ context.Context, flux string) ([]Row, error) {
	return c.Answer(flux)
}

// Ping implements Querier; the sim is always reachable.
func (c *CounterSim) Ping(context.Context) bool { return true }

var (
	simRangeRe = regexp.MustCompile(`range\(start: (\S+), stop: (\S+)\)`)
	simEveryRe = regexp.MustCompile(`every: (\w+),`)
)

// Answer resolves one Flux script against the registered counters. Its signature
// matches FakeQuerier.QueryFunc, so a test wanting FakeQuerier's query recording
// can wire it in as `&FakeQuerier{QueryFunc: sim.Answer}` instead of using the
// sim as the Querier directly.
func (c *CounterSim) Answer(flux string) ([]Row, error) {
	if !strings.Contains(flux, `"energy_kwh"`) {
		return nil, nil // not this sim's field
	}
	start, stop, err := simRange(flux)
	if err != nil {
		return nil, err
	}

	var every time.Duration
	bucketed := strings.Contains(flux, "aggregateWindow")
	if bucketed {
		m := simEveryRe.FindStringSubmatch(flux)
		if m == nil {
			return nil, fmt.Errorf("influx: CounterSim: aggregateWindow with no every: in flux:\n%s", flux)
		}
		d, ok := intervalDuration(m[1])
		if !ok {
			return nil, fmt.Errorf("influx: CounterSim: unsupported interval %q", m[1])
		}
		every = d
	}

	var rows []Row
	for _, id := range c.ids {
		// Both the single-device predicate (r.device_id == "id") and the set
		// form (contains(..., set: ["id", ...])) embed the id quoted.
		if !strings.Contains(flux, `"`+id+`"`) {
			continue
		}
		in := c.series[id].inRange(start, stop)
		if len(in.at) == 0 {
			continue
		}
		if bucketed {
			rows = append(rows, in.windowDeltas(id, c.location(), every, stop)...)
			continue
		}
		rows = append(rows, Row{
			DeviceID: id,
			Field:    "energy_kwh",
			// increase() re-bases at the first reading in range.
			Value: in.kwh[len(in.kwh)-1] - in.kwh[0],
			Time:  in.at[len(in.at)-1],
		})
	}
	return rows, nil
}

func (c *CounterSim) location() *time.Location {
	if c.Loc == nil {
		return time.UTC
	}
	return c.Loc
}

// simRange parses the half-open bounds out of a range() call.
func simRange(flux string) (start, stop time.Time, err error) {
	m := simRangeRe.FindStringSubmatch(flux)
	if m == nil {
		return time.Time{}, time.Time{}, fmt.Errorf("influx: CounterSim: no range() in flux:\n%s", flux)
	}
	if start, err = time.Parse(time.RFC3339, m[1]); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if stop, err = time.Parse(time.RFC3339, m[2]); err != nil {
		return time.Time{}, time.Time{}, err
	}
	return start, stop, nil
}

// inRange narrows a counter to the readings inside the half-open [start, stop),
// which is what range() selects.
func (s counterSeries) inRange(start, stop time.Time) counterSeries {
	var out counterSeries
	for i, ts := range s.at {
		if !ts.Before(start) && ts.Before(stop) {
			out.at = append(out.at, ts)
			out.kwh = append(out.kwh, s.kwh[i])
		}
	}
	return out
}

// windowDeltas reproduces increase() |> aggregateWindow(every:, fn: last,
// timeSrc: "_start", location:, createEmpty: true) |> difference() over an
// already-range-narrowed counter.
//
// Each grid window collapses to the counter at its LAST reading; difference()
// then emits the rise between consecutive windows, consuming the first as its
// seed. A window with no readings is a null that difference() cannot emit
// against, so it is skipped and the next delta spans it — the same shape the
// database produces for a gap.
func (s counterSeries) windowDeltas(deviceID string, loc *time.Location, every time.Duration, stop time.Time) []Row {
	type windowClose struct {
		at  time.Time
		val float64
	}
	base := s.kwh[0] // increase() re-bases at the first reading in range

	var closes []windowClose
	for w := simGridStart(s.at[0], loc, every); w.Before(stop); w = simGridNext(w, loc, every) {
		next := simGridNext(w, loc, every)
		last := -1
		for i, ts := range s.at {
			if !ts.Before(w) && ts.Before(next) {
				last = i
			}
		}
		if last < 0 {
			continue
		}
		closes = append(closes, windowClose{at: w, val: s.kwh[last] - base})
	}

	rows := make([]Row, 0, len(closes))
	for i := 1; i < len(closes); i++ {
		rows = append(rows, Row{
			DeviceID: deviceID,
			Field:    "energy_kwh",
			Value:    closes[i].val - closes[i-1].val,
			Time:     closes[i].at, // timeSrc: "_start"
		})
	}
	return rows
}

// simGridStart snaps t DOWN to the aggregation grid, which Influx anchors at
// local midnight in the configured location rather than at the range start.
func simGridStart(t time.Time, loc *time.Location, every time.Duration) time.Time {
	lt := t.In(loc)
	midnight := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
	if every >= 24*time.Hour {
		return midnight
	}
	return lt.Add(-(lt.Sub(midnight) % every))
}

// simGridNext steps one window on. A daily window steps by CALENDAR day so a
// London day across a clock change is 23h or 25h, matching the DST-aware
// stretching aggregateWindow(location:) applies; sub-day windows step by their
// fixed duration.
func simGridNext(w time.Time, loc *time.Location, every time.Duration) time.Time {
	if every >= 24*time.Hour {
		lw := w.In(loc)
		return time.Date(lw.Year(), lw.Month(), lw.Day()+1, 0, 0, 0, 0, loc)
	}
	return w.Add(every)
}
