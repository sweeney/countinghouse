package energy

import (
	"fmt"
	"time"
)

// Window labels returned by ResolveWindow.
const (
	WindowToday  = "today"
	WindowWeek   = "week"
	WindowMonth  = "month"
	WindowCustom = "custom"
)

// Window is a resolved, half-open time range [Start, Stop). Label records which
// spec produced it ("today"/"week"/"month"/"custom").
//
// Boundary semantics:
//   - today, week and month are PERIOD-TO-DATE: Start is a local-midnight
//     boundary (today's midnight / most-recent-Monday midnight / the 1st of the
//     month) and Stop is the caller-supplied "now". These windows therefore
//     normally cover a partial day.
//   - custom is an arbitrary explicit range: Start and Stop are exactly the
//     caller-supplied from/to.
type Window struct {
	Start, Stop time.Time
	Label       string
}

// ResolveWindow turns a window spec into a half-open [Start, Stop) range.
//
// It is pure and clock-driven: now is supplied by the caller (sourced from the
// injected Clock) and ResolveWindow never calls time.Now() itself. loc is the
// timezone the calendar boundaries are computed in (Europe/London in prod);
// week starts Monday per PLAN §2.
//
// Spec handling:
//   - "today": Start = local midnight of now's date in loc; Stop = now.
//   - "week":  Start = most recent Monday 00:00 local (if now is a Monday, that
//     is today's midnight); Stop = now.
//   - "month": Start = the 1st of now's month at 00:00 local; Stop = now.
//   - "custom": requires both from and to non-zero; Start = from, Stop = to.
//     Errors if either is missing or if to <= from.
//   - any other spec is an error.
//
// Local midnights are built with time.Date(y, m, d, 0,0,0,0, loc) so DST is
// handled correctly: a London "day" may be 23h (spring forward) or 25h (autumn
// back) across the clock change, and the resulting Start carries the correct
// UTC offset for its date (00:00 local is 23:00 UTC in BST, 00:00 UTC in GMT).
func ResolveWindow(now time.Time, loc *time.Location, spec string, from, to time.Time) (Window, error) {
	if loc == nil {
		return Window{}, fmt.Errorf("energy: nil location")
	}

	switch spec {
	case WindowToday:
		ln := now.In(loc)
		start := time.Date(ln.Year(), ln.Month(), ln.Day(), 0, 0, 0, 0, loc)
		return Window{Start: start, Stop: now, Label: WindowToday}, nil

	case WindowWeek:
		ln := now.In(loc)
		midnight := time.Date(ln.Year(), ln.Month(), ln.Day(), 0, 0, 0, 0, loc)
		// Weekday(): Sunday=0..Saturday=6. Days since Monday: Mon->0 .. Sun->6.
		offset := (int(midnight.Weekday()) + 6) % 7
		start := time.Date(ln.Year(), ln.Month(), ln.Day()-offset, 0, 0, 0, 0, loc)
		return Window{Start: start, Stop: now, Label: WindowWeek}, nil

	case WindowMonth:
		ln := now.In(loc)
		start := time.Date(ln.Year(), ln.Month(), 1, 0, 0, 0, 0, loc)
		return Window{Start: start, Stop: now, Label: WindowMonth}, nil

	case WindowCustom:
		if from.IsZero() || to.IsZero() {
			return Window{}, fmt.Errorf("energy: custom window requires both from and to")
		}
		if !to.After(from) {
			return Window{}, fmt.Errorf("energy: custom window to (%s) must be after from (%s)", to, from)
		}
		return Window{Start: from, Stop: to, Label: WindowCustom}, nil

	default:
		return Window{}, fmt.Errorf("energy: unknown window spec %q", spec)
	}
}

// Days returns the window length in days, computed as the real elapsed duration
// (Stop - Start) divided by 24 hours. This is FRACTIONAL: period-to-date
// windows (today/week/month) end at "now" and so are partial days. Because the
// duration is wall-clock elapsed time, a London day that spans a DST change
// counts as 23h/24 or 25h/24 of a day, which is the physically correct number
// of days the standing charge applied for.
//
// How this should be consumed for standing charge is left to the cost
// milestone: it decides any rounding/ceiling policy (e.g. whether a partial day
// is billed as a whole day).
func (w Window) Days() float64 {
	return w.Stop.Sub(w.Start).Hours() / 24
}
