package events

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/influx"
)

// london is the production timezone; used where tz rendering / DST matters.
func london(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("load Europe/London: %v", err)
	}
	return loc
}

// transition builds the two raw field-rows a single device_activity edge writes
// (from and to at the same instant).
func transition(deviceID string, at time.Time, from, to string) []influx.Row {
	return []influx.Row{
		{DeviceID: deviceID, Class: "binary", Field: "from", Text: from, Time: at},
		{DeviceID: deviceID, Class: "binary", Field: "to", Text: to, Time: at},
	}
}

func TestNormalizeOn(t *testing.T) {
	cases := []struct {
		state string
		want  *bool
	}{
		{"active", boolPtr(true)},
		{"idle", boolPtr(false)},
		{"ring", nil},
		{"", nil},
		{"unknown", nil},
	}
	for _, c := range cases {
		got := normalizeOn(c.state)
		switch {
		case c.want == nil && got != nil:
			t.Errorf("normalizeOn(%q) = %v, want nil", c.state, *got)
		case c.want != nil && got == nil:
			t.Errorf("normalizeOn(%q) = nil, want %v", c.state, *c.want)
		case c.want != nil && got != nil && *got != *c.want:
			t.Errorf("normalizeOn(%q) = %v, want %v", c.state, *got, *c.want)
		}
	}
}

func TestEventsFromRowsPairsAndOrders(t *testing.T) {
	loc := london(t)
	t1 := time.Date(2026, 6, 11, 5, 30, 1, 0, time.UTC)
	t2 := time.Date(2026, 6, 11, 6, 15, 0, 0, time.UTC)

	// Supply rows out of order to prove sorting.
	var rows []influx.Row
	rows = append(rows, transition("hot_water", t2, "active", "idle")...)
	rows = append(rows, transition("hot_water", t1, "idle", "active")...)

	evs := EventsFromRows(rows, loc)
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2", len(evs))
	}
	if !evs[0].Time.Before(evs[1].Time) {
		t.Fatalf("events not ordered: %v then %v", evs[0].Time, evs[1].Time)
	}
	if evs[0].From != "idle" || evs[0].To != "active" || evs[0].On == nil || !*evs[0].On {
		t.Errorf("event 0 = %+v, want idle->active on=true", evs[0])
	}
	if evs[1].From != "active" || evs[1].To != "idle" || evs[1].On == nil || *evs[1].On {
		t.Errorf("event 1 = %+v, want active->idle on=false", evs[1])
	}
	// Rendered in loc (BST = +01:00).
	if name, _ := evs[0].Time.Zone(); name != "BST" {
		t.Errorf("event time not rendered in London tz: zone=%s", name)
	}
}

func TestEventsFromRowsUnknownLabel(t *testing.T) {
	at := time.Date(2026, 6, 11, 9, 0, 0, 0, time.UTC)
	rows := transition("doorbell", at, "idle", "ring")
	evs := EventsFromRows(rows, time.UTC)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if evs[0].To != "ring" {
		t.Errorf("raw To not preserved: %+v", evs[0])
	}
	if evs[0].On != nil {
		t.Errorf("unknown label should have nil On, got %v", *evs[0].On)
	}
}

func TestEventsFromRowsPartialPair(t *testing.T) {
	at := time.Date(2026, 6, 11, 9, 0, 0, 0, time.UTC)
	// Only a `to` row present (from dropped) — should still yield an Event.
	rows := []influx.Row{{DeviceID: "x", Field: "to", Text: "active", Time: at}}
	evs := EventsFromRows(rows, time.UTC)
	if len(evs) != 1 || evs[0].To != "active" || evs[0].From != "" {
		t.Fatalf("partial pair handling wrong: %+v", evs)
	}
	if evs[0].On == nil || !*evs[0].On {
		t.Errorf("On should derive from to-only: %+v", evs[0])
	}
}

func TestBuildIntervalsNormalOnOff(t *testing.T) {
	loc := london(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 23, 59, 59, 0, loc)

	on := time.Date(2026, 6, 11, 5, 30, 1, 0, loc)
	off := time.Date(2026, 6, 11, 6, 15, 0, 0, loc)
	evs := []Event{
		{Time: on, From: "idle", To: "active", On: boolPtr(true)},
		{Time: off, From: "active", To: "idle", On: boolPtr(false)},
	}

	ivs, endState := BuildIntervals(evs, "idle", start, stop, loc)
	if len(ivs) != 1 {
		t.Fatalf("intervals = %d, want 1", len(ivs))
	}
	iv := ivs[0]
	if !iv.Start.Equal(on) {
		t.Errorf("start = %v, want %v", iv.Start, on)
	}
	if iv.End == nil || !iv.End.Equal(off) {
		t.Errorf("end = %v, want %v", iv.End, off)
	}
	if iv.Open {
		t.Errorf("interval should be closed")
	}
	wantDur := off.Sub(on).Seconds()
	if iv.DurationS != wantDur {
		t.Errorf("duration = %v, want %v", iv.DurationS, wantDur)
	}
	if iv.On == nil || !*iv.On {
		t.Errorf("interval On should be true")
	}
	if endState != "idle" {
		t.Errorf("end state = %q, want idle", endState)
	}
}

func TestBuildIntervalsCarryInOn(t *testing.T) {
	loc := london(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 12, 0, 0, 0, loc)

	// Device was already ON at window start; turns off at 06:00.
	off := time.Date(2026, 6, 11, 6, 0, 0, 0, loc)
	evs := []Event{
		{Time: off, From: "active", To: "idle", On: boolPtr(false)},
	}

	ivs, _ := BuildIntervals(evs, "active", start, stop, loc)
	if len(ivs) != 1 {
		t.Fatalf("intervals = %d, want 1", len(ivs))
	}
	if !ivs[0].Start.Equal(start) {
		t.Errorf("carried-in interval should open at window start; start = %v", ivs[0].Start)
	}
	if ivs[0].End == nil || !ivs[0].End.Equal(off) {
		t.Errorf("end = %v, want %v", ivs[0].End, off)
	}
	if ivs[0].DurationS != off.Sub(start).Seconds() {
		t.Errorf("duration wrong: %v", ivs[0].DurationS)
	}
}

func TestBuildIntervalsOpenSpan(t *testing.T) {
	loc := london(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 12, 0, 0, 0, loc)

	on := time.Date(2026, 6, 11, 11, 0, 0, 0, loc)
	evs := []Event{
		{Time: on, From: "idle", To: "active", On: boolPtr(true)},
	}

	ivs, endState := BuildIntervals(evs, "idle", start, stop, loc)
	if len(ivs) != 1 {
		t.Fatalf("intervals = %d, want 1", len(ivs))
	}
	if ivs[0].End != nil {
		t.Errorf("open interval should have nil End, got %v", *ivs[0].End)
	}
	if !ivs[0].Open {
		t.Errorf("interval should be Open")
	}
	if ivs[0].DurationS != stop.Sub(on).Seconds() {
		t.Errorf("open duration = %v, want to stop %v", ivs[0].DurationS, stop.Sub(on).Seconds())
	}
	if endState != "active" {
		t.Errorf("end state = %q, want active", endState)
	}
}

func TestBuildIntervalsClampsStart(t *testing.T) {
	loc := london(t)
	start := time.Date(2026, 6, 11, 8, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 12, 0, 0, 0, loc)

	// On transition is BEFORE the window start (e.g. stray row); clamp to start.
	on := time.Date(2026, 6, 11, 7, 0, 0, 0, loc)
	off := time.Date(2026, 6, 11, 9, 0, 0, 0, loc)
	evs := []Event{
		{Time: on, From: "idle", To: "active", On: boolPtr(true)},
		{Time: off, From: "active", To: "idle", On: boolPtr(false)},
	}
	ivs, _ := BuildIntervals(evs, "idle", start, stop, loc)
	if len(ivs) != 1 {
		t.Fatalf("intervals = %d, want 1", len(ivs))
	}
	if !ivs[0].Start.Equal(start) {
		t.Errorf("start should clamp to window start, got %v", ivs[0].Start)
	}
}

func TestBuildIntervalsNoEventsOff(t *testing.T) {
	loc := london(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 12, 0, 0, 0, loc)

	ivs, endState := BuildIntervals(nil, "idle", start, stop, loc)
	if len(ivs) != 0 {
		t.Fatalf("intervals = %d, want 0 (device off all window)", len(ivs))
	}
	if endState != "idle" {
		t.Errorf("end state = %q, want idle", endState)
	}
}

func TestBuildIntervalsNoEventsOn(t *testing.T) {
	loc := london(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 12, 0, 0, 0, loc)

	ivs, endState := BuildIntervals(nil, "active", start, stop, loc)
	if len(ivs) != 1 {
		t.Fatalf("intervals = %d, want 1 (on all window)", len(ivs))
	}
	if !ivs[0].Start.Equal(start) || ivs[0].End != nil || !ivs[0].Open {
		t.Errorf("on-all-window interval wrong: %+v", ivs[0])
	}
	if ivs[0].DurationS != stop.Sub(start).Seconds() {
		t.Errorf("duration = %v, want full window", ivs[0].DurationS)
	}
	if endState != "active" {
		t.Errorf("end state = %q, want active", endState)
	}
}

// Regression: a closed ON interval's State must be the ON-state label active
// during the span (e.g. "active"), not the OFF label of the closing transition.
// Live data showed state:"idle" with on:true.
func TestBuildIntervals_StateIsOnLabel(t *testing.T) {
	loc := london(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 23, 0, 0, 0, loc)
	evs := []Event{
		{Time: time.Date(2026, 6, 11, 6, 30, 0, 0, loc), From: "idle", To: "active"},
		{Time: time.Date(2026, 6, 11, 7, 15, 0, 0, loc), From: "active", To: "idle"},
	}
	ivs, _ := BuildIntervals(evs, "idle", start, stop, loc)
	if len(ivs) != 1 {
		t.Fatalf("want 1 interval, got %d", len(ivs))
	}
	if ivs[0].State != "active" {
		t.Errorf("interval state = %q, want %q (the ON-state label)", ivs[0].State, "active")
	}
	if ivs[0].On == nil || !*ivs[0].On {
		t.Errorf("interval On = %v, want true", ivs[0].On)
	}
}

func TestComputeStats(t *testing.T) {
	loc := london(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 0, 0, 0, 0, loc).Add(24 * time.Hour)

	mk := func(h0, m0, h1, m1 int) Interval {
		s := time.Date(2026, 6, 11, h0, m0, 0, 0, loc)
		e := time.Date(2026, 6, 11, h1, m1, 0, 0, loc)
		return Interval{Start: s, End: &e, On: boolPtr(true), DurationS: e.Sub(s).Seconds()}
	}

	openIv := Interval{
		Start: time.Date(2026, 6, 11, 23, 0, 0, 0, loc),
		End:   nil, Open: true, On: boolPtr(true),
	}
	ivs := []Interval{mk(5, 30, 6, 15), openIv}

	s := ComputeStats(ivs, start, stop)
	if s.OnCount != 2 {
		t.Errorf("on_count = %d, want 2", s.OnCount)
	}
	// 45m closed + 1h open (23:00->24:00) = 2700 + 3600 = 6300.
	wantTotal := 45.0*60 + 60.0*60
	if s.TotalOnSeconds != wantTotal {
		t.Errorf("total_on_seconds = %v, want %v", s.TotalOnSeconds, wantTotal)
	}
	wantDuty := wantTotal / (24 * 3600)
	if s.Duty != wantDuty {
		t.Errorf("duty = %v, want %v", s.Duty, wantDuty)
	}
}

func TestComputeStatsZeroWindow(t *testing.T) {
	loc := london(t)
	at := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	ivs := []Interval{{Start: at, End: &at, On: boolPtr(true)}}
	s := ComputeStats(ivs, at, at) // zero-length window
	if s.Duty != 0 {
		t.Errorf("duty should be 0 for zero-length window, got %v", s.Duty)
	}
}

func TestComputeStatsClampsToWindow(t *testing.T) {
	loc := london(t)
	start := time.Date(2026, 6, 11, 8, 0, 0, 0, loc)
	stop := time.Date(2026, 6, 11, 10, 0, 0, 0, loc)

	// Interval spills out both ends; only the in-window 2h should count.
	s0 := time.Date(2026, 6, 11, 7, 0, 0, 0, loc)
	e0 := time.Date(2026, 6, 11, 11, 0, 0, 0, loc)
	ivs := []Interval{{Start: s0, End: &e0, On: boolPtr(true), DurationS: e0.Sub(s0).Seconds()}}

	s := ComputeStats(ivs, start, stop)
	if s.TotalOnSeconds != 2*3600 {
		t.Errorf("clamped total = %v, want 7200", s.TotalOnSeconds)
	}
	if s.Duty != 1.0 {
		t.Errorf("duty = %v, want 1.0 (fully on across window)", s.Duty)
	}
}

func TestBuildTimelineMultiDevice(t *testing.T) {
	loc := london(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc).UTC()
	stop := time.Date(2026, 6, 11, 23, 0, 0, 0, loc).UTC()

	hwOn := time.Date(2026, 6, 11, 5, 30, 1, 0, time.UTC)
	hwOff := time.Date(2026, 6, 11, 6, 15, 0, 0, time.UTC)
	boilerOn := time.Date(2026, 6, 11, 16, 0, 0, 0, time.UTC)

	var activity []influx.Row
	activity = append(activity, transition("hot_water", hwOn, "idle", "active")...)
	activity = append(activity, transition("hot_water", hwOff, "active", "idle")...)
	activity = append(activity, transition("boiler", boilerOn, "idle", "active")...)

	// Carry-in: hot_water was idle before the window, boiler was active.
	carry := []influx.Row{
		{DeviceID: "hot_water", Field: "from", Text: "active", Time: start.Add(-time.Hour)},
		{DeviceID: "hot_water", Field: "to", Text: "idle", Time: start.Add(-time.Hour)},
		{DeviceID: "boiler", Field: "from", Text: "idle", Time: start.Add(-2 * time.Hour)},
		{DeviceID: "boiler", Field: "to", Text: "active", Time: start.Add(-2 * time.Hour)},
	}

	q := &influx.FakeQuerier{
		QueryFunc: func(flux string) ([]influx.Row, error) {
			if strings.Contains(flux, "1970-01-01") {
				return carry, nil
			}
			return activity, nil
		},
	}

	out, err := BuildTimeline(context.Background(), q, "statehouse", []string{"hot_water", "boiler"}, start, stop, loc)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("timelines = %d, want 2", len(out))
	}

	hw := out["hot_water"]
	if hw.StateAtStart != "idle" {
		t.Errorf("hot_water state_at_start = %q, want idle", hw.StateAtStart)
	}
	if len(hw.Events) != 2 {
		t.Errorf("hot_water events = %d, want 2", len(hw.Events))
	}
	if len(hw.Intervals) != 1 || hw.Intervals[0].Open {
		t.Errorf("hot_water should have 1 closed interval, got %+v", hw.Intervals)
	}
	if hw.Stats.OnCount != 1 {
		t.Errorf("hot_water on_count = %d, want 1", hw.Stats.OnCount)
	}

	b := out["boiler"]
	if b.StateAtStart != "active" {
		t.Errorf("boiler state_at_start = %q, want active", b.StateAtStart)
	}
	// Carried in active, never turned off in-window, then turned active again
	// (no-op): one open interval spanning the whole window.
	if len(b.Intervals) != 1 || !b.Intervals[0].Open {
		t.Errorf("boiler should have 1 open interval, got %+v", b.Intervals)
	}
	if !b.Intervals[0].Start.Equal(start.In(loc)) {
		t.Errorf("boiler open interval should start at window start, got %v", b.Intervals[0].Start)
	}
}

func TestBuildTimelineEmptyDevice(t *testing.T) {
	loc := london(t)
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	stop := time.Date(2026, 6, 11, 23, 0, 0, 0, time.UTC)

	q := &influx.FakeQuerier{} // no responses -> empty rows
	out, err := BuildTimeline(context.Background(), q, "statehouse", []string{"ghost"}, start, stop, loc)
	if err != nil {
		t.Fatalf("BuildTimeline: %v", err)
	}
	tl, ok := out["ghost"]
	if !ok {
		t.Fatal("device with no activity should still get a timeline entry")
	}
	if len(tl.Events) != 0 || len(tl.Intervals) != 0 || tl.StateAtStart != "" {
		t.Errorf("empty device timeline malformed: %+v", tl)
	}
	if len(q.Queries) != 2 {
		t.Errorf("BuildTimeline should run 2 queries (activity + carry-in), ran %d", len(q.Queries))
	}
}
