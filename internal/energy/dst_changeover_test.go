package energy

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/influx"
)

// ---------------------------------------------------------------------------
// /series across BOTH London clock changes, at every interval (issue #49).
//
// dst_fallback_test.go reproduces the bug as prod saw it. This file pins the
// shape of the fix, and the invariant the issue asked for: across a clock
// change, a continuously-reporting steady load has NO zero bucket and every
// bucket holds exactly as much energy as its real length says it should.
//
//   - autumn: 2026-10-25, 02:00 BST → 01:00 GMT. The local day is 25h.
//   - spring: 2027-03-28, 01:00 GMT → 02:00 BST. The local day is 23h.
//
// The sims model the grid the FLUX asks for (UTC fixed-step for sub-day,
// calendar days for 1d), and refuse a sub-day location-aware query outright,
// because that is the shape prod Influx stretches across the repeated hour and
// that the sims no longer pretend to model (see influx.simWindowing). So any
// regression that reintroduces location: on a sub-day query fails here loudly
// rather than passing against a sim that agrees with the bug.
// ---------------------------------------------------------------------------

// changeover is one London clock-change day.
type changeover struct {
	name  string
	day   time.Time // local midnight starting the changeover day
	hours float64   // that local day's real length
}

func changeovers(t *testing.T) []changeover {
	loc := mustLondon(t)
	return []changeover{
		{name: "autumn", day: time.Date(2026, 10, 25, 0, 0, 0, 0, loc), hours: 25},
		{name: "spring", day: time.Date(2027, 3, 28, 0, 0, 0, 0, loc), hours: 23},
	}
}

// fluxEveryRe pulls the aggregateWindow width out of a query.
var fluxEveryRe = regexp.MustCompile(`every: (\w+),`)

// TestSeriesAcrossClockChangesHasNoMisplacedEnergy is the issue's invariant
// test (its suggested-fix item 5), swept across both changes, every allowed
// interval, both value paths (counter and UPS integral) and both cost axes
// (flat pricer: queried at the display interval; half-hourly pricer: queried
// at 30m and folded).
func TestSeriesAcrossClockChangesHasNoMisplacedEnergy(t *testing.T) {
	const watts = 1000.0
	loc := mustLondon(t)

	for _, co := range changeovers(t) {
		next := time.Date(co.day.Year(), co.day.Month(), co.day.Day()+1, 0, 0, 0, 0, loc)
		if got := next.Sub(co.day).Hours(); got != co.hours {
			t.Fatalf("%s: tzdata says the day is %vh, test expects %vh", co.name, got, co.hours)
		}
		win := Window{Start: co.day, Stop: next, Label: WindowCustom}

		// Readings run an hour either side so the window's edges are interior.
		counters := influx.NewCounterSim().
			AddSteady("plug", co.day.Add(-time.Hour), next.Add(time.Hour), time.Minute, watts)
		powers := influx.NewPowerSim().
			AddSteady("ups", co.day.Add(-time.Hour), next.Add(time.Hour), time.Minute, watts)
		devices := map[string]config.DeviceConfig{
			"plug": {Class: "continuous_power_device"},
			"ups":  {Class: "ups_sensor"},
		}

		pricers := map[string]Pricer{
			"flat": testPricer(),
			"slot": flatSlots(co.day.Add(-time.Hour), int(co.hours*2)+4, 20),
		}

		for _, tok := range AllowedIntervals() {
			for pname, pricer := range pricers {
				name := fmt.Sprintf("%s/%s/%s", co.name, tok, pname)
				t.Run(name, func(t *testing.T) {
					q := &influx.FakeQuerier{QueryFunc: func(flux string) ([]influx.Row, error) {
						rows, err := counters.Answer(flux)
						if err != nil || rows != nil {
							return rows, err
						}
						return powers.Answer(flux)
					}}
					iv, _ := lookupInterval(tok)
					resp, err := BuildSeries(context.Background(), q, "b", win, iv, GroupByDevice,
						false, false, devices, pricer, nil, loc)
					if err != nil {
						t.Fatalf("BuildSeries: %v", err)
					}

					// No query may ask Influx for local SUB-DAY windows: that is
					// the shape it stretches across the repeated hour.
					for _, flux := range q.Queries {
						m := fluxEveryRe.FindStringSubmatch(flux)
						if m != nil && m[1] != "1d" && strings.Contains(flux, "timezone.location(name:") {
							t.Errorf("sub-day query (every: %s) asks for local windows:\n%s", m[1], flux)
						}
					}

					assertBucketsMatchTheirLength(t, resp, win, watts)
				})
			}
		}
	}
}

// assertBucketsMatchTheirLength checks every series bucket against the energy
// a steady load must put in it: its real length (clipped to the window) at
// `watts`. Zero buckets, stretched buckets and swallowed hours all fail this.
func assertBucketsMatchTheirLength(t *testing.T, resp SeriesResponse, win Window, watts float64) {
	t.Helper()
	n := len(resp.Buckets)
	if n == 0 {
		t.Fatal("no buckets")
	}
	if len(resp.Series) != 2 {
		t.Fatalf("series = %d, want 2 (plug, ups)", len(resp.Series))
	}
	// One reading's worth: increase() re-bases at the first reading in range,
	// so the counter's first bucket is a minute short.
	minute := watts / 60 / 1000
	var lengthSum float64
	for _, s := range resp.Series {
		if len(s.KWh) != n {
			t.Fatalf("%s: %d values for %d buckets", s.Key, len(s.KWh), n)
		}
		for i := range resp.Buckets {
			from, to := resp.Buckets[i], win.Stop
			if i+1 < n {
				to = resp.Buckets[i+1]
			}
			if from.Before(win.Start) {
				from = win.Start
			}
			hours := to.Sub(from).Hours()
			if s.Key == "plug" {
				lengthSum += hours
			}
			want := watts * hours / 1000
			slack := 0.0006 // wire rounding
			if s.Key == "plug" && i == 0 {
				slack += minute
			}
			if got := s.KWh[i]; got <= 0 || math.Abs(got-want) > slack {
				t.Errorf("%s bucket %d (%s, %.2fh): %.4f kWh, want %.4f",
					s.Key, i, resp.Buckets[i].Format(time.RFC3339), hours, got, want)
			}
		}
	}
	// The buckets tile the window exactly: no gap and no overlap.
	if span := win.Stop.Sub(win.Start).Hours(); math.Abs(lengthSum-span) > 1e-9 {
		t.Errorf("buckets cover %.2fh of a %.2fh window", lengthSum, span)
	}
}

// flatSlots is a half-hourly pricer with the same rate in every slot. It
// declares the 30m grid, so BuildSeries takes the fine-cost-axis path.
func flatSlots(start time.Time, n int, pence float64) Pricer {
	rates := make([]*float64, n)
	for i := range rates {
		rates[i] = fp(pence)
	}
	return slotRates{start: start.UTC(), rates: rates}
}

// The 6h axis is LOCAL wall clock: 00/06/12/18 local on both sides of a
// change, which makes the changeover day's first bucket 7h (autumn) or 5h
// (spring). It used to step a fixed 6h from local midnight, which drifted an
// hour off that grid after the change (00:00+01:00, 05:00Z, 11:00Z, 17:00Z in
// prod on 2026-10-25).
func TestBucketStarts6hFollowsLocalWallClockAcrossChanges(t *testing.T) {
	loc := mustLondon(t)
	iv, _ := lookupInterval("6h")
	for _, tc := range []struct {
		name  string
		start time.Time
		stop  time.Time
		want  []string
	}{
		{
			name:  "autumn",
			start: time.Date(2026, 10, 25, 0, 0, 0, 0, loc),
			stop:  time.Date(2026, 10, 26, 0, 0, 0, 0, loc),
			want:  []string{"2026-10-25T00:00:00+01:00", "2026-10-25T06:00:00Z", "2026-10-25T12:00:00Z", "2026-10-25T18:00:00Z"},
		},
		{
			name:  "spring",
			start: time.Date(2027, 3, 28, 0, 0, 0, 0, loc),
			stop:  time.Date(2027, 3, 29, 0, 0, 0, 0, loc),
			want:  []string{"2027-03-28T00:00:00Z", "2027-03-28T06:00:00+01:00", "2027-03-28T12:00:00+01:00", "2027-03-28T18:00:00+01:00"},
		},
		{
			// An off-grid custom `from` inside the REPEATED hour (the second
			// 01:30, in GMT) still snaps down to the local 00:00 boundary.
			name:  "autumn off-grid in repeated hour",
			start: time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC),
			stop:  time.Date(2026, 10, 25, 13, 0, 0, 0, time.UTC),
			want:  []string{"2026-10-25T00:00:00+01:00", "2026-10-25T06:00:00Z", "2026-10-25T12:00:00Z"},
		},
		{
			// Several days, so the grid is checked well clear of the change too.
			name:  "multi-day across autumn",
			start: time.Date(2026, 10, 24, 12, 0, 0, 0, loc),
			stop:  time.Date(2026, 10, 26, 6, 0, 0, 0, loc),
			want: []string{
				"2026-10-24T12:00:00+01:00", "2026-10-24T18:00:00+01:00",
				"2026-10-25T00:00:00+01:00", "2026-10-25T06:00:00Z", "2026-10-25T12:00:00Z", "2026-10-25T18:00:00Z",
				"2026-10-26T00:00:00Z",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := BucketStarts(Window{Start: tc.start, Stop: tc.stop, Label: WindowCustom}, iv, loc)
			var gotS []string
			for _, b := range got {
				gotS = append(gotS, b.Format(time.RFC3339))
			}
			if strings.Join(gotS, " ") != strings.Join(tc.want, " ") {
				t.Errorf("buckets\n got  %v\n want %v", gotS, tc.want)
			}
		})
	}
}

// At 1h and finer the axis is the absolute (UTC) grid, so the repeated hour is
// two distinct buckets and the missing hour is simply absent: 25 buckets on
// the autumn day, 23 in spring, each exactly an hour apart.
func TestBucketStartsSubHourlyIsAbsoluteAcrossChanges(t *testing.T) {
	loc := mustLondon(t)
	for _, co := range changeovers(t) {
		next := time.Date(co.day.Year(), co.day.Month(), co.day.Day()+1, 0, 0, 0, 0, loc)
		for _, tok := range []string{"5m", "15m", "30m", "1h"} {
			iv, _ := lookupInterval(tok)
			got := BucketStarts(Window{Start: co.day, Stop: next, Label: WindowCustom}, iv, loc)
			if want := int(co.hours * float64(time.Hour) / float64(iv.Duration)); len(got) != want {
				t.Errorf("%s %s: %d buckets, want %d", co.name, tok, len(got), want)
			}
			for i := 1; i < len(got); i++ {
				if d := got[i].Sub(got[i-1]); d != iv.Duration {
					t.Errorf("%s %s: bucket %d is %v after the previous, want %v", co.name, tok, i, d, iv.Duration)
				}
			}
		}
	}
}

// A 6h display is never queried AT 6h: Influx would lay 6h windows on the
// stretched local grid. It is queried on the 1h UTC grid and folded in Go.
func TestSeries6hIsQueriedHourlyAndFolded(t *testing.T) {
	loc := mustLondon(t)
	day := time.Date(2026, 10, 25, 0, 0, 0, 0, loc)
	q := &influx.FakeQuerier{}
	iv, _ := lookupInterval("6h")
	devices := map[string]config.DeviceConfig{
		"plug": {Class: "continuous_power_device"},
		"ups":  {Class: "ups_sensor"},
	}
	resp, err := BuildSeries(context.Background(), q, "b",
		Window{Start: day, Stop: day.AddDate(0, 0, 1), Label: WindowCustom},
		iv, GroupByDevice, false, false, devices, testPricer(), nil, loc)
	if err != nil {
		t.Fatalf("BuildSeries: %v", err)
	}
	if resp.Interval != "6h" {
		t.Errorf("interval = %q, want 6h on the wire", resp.Interval)
	}
	if len(q.Queries) == 0 {
		t.Fatal("no queries issued")
	}
	for _, flux := range q.Queries {
		if !strings.Contains(flux, "every: 1h,") {
			t.Errorf("6h display must query at 1h; got:\n%s", flux)
		}
		if !strings.Contains(flux, "location: timezone.utc") {
			t.Errorf("1h query must window on the UTC grid; got:\n%s", flux)
		}
	}
}

// bucketStartsWithin runs BucketStarts under a deadline, so a regression to the
// old calendar loop — which never returned on a skipped local midnight — fails
// this test instead of hanging the whole package.
func bucketStartsWithin(t *testing.T, win Window, iv Interval, loc *time.Location) []time.Time {
	t.Helper()
	done := make(chan []time.Time, 1)
	go func() { done <- BucketStarts(win, iv, loc) }()
	select {
	case got := <-done:
		return got
	case <-time.After(2 * time.Second):
		t.Fatalf("BucketStarts(%s, %s, %s) did not return", win.Start.Format(time.RFC3339), iv.Token, loc)
		return nil
	}
}

func formatAll(ts []time.Time) string {
	var s []string
	for _, b := range ts {
		s = append(s, b.Format(time.RFC3339))
	}
	return strings.Join(s, " ")
}

// Zones whose clocks go forward AT a local 00:00 (PR #50 review). The skipped
// midnight is a 6h and a 1d boundary, and it used to break both axes:
//
//   - 6h drifted onto 05/11/17/23 for the rest of the window, because each step
//     was taken from the previous RESOLVED instant and time.Date resolves the
//     missing 00:00 to 23:00 the day before;
//   - 1d never returned: the missing midnight resolved into the previous day,
//     so "the next day's midnight" was the same instant again, forever.
//
// Both zones are whole-hour, so checkWholeHourZone accepts them. A skipped
// boundary now starts at the transition instant (the moment the clock reached
// that time), and every boundary after it is back on the local grid.
func TestBucketStartsZonesThatSkipLocalMidnight(t *testing.T) {
	for _, tc := range []struct {
		tz, interval string
		from, to     [3]int // y, m, d (local midnight)
		want         string
	}{
		{
			// Santiago 2026-09-06: 00:00 -04 → 01:00 -03 (04:00Z).
			tz: "America/Santiago", interval: "6h", from: [3]int{2026, 9, 5}, to: [3]int{2026, 9, 7},
			want: "2026-09-05T00:00:00-04:00 2026-09-05T06:00:00-04:00 2026-09-05T12:00:00-04:00 2026-09-05T18:00:00-04:00 " +
				"2026-09-06T01:00:00-03:00 2026-09-06T06:00:00-03:00 2026-09-06T12:00:00-03:00 2026-09-06T18:00:00-03:00",
		},
		{
			tz: "America/Santiago", interval: "1d", from: [3]int{2026, 9, 3}, to: [3]int{2026, 9, 9},
			want: "2026-09-03T00:00:00-04:00 2026-09-04T00:00:00-04:00 2026-09-05T00:00:00-04:00 " +
				"2026-09-06T01:00:00-03:00 2026-09-07T00:00:00-03:00 2026-09-08T00:00:00-03:00",
		},
		{
			// Havana 2026-03-08: 00:00 -05 → 01:00 -04 (05:00Z).
			tz: "America/Havana", interval: "6h", from: [3]int{2026, 3, 7}, to: [3]int{2026, 3, 9},
			want: "2026-03-07T00:00:00-05:00 2026-03-07T06:00:00-05:00 2026-03-07T12:00:00-05:00 2026-03-07T18:00:00-05:00 " +
				"2026-03-08T01:00:00-04:00 2026-03-08T06:00:00-04:00 2026-03-08T12:00:00-04:00 2026-03-08T18:00:00-04:00",
		},
		{
			tz: "America/Havana", interval: "1d", from: [3]int{2026, 3, 6}, to: [3]int{2026, 3, 10},
			want: "2026-03-06T00:00:00-05:00 2026-03-07T00:00:00-05:00 2026-03-08T01:00:00-04:00 2026-03-09T00:00:00-04:00",
		},
		{
			// Havana 2026-11-01 goes BACK at 01:00 → 00:00, so local midnight
			// happens twice. The day starts at its FIRST occurrence (-04), which
			// makes it the 25h day.
			tz: "America/Havana", interval: "1d", from: [3]int{2026, 10, 31}, to: [3]int{2026, 11, 3},
			want: "2026-10-31T00:00:00-04:00 2026-11-01T00:00:00-04:00 2026-11-02T00:00:00-05:00",
		},
	} {
		t.Run(tc.tz+"/"+tc.interval, func(t *testing.T) {
			loc, err := time.LoadLocation(tc.tz)
			if err != nil {
				t.Skipf("no tzdata for %s: %v", tc.tz, err)
			}
			iv, _ := lookupInterval(tc.interval)
			win := Window{
				Start: time.Date(tc.from[0], time.Month(tc.from[1]), tc.from[2], 0, 0, 0, 0, loc),
				Stop:  time.Date(tc.to[0], time.Month(tc.to[1]), tc.to[2], 0, 0, 0, 0, loc),
				Label: WindowCustom,
			}
			if got := formatAll(bucketStartsWithin(t, win, iv, loc)); got != tc.want {
				t.Errorf("buckets\n got  %s\n want %s", got, tc.want)
			}
		})
	}
}

// The wall-clock and calendar axes, swept over two whole years in zones with
// every kind of clock change (none, at 01:00, at local midnight, going back
// across midnight), must terminate, strictly increase, and keep every
// boundary on the local grid — except a boundary a clock change skipped, which
// must sit exactly on that change.
func TestBucketStartsWallClockAxesStayOnGrid(t *testing.T) {
	zones := []string{"Europe/London", "UTC", "America/New_York", "America/Santiago",
		"America/Havana", "Asia/Beirut", "America/Asuncion", "Australia/Sydney"}
	for _, tz := range zones {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			t.Logf("skipping %s: %v", tz, err)
			continue
		}
		win := Window{
			Start: time.Date(2026, 1, 1, 0, 0, 0, 0, loc),
			Stop:  time.Date(2028, 1, 1, 0, 0, 0, 0, loc),
			Label: WindowCustom,
		}
		for _, tok := range []string{"6h", "1d"} {
			iv, _ := lookupInterval(tok)
			got := bucketStartsWithin(t, win, iv, loc)
			step := 6
			if iv.Calendar {
				step = 24
			}
			for i, b := range got {
				if i > 0 && !b.After(got[i-1]) {
					t.Errorf("%s %s: bucket %d (%s) does not follow %s", tz, tok, i, b.Format(time.RFC3339), got[i-1].Format(time.RFC3339))
				}
				if b.Hour()%step == 0 && b.Minute() == 0 {
					continue // on the local grid
				}
				// Off-grid is allowed only AT a clock change: the instant a
				// zone period starts.
				if start, _ := b.ZoneBounds(); !start.Equal(b) {
					t.Errorf("%s %s: bucket %d (%s) is off the local grid and not at a clock change",
						tz, tok, i, b.Format(time.RFC3339))
				}
			}
		}
	}
}
