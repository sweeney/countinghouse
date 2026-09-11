package influx

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// PowerSim is CounterSim's opposite number for the power-only path: a
// deterministic stand-in for InfluxDB over the instantaneous power_w field,
// answering the three Flux shapes countinghouse issues against it.
//
// It exists for the same reason CounterSim does. /series and
// /devices/{id}/energy are supposed to describe the same electricity, and an
// assertion that they agree is worth something only if both read ONE underlying
// telemetry stream through ONE model of the query engine. For a ups_sensor that
// matters more than for a counter, not less: a UPS has no counter to read, so
// BOTH endpoints are estimating an integral, and what used to separate them was
// that they estimated it differently (issue #32).
//
// Shapes answered:
//
//   - integral(unit: 1h, interpolate: "linear") |> map(/1000) — BuildIntegralFlux,
//     the whole-window reduction behind /devices/{id}/energy: one row per device,
//     kWh over the range.
//   - aggregateWindow(fn: integral-lambda) |> map(/1000) — BuildPowerIntegralSeriesFlux,
//     the UPS energy series: one row per grid window that HAS a sample, stamped
//     at the window's left edge truncated to the range, carrying that window's
//     kWh. A window with no sample is omitted (createEmpty: false).
//   - aggregateWindow(fn: mean) — BuildPowerMeanSeriesFlux, the avg_w series: the
//     SAMPLE mean per window, emitted for every window because createEmpty is
//     true — as a null (Row.Null) where there is nothing to average.
//
// # What this models, and what it does not
//
// integral() is modelled as the trapezoid rule across the samples inside the
// table, plus a FLAT HOLD from the table's lower bound to the first sample and
// from the last sample to its upper bound. The flat hold is the sim's reading of
// the behaviour countinghouse observed against real Influx during issue #17,
// where three tag-fragments of one device were each "extrapolated across the
// whole window" and summed to 3x the real energy: integral() takes its bounds
// from _start/_stop and fills to them rather than integrating only between the
// samples it can see.
//
// That is a MODEL of Flux's edge arithmetic, not a transcription of it, and it
// is the one part of this sim that a reviewer should not take on trust. The
// shapes, the bucketing, the omission of empty windows and the kWh conversion
// are all pinned against the real builders by power_sim_test.go; the exact value
// Flux interpolates at a window bound is not something a fake can establish.
// Tests here therefore use STEADY loads wherever they compare the two endpoints,
// because a steady load makes every plausible edge rule agree — which is also
// the load the two real UPSs actually carry.
//
// Scope: power_w only. A query for any other field (energy_kwh) returns no rows,
// so a test needing a counter alongside should compose this with CounterSim.
type PowerSim struct {
	// Loc is the timezone whose local midnight anchors the aggregation grid,
	// matching aggregateWindow(location: timezone.location(...)). Defaults to
	// UTC when nil.
	Loc *time.Location

	ids    []string // registration order, so rows are returned deterministically
	series map[string]powerSeries
}

// powerSeries is one device's instantaneous power samples, ascending by time.
type powerSeries struct {
	at []time.Time
	w  []float64
}

// NewPowerSim returns an empty sim whose aggregation grid is anchored at local
// midnight in loc.
func NewPowerSim(loc *time.Location) *PowerSim {
	return &PowerSim{Loc: loc, series: map[string]powerSeries{}}
}

// AddSteady registers deviceID drawing a constant `watts`, sampled every
// `cadence` across [from, to] inclusive. It returns the sim so registrations can
// be chained.
func (p *PowerSim) AddSteady(deviceID string, from, to time.Time, cadence time.Duration, watts float64) *PowerSim {
	var at []time.Time
	var w []float64
	for ts := from; !ts.After(to); ts = ts.Add(cadence) {
		at = append(at, ts)
		w = append(w, watts)
	}
	return p.AddSamples(deviceID, at, w)
}

// AddSteadyWithGaps is AddSteady with the given half-open intervals omitted
// entirely — a UPS that dropped off the network while, presumably, continuing to
// draw exactly as before.
func (p *PowerSim) AddSteadyWithGaps(deviceID string, from, to time.Time, cadence time.Duration, watts float64, gaps ...[2]time.Time) *PowerSim {
	var at []time.Time
	var w []float64
	for ts := from; !ts.After(to); ts = ts.Add(cadence) {
		skip := false
		for _, g := range gaps {
			if !ts.Before(g[0]) && ts.Before(g[1]) {
				skip = true
			}
		}
		if skip {
			continue
		}
		at = append(at, ts)
		w = append(w, watts)
	}
	return p.AddSamples(deviceID, at, w)
}

// AddSamples registers deviceID with explicit instantaneous readings. at must be
// ascending. It returns the sim so registrations can be chained.
//
// Mismatched slice lengths panic here, naming the call site: the alternative is
// an index panic later, from inside a query, pointing at the sim rather than at
// the test that mis-built the fixture.
func (p *PowerSim) AddSamples(deviceID string, at []time.Time, watts []float64) *PowerSim {
	if len(at) != len(watts) {
		panic(fmt.Sprintf("influx: PowerSim.AddSamples(%q): %d times but %d readings",
			deviceID, len(at), len(watts)))
	}
	if p.series == nil {
		p.series = map[string]powerSeries{}
	}
	if _, seen := p.series[deviceID]; !seen {
		p.ids = append(p.ids, deviceID)
	}
	p.series[deviceID] = powerSeries{at: at, w: watts}
	return p
}

// Query implements Querier.
func (p *PowerSim) Query(_ context.Context, flux string) ([]Row, error) {
	return p.Answer(flux)
}

// Ping implements Querier; the sim is always reachable.
func (p *PowerSim) Ping(context.Context) bool { return true }

// Answer resolves one Flux script against the registered samples. Its signature
// matches FakeQuerier.QueryFunc, so a test wanting FakeQuerier's query recording
// can wire it in as `&FakeQuerier{QueryFunc: sim.Answer}`.
func (p *PowerSim) Answer(flux string) ([]Row, error) {
	if !strings.Contains(flux, `"power_w"`) {
		return nil, nil // not this sim's field
	}
	start, stop, err := simRange(flux)
	if err != nil {
		return nil, err
	}

	bucketed := strings.Contains(flux, "aggregateWindow")
	integrating := strings.Contains(flux, "integral(")

	var every time.Duration
	if bucketed {
		m := simEveryRe.FindStringSubmatch(flux)
		if m == nil {
			return nil, fmt.Errorf("influx: PowerSim: aggregateWindow with no every: in flux:\n%s", flux)
		}
		d, ok := intervalDuration(m[1])
		if !ok {
			return nil, fmt.Errorf("influx: PowerSim: unsupported interval %q", m[1])
		}
		every = d
	}

	var rows []Row
	for _, id := range p.ids {
		// Both the single-device predicate (r.device_id == "id") and the set form
		// (contains(..., set: ["id", ...])) embed the id quoted.
		if !strings.Contains(flux, `"`+id+`"`) {
			continue
		}
		in := p.series[id].inRange(start, stop)

		if !bucketed {
			// Whole-window integral: one row per device, kWh over the range. No
			// samples means no row at all — an empty result, not a zero.
			if len(in.at) == 0 {
				continue
			}
			rows = append(rows, Row{
				DeviceID: id,
				Field:    "power_w",
				Value:    in.integrateKWh(start, stop),
				Time:     in.at[len(in.at)-1],
			})
			continue
		}
		rows = append(rows, in.windows(id, p.location(), every, start, stop, integrating)...)
	}
	return rows, nil
}

func (p *PowerSim) location() *time.Location {
	if p.Loc == nil {
		return time.UTC
	}
	return p.Loc
}

// inRange narrows a series to the samples inside the half-open [start, stop),
// which is what range() selects.
func (s powerSeries) inRange(start, stop time.Time) powerSeries {
	var out powerSeries
	for i, ts := range s.at {
		if !ts.Before(start) && ts.Before(stop) {
			out.at = append(out.at, ts)
			out.w = append(out.w, s.w[i])
		}
	}
	return out
}

// integrateKWh is integral(unit: 1h, interpolate: "linear") followed by the /1000
// map: the trapezoid rule across the samples, flat-held out to [lo, hi).
//
// See the type comment on PowerSim for why the flat hold is a model rather than
// a transcription, and why the comparisons that matter use steady loads.
func (s powerSeries) integrateKWh(lo, hi time.Time) float64 {
	if len(s.at) == 0 {
		return 0
	}
	var wh float64
	// Flat hold from the lower bound to the first sample.
	wh += s.w[0] * s.at[0].Sub(lo).Hours()
	// Trapezoid across consecutive samples.
	for i := 0; i+1 < len(s.at); i++ {
		wh += (s.w[i] + s.w[i+1]) / 2 * s.at[i+1].Sub(s.at[i]).Hours()
	}
	// Flat hold from the last sample to the upper bound.
	wh += s.w[len(s.w)-1] * hi.Sub(s.at[len(s.at)-1]).Hours()
	return wh / 1000.0
}

// mean is aggregateWindow(fn: mean): the arithmetic mean of the samples,
// weighting each equally regardless of how long it stood. Reporting false for an
// empty window is what lets the caller emit a null instead of a zero.
func (s powerSeries) mean() (float64, bool) {
	if len(s.w) == 0 {
		return 0, false
	}
	var sum float64
	for _, v := range s.w {
		sum += v
	}
	return sum / float64(len(s.w)), true
}

// windows walks the local aggregation grid across [start, stop) and reduces each
// window, reproducing aggregateWindow for whichever fn the flux asked for.
//
// Stamps are the window's _start TRUNCATED TO THE RANGE, exactly as CounterSim
// does and as Influx does: a window the range opens part-way through reports the
// range start, so an off-grid `from` yields a first row the caller must resolve
// by containment rather than by exact match.
//
// The integral variant omits an empty window (createEmpty: false); the mean
// variant emits it as a NULL row (createEmpty: true), which is the shape that
// used to decode to a plausible-looking 0 W.
func (s powerSeries) windows(deviceID string, loc *time.Location, every time.Duration, start, stop time.Time, integrating bool) []Row {
	var rows []Row
	for w := simGridStart(start, loc, every); w.Before(stop); w = simGridNext(w, loc, every) {
		next := simGridNext(w, loc, every)

		// Window bounds are truncated to the range at BOTH ends, which is what
		// makes the first and last windows partial.
		lo, hi := w, next
		if lo.Before(start) {
			lo = start
		}
		if hi.After(stop) {
			hi = stop
		}
		in := s.inRange(lo, hi)

		row := Row{DeviceID: deviceID, Field: "power_w", Time: lo}
		if integrating {
			if len(in.at) == 0 {
				continue // nothing to integrate: createEmpty is false
			}
			row.Value = in.integrateKWh(lo, hi)
			rows = append(rows, row)
			continue
		}
		v, ok := in.mean()
		if !ok {
			row.Null = true // createEmpty: true emits the window as a null
		}
		row.Value = v
		rows = append(rows, row)
	}
	return rows
}
