package httpapi

import (
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/influx"
)

// ---------------------------------------------------------------------------
// Issue #27 at the HTTP boundary: GET /devices/{id}/series and
// GET /devices/{id}/energy must describe the same electricity over the same
// window. They did not, for any window whose `from` is off the interval grid:
// the series' first bucket reported its whole grid interval, so holding `to`
// fixed and walking `from` later through that bucket returned an unchanged
// total while /energy correctly fell.
// ---------------------------------------------------------------------------

// offGridFixtureSetup wires a Server whose Influx is the steady-1kW winefridge
// counter, sampled every 10s from 12:00 to 18:00 local on 2026-06-11 (real
// plugs report every 30s or faster).
func offGridFixtureSetup(t *testing.T) *Server {
	t.Helper()
	s, _ := dataSetup(t)
	s.Influx = influx.NewCounterSim(s.Loc).AddSteady("winefridge",
		time.Date(2026, 6, 11, 12, 0, 0, 0, s.Loc),
		time.Date(2026, 6, 11, 18, 0, 0, 0, s.Loc),
		10*time.Second, 1000)
	return s
}

// TestOffGridWindow_SeriesAndEnergyAgree is the invariant issue #27 asks for:
// over a deliberately off-grid window (14:29 BST, a 30m grid) the two endpoints
// must report the same kWh. Before the fix /series was a full grid interval
// high — 29 of a 134-minute window, ~22%.
func TestOffGridWindow_SeriesAndEnergyAgree(t *testing.T) {
	s := offGridFixtureSetup(t)
	const win = "window=custom&from=2026-06-11T13:29:00Z&to=2026-06-11T15:43:00Z" // 14:29->16:43 BST

	we := doGET(t, s, "/devices/winefridge/energy?"+win)
	if we.Code != http.StatusOK {
		t.Fatalf("energy: want 200, got %d: %s", we.Code, we.Body.String())
	}
	scalar := decode(t, we)["kwh"].(float64)

	ws := doGET(t, s, "/devices/winefridge/series?"+win+"&interval=30m")
	if ws.Code != http.StatusOK {
		t.Fatalf("series: want 200, got %d: %s", ws.Code, ws.Body.String())
	}
	r := decodeSeries(t, ws)
	if len(r.Series) != 1 {
		t.Fatalf("series = %d, want 1: %+v", len(r.Series), r.Series)
	}
	total := r.Series[0].TotalKWh

	// Per-bucket kWh is rounded to 3dp before summing: half a unit in the last
	// place per bucket, and no more.
	tol := 0.0005 * float64(len(r.Buckets))
	if math.Abs(total-scalar) > tol {
		t.Errorf("/devices/winefridge/series total_kwh = %.4f but "+
			"/devices/winefridge/energy kwh = %.4f over the same window "+
			"(delta %.4f, tolerance %.4f)", total, scalar, total-scalar, tol)
	}
	// Both must be the physically right answer: 1 kW from 14:29 to the last
	// sample strictly inside the window, ~2h14m.
	if want := 134.0 / 60; math.Abs(total-want) > 0.01 {
		t.Errorf("series total_kwh = %.4f, want ~%.4f (1 kW over 14:29->16:43)", total, want)
	}

	// Bucket 0 covers only 14:29->14:30, so at 1 kW it can bill at most one
	// minute — nowhere near the half hour its grid interval spans. It must also
	// be non-zero: the clip is meant to narrow the bucket, not discard it.
	head := r.Series[0].KWh[0]
	if head <= 0 || head > round3(1.0/60)+1e-9 {
		t.Errorf("first bucket kwh = %v, want (0, %v] — one minute at 1 kW, not the "+
			"full 30m grid interval (%v)", head, round3(1.0/60), 0.5)
	}
	// Interior buckets are untouched full intervals; the tail is clipped to `to`.
	for i := 1; i < len(r.Series[0].KWh)-1; i++ {
		if math.Abs(r.Series[0].KWh[i]-0.5) > 1e-9 {
			t.Errorf("interior bucket %d kwh = %v, want 0.5 (30m at 1 kW)", i, r.Series[0].KWh[i])
		}
	}
	if tail := r.Series[0].KWh[len(r.Series[0].KWh)-1]; math.Abs(tail-round3(13.0/60)) > 0.001 {
		t.Errorf("last bucket kwh = %v, want ~%v (16:30->16:43)", tail, round3(13.0/60))
	}
	// The axis itself is unchanged: it still snaps DOWN to the grid (issue #1),
	// so bucket 0's timestamp legitimately precedes `from`.
	if !strings.HasPrefix(r.Buckets[0], "2026-06-11T14:00:00") {
		t.Errorf("bucket[0] = %q, want the 14:00 grid boundary (the axis snap is deliberate)", r.Buckets[0])
	}
}

// TestOffGridWindow_SeriesTotalTracksTheWindowStart reproduces the issue's own
// table: hold `to` fixed, walk `from` later through the first 30m bucket, and
// both endpoints must fall together. /series previously returned a
// byte-identical total for all four windows.
func TestOffGridWindow_SeriesTotalTracksTheWindowStart(t *testing.T) {
	s := offGridFixtureSetup(t)

	read := func(fromUTC string) (series, energy float64) {
		q := "window=custom&from=2026-06-11T" + fromUTC + ":00Z&to=2026-06-11T15:43:00Z"
		we := doGET(t, s, "/devices/winefridge/energy?"+q)
		if we.Code != http.StatusOK {
			t.Fatalf("energy %s: %d %s", fromUTC, we.Code, we.Body.String())
		}
		ws := doGET(t, s, "/devices/winefridge/series?"+q+"&interval=30m")
		if ws.Code != http.StatusOK {
			t.Fatalf("series %s: %d %s", fromUTC, ws.Code, ws.Body.String())
		}
		r := decodeSeries(t, ws)
		if len(r.Series) != 1 {
			t.Fatalf("series %s: got %d series", fromUTC, len(r.Series))
		}
		return r.Series[0].TotalKWh, decode(t, we)["kwh"].(float64)
	}

	// 13:00Z..13:29Z = 14:00..14:29 BST, all inside the first 30m grid bucket.
	prevSeries, prevEnergy := read("13:00")
	for _, from := range []string{"13:01", "13:17", "13:29"} {
		gotSeries, gotEnergy := read(from)
		if gotEnergy >= prevEnergy {
			t.Fatalf("fixture broken: /energy from %sZ = %.4f, expected a fall from %.4f",
				from, gotEnergy, prevEnergy)
		}
		if gotSeries >= prevSeries {
			t.Errorf("/series total_kwh from %sZ = %.4f, must fall below the earlier start's %.4f "+
				"(it tracks /energy, which fell to %.4f)", from, gotSeries, prevSeries, gotEnergy)
		}
		if math.Abs(gotSeries-gotEnergy) > 0.004 {
			t.Errorf("from %sZ: /series %.4f vs /energy %.4f — the endpoints disagree",
				from, gotSeries, gotEnergy)
		}
		prevSeries, prevEnergy = gotSeries, gotEnergy
	}
}

// round3 mirrors the kWh display precision the series applies per bucket.
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
