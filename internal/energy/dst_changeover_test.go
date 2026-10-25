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
		counters := influx.NewCounterSim(loc).
			AddSteady("plug", co.day.Add(-time.Hour), next.Add(time.Hour), time.Minute, watts)
		powers := influx.NewPowerSim(loc).
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
