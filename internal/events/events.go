// Package events is countinghouse's read-side binary/event-timeline domain
// logic. It is the categorical counterpart to the quantitative energy series:
// it turns the per-device state transitions statehouse writes to the
// device_activity Influx measurement into ordered transition Events, sustained
// on/off Intervals, and summary Stats over an arbitrary window.
//
// The package is deliberately pure: every function takes its inputs explicitly
// (rows, the window start/stop, a location) and never calls time.Now() or
// touches Influx directly. BuildTimeline is the thin orchestrator that runs the
// activity + carry-in queries via a Querier and hands off to the pure
// functions; everything below it is unit-testable with fixtures.
//
// Decisions (documented per the M11 brief):
//
//   - Row.Text carries the string _value: device_activity's `from`/`to` fields
//     are strings, which the numeric influx.Row.Value cannot hold.
//   - Intervals are ON-STATE intervals only. The timeline's primary consumer is
//     "heating on" / "device active" shaded bands, so BuildIntervals emits one
//     interval per sustained ON span (state whose normalized On is true) and
//     drops OFF/idle spans. Each emitted interval still records its raw State
//     and On=true. This keeps ComputeStats (on_count, duty) trivial and the
//     /intervals response directly renderable.
//   - Normalization lives in one place (onStates) so new vocabularies extend
//     easily; raw From/To are always preserved on every Event regardless.
package events

import (
	"context"
	"sort"
	"time"

	"github.com/sweeney/countinghouse/internal/influx"
)

// onStates maps a raw device_activity state label to its normalized on/off
// meaning. Labels absent from the map are "unknown" (momentary events such as a
// doorbell "ring" or a fire-alarm "alert") and yield a nil On — they render as
// vertical-line overlays, never as shaded bands. Keep all known vocabulary here.
var onStates = map[string]bool{
	"active": true,
	"idle":   false,
}

// eventClasses are the device classes whose state transitions are exposed as
// on/off / state-change events (the device_activity series). Extend this set as
// new event-bearing classes appear (e.g. "doorbell"). Energy/sensor classes are
// deliberately excluded: their incidental active/idle activity is not a binary
// state worth overlaying.
var eventClasses = map[string]bool{
	"binary_state_device": true, // hot_water, central_heating
	"fire_alarm":          true, // firealarm_*
}

// IsEventClass reports whether a device class produces on/off / state-transition
// events surfaced by the events endpoints. Drives the "events" capability in the
// /devices catalog and the default device set for /events.
func IsEventClass(class string) bool { return eventClasses[class] }

// normalizeOn returns the normalized on/off state for a raw label: a pointer to
// true for an "on" label, to false for an "off" label, and nil when the label
// is not a known on/off state (a momentary/unknown event).
func normalizeOn(state string) *bool {
	v, ok := onStates[state]
	if !ok {
		return nil
	}
	return &v
}

// Event is a single state transition at a point in time. From/To are the raw
// device_activity labels (always preserved). On is the normalized state derived
// from To: true for an on-label, false for an off-label, nil when To is not a
// known on/off label (a momentary event such as a doorbell ring).
type Event struct {
	Time time.Time `json:"time"`
	From string    `json:"from"`
	To   string    `json:"to"`
	On   *bool     `json:"on,omitempty"`
}

// Interval is a sustained ON span. Start is the transition into the ON state
// (clamped to the window start for a carried-in span); End is the transition
// out of it (clamped to the window stop), or nil when the span is still active
// at the window stop, in which case Open is true. State is the raw label of the
// span and On is always true (BuildIntervals emits ON spans only). DurationS is
// the clamped span length in seconds.
type Interval struct {
	Start     time.Time  `json:"start"`
	End       *time.Time `json:"end"`
	State     string     `json:"state"`
	On        *bool      `json:"on,omitempty"`
	DurationS float64    `json:"duration_s"`
	Open      bool       `json:"open,omitempty"`
}

// Stats summarises a set of ON intervals over a window. OnCount is the number of
// ON intervals (including an open one). TotalOnSeconds is the sum of their
// window-clamped durations. Duty is TotalOnSeconds divided by the window length
// in seconds (0 for a zero-length window).
type Stats struct {
	OnCount        int     `json:"on_count"`
	TotalOnSeconds float64 `json:"total_on_seconds"`
	Duty           float64 `json:"duty"`
}

// EventsFromRows pairs the raw device_activity `from`/`to` field-rows into
// ordered Events. A transition writes both fields at the same timestamp, so
// rows are grouped by (device-agnostic) timestamp; each group becomes one Event
// carrying the from-label, the to-label and the normalized On derived from the
// to-label. The result is sorted ascending by time and rendered in loc.
//
// It is robust to a group with only one of the two fields present: the missing
// label is left empty and On is derived from whatever To is available.
func EventsFromRows(rows []influx.Row, loc *time.Location) []Event {
	if loc == nil {
		loc = time.UTC
	}

	// Group field-rows by their transition timestamp (UnixNano is exact and
	// hashable). A single transition contributes a "from" row and a "to" row at
	// the same instant.
	type pair struct {
		from, to string
		t        time.Time
	}
	byTime := make(map[int64]*pair)
	var order []int64
	for _, r := range rows {
		key := r.Time.UnixNano()
		p, ok := byTime[key]
		if !ok {
			p = &pair{t: r.Time}
			byTime[key] = p
			order = append(order, key)
		}
		switch r.Field {
		case "from":
			p.from = r.Text
		case "to":
			p.to = r.Text
		}
	}

	events := make([]Event, 0, len(order))
	for _, key := range order {
		p := byTime[key]
		events = append(events, Event{
			Time: p.t.In(loc),
			From: p.from,
			To:   p.to,
			On:   normalizeOn(p.to),
		})
	}
	sort.Slice(events, func(i, j int) bool {
		return events[i].Time.Before(events[j].Time)
	})
	return events
}

// BuildIntervals pairs consecutive transitions into sustained ON spans over the
// half-open window [start, stop). It emits ON-state intervals only (see package
// doc): every returned Interval has On=true and represents a span the device
// was in an on-label state.
//
// stateAtStart is the carry-in: the raw `to` label of the last transition
// BEFORE start ("" if none). If it normalizes to ON, the first interval opens
// at start (clamped) and is closed by the first in-window transition out of the
// ON state. Spans are clamped to the window: a Start before start becomes
// start, and a span still ON at stop has End=nil, Open=true and DurationS
// measured to stop.
//
// It returns the intervals and the raw state the device is in at stop (the
// to-label of the last event, or stateAtStart when no events occurred), which
// callers may surface or chain.
func BuildIntervals(events []Event, stateAtStart string, start, stop time.Time, loc *time.Location) ([]Interval, string) {
	if loc == nil {
		loc = time.UTC
	}
	start = start.In(loc)
	stop = stop.In(loc)

	var intervals []Interval

	// curState is the raw label the device is currently in; openFrom is the
	// (clamped) instant the current ON span began, valid only while in an ON
	// state. We walk transitions in order, opening an ON span on entry and
	// closing it on exit.
	curState := stateAtStart
	var openFrom time.Time
	// openState is the ON-state label captured when a span opens, so the closed
	// interval reports the state active DURING the span — not the OFF label of
	// the transition that closes it.
	var openState string
	onOpen := false

	openIfOn := func(label string, at time.Time) {
		if on := normalizeOn(label); on != nil && *on {
			if at.Before(start) {
				at = start
			}
			openFrom = at
			openState = label
			onOpen = true
		}
	}
	closeAt := func(at time.Time) {
		if !onOpen {
			return
		}
		if at.After(stop) {
			at = stop
		}
		end := at
		intervals = append(intervals, Interval{
			Start:     openFrom,
			End:       &end,
			State:     openState,
			On:        boolPtr(true),
			DurationS: end.Sub(openFrom).Seconds(),
		})
		onOpen = false
	}

	// Carry-in: if the device entered the window already ON, open a span at the
	// window start.
	openIfOn(curState, start)

	for _, ev := range events {
		// A transition closes any open ON span (the device left the ON state)
		// and may open a new one if the to-label is itself ON. We compare the
		// to-label's on-ness rather than just closing/opening blindly so a
		// transition between two on-labels (rare) does not spuriously split.
		toOn := normalizeOn(ev.To)
		switch {
		case onOpen && (toOn == nil || !*toOn):
			// Leaving ON.
			curState = ev.To
			closeAt(ev.Time)
		case !onOpen && toOn != nil && *toOn:
			// Entering ON.
			curState = ev.To
			openIfOn(ev.To, ev.Time)
		default:
			// Same on/off-ness (e.g. on->on or off->off): just track the
			// latest label so State reflects reality at close, without
			// splitting the span.
			curState = ev.To
		}
	}

	// A span still ON at the window end is left open.
	if onOpen {
		dur := stop.Sub(openFrom).Seconds()
		intervals = append(intervals, Interval{
			Start:     openFrom,
			End:       nil,
			State:     openState,
			On:        boolPtr(true),
			DurationS: dur,
			Open:      true,
		})
	}

	return intervals, curState
}

// ComputeStats summarises ON intervals over the half-open window [start, stop).
// OnCount counts ON intervals (including an open one). TotalOnSeconds sums their
// durations, each clamped to the window. Duty is TotalOnSeconds / window-seconds
// and is 0 when the window has zero (or negative) length.
func ComputeStats(intervals []Interval, start, stop time.Time) Stats {
	var s Stats
	for _, iv := range intervals {
		if iv.On == nil || !*iv.On {
			continue
		}
		s.OnCount++

		ivStart := iv.Start
		if ivStart.Before(start) {
			ivStart = start
		}
		var ivEnd time.Time
		if iv.End != nil {
			ivEnd = *iv.End
		} else {
			ivEnd = stop
		}
		if ivEnd.After(stop) {
			ivEnd = stop
		}
		if d := ivEnd.Sub(ivStart).Seconds(); d > 0 {
			s.TotalOnSeconds += d
		}
	}

	windowSeconds := stop.Sub(start).Seconds()
	if windowSeconds > 0 {
		s.Duty = s.TotalOnSeconds / windowSeconds
	}
	return s
}

// boolPtr returns a pointer to v. Kept local so the package has no test-util
// dependency in production code.
func boolPtr(v bool) *bool { return &v }

// Timeline is the fully-derived per-device result: ordered Events, ON
// Intervals, summary Stats, and the carry-in StateAtStart (the raw state the
// device was in as the window opened, "" if unknown).
type Timeline struct {
	Events       []Event    `json:"events"`
	Intervals    []Interval `json:"intervals"`
	Stats        Stats      `json:"stats"`
	StateAtStart string     `json:"state_at_start"`
}

// BuildTimeline is the thin orchestrator M12's HTTP handlers call. Given a
// Querier, bucket, device set and window, it runs two queries — the in-window
// activity query and the carry-in (last transition before start) query — demuxes
// the rows per device, and applies the pure EventsFromRows / BuildIntervals /
// ComputeStats pipeline. It returns one Timeline per requested device id
// (devices with no activity still get an entry with empty events/intervals and
// their carried-in StateAtStart, so consumers can render an off-baseline).
//
// All Influx access is confined here; the per-device computation is pure and
// independently tested.
func BuildTimeline(ctx context.Context, q influx.Querier, bucket string, deviceIDs []string, start, stop time.Time, loc *time.Location) (map[string]Timeline, error) {
	if loc == nil {
		loc = time.UTC
	}

	activityRows, err := q.Query(ctx, influx.BuildActivityFlux(bucket, deviceIDs, start, stop))
	if err != nil {
		return nil, err
	}
	carryRows, err := q.Query(ctx, influx.BuildActivityCarryInFlux(bucket, deviceIDs, start))
	if err != nil {
		return nil, err
	}

	// Demux rows per device.
	rowsByDevice := make(map[string][]influx.Row, len(deviceIDs))
	for _, r := range activityRows {
		rowsByDevice[r.DeviceID] = append(rowsByDevice[r.DeviceID], r)
	}

	// The carry-in query yields the last `from`/`to` rows per device; the `to`
	// label is the state the device was in at window start.
	carryByDevice := make(map[string]string, len(deviceIDs))
	for _, r := range carryRows {
		if r.Field == "to" {
			carryByDevice[r.DeviceID] = r.Text
		}
	}

	out := make(map[string]Timeline, len(deviceIDs))
	for _, id := range deviceIDs {
		evs := EventsFromRows(rowsByDevice[id], loc)
		stateAtStart := carryByDevice[id]
		intervals, _ := BuildIntervals(evs, stateAtStart, start, stop, loc)
		stats := ComputeStats(intervals, start.In(loc), stop.In(loc))
		out[id] = Timeline{
			Events:       evs,
			Intervals:    intervals,
			Stats:        stats,
			StateAtStart: stateAtStart,
		}
	}
	return out, nil
}
