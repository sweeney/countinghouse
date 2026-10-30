// Package calendar answers "how many billing days is this span?" — the one
// question every standing-charge path in countinghouse has to ask.
//
// It exists as its own leaf package (importing nothing but the standard
// library) so that config, energy and prices can all share ONE answer. Before
// issue #56 each of them did its own `elapsed.Hours() / 24`, and the archive
// and configured-tariff paths agreeing with each other was a coincidence of
// identical bugs rather than a property of the code.
package calendar

import (
	"fmt"
	"time"
)

// Days returns how many calendar days of supply [from, to) covers, measured in
// the local calendar of loc. It is the multiplier for a per-day standing charge.
//
// The definition (issue #56):
//
//	Days(from, to) = Σ over each local day d in loc that [from, to) overlaps:
//	                   overlap(d, [from, to)) / length(d)
//
// So:
//
//   - A WHOLE local day counts exactly 1, however long it lasted. In
//     Europe/London 25 Oct 2026 is 25 hours (the clocks go back) and 29 Mar
//     2026 is 23 hours (they go forward); each is one day, because a UK
//     standing charge is a price per calendar day of supply and that is how the
//     supplier bills it. The previous elapsed/24 arithmetic billed them as
//     25/24 and 23/24 of a day: any window containing the autumn change was
//     1/24 of a day's charge high (~2.5p at ~59p/day), any containing the
//     spring change the same low, cancelling only in a window containing both.
//
//   - A PART day is pro rata, as the API has always promised — but against the
//     length of the day it is part of, not against a fixed 24 hours. The first
//     12 elapsed hours of 25 Oct are 12/25 of a day; 24 elapsed hours from that
//     midnight are 24/25, because an hour of the day is still to come. Only
//     the denominator changed; on any ordinary day this is identical to the old
//     elapsed/24 answer.
//
//   - It is ADDITIVE: Days(a, b) + Days(b, c) == Days(a, c) for any b in
//     between (to float rounding). The bill splits windows at every tariff and
//     VAT boundary and the archive path splits again at every standing-charge
//     change; additivity is what makes a day's charge independent of how many
//     such cuts happen to land in it, so the segments still sum to the window.
//
// The answer depends only on the two instants and loc — never on the
// *time.Location that from and to happen to carry — so a window parsed from a
// query string (UTC) and one built by ResolveWindow (local) bill identically.
//
// Zones whose clocks change AT local midnight (America/Santiago,
// America/Havana — both accepted by config) are handled: a date whose 00:00 is
// skipped begins at the transition instant (see startOfDay).
//
// An empty or inverted span is 0 days. A nil loc is treated as UTC, where every
// day is 24 hours and this reduces exactly to the old elapsed/24; that keeps a
// zero-value caller working rather than panicking, but production callers must
// pass the house zone (Server.loc()) or they reintroduce the bug.
func Days(from, to time.Time, loc *time.Location) float64 {
	if !to.After(from) {
		return 0
	}
	if loc == nil {
		loc = time.UTC
	}

	// Walk CIVIL DATES, not instants. Each pass names a date (y, m, d) and asks
	// when that date begins and when the next one does; the date counter, not
	// the instant, is what advances. Deriving "the next date" from an instant
	// instead is what used to hang: in a zone that skips midnight, time.Date
	// resolves the missing 00:00 back to 23:00 the PREVIOUS day, the next pass
	// read that day's date back off it, and the walk never moved (PR #57 review).
	y, m, d := from.In(loc).Date()

	var days float64
	for {
		dayStart := startOfDay(y, m, d, loc)
		if !dayStart.Before(to) {
			break
		}
		// d+1 rather than AddDate on dayStart: AddDate keeps dayStart's wall
		// clock, which on a gap day is 01:00, not 00:00. startOfDay normalises
		// overflow (31 Oct + 1 is 1 Nov), so month and year ends need no case.
		next := startOfDay(y, m, d+1, loc)
		if !next.After(dayStart) {
			// Unreachable while startOfDay is right: a civil date always lasts
			// some positive time. Fail loudly rather than spin on the request path,
			// which is how the original form of this bug presented.
			panic(fmt.Sprintf("calendar: %04d-%02d-%02d in %s starts at %s but the next date does not start after it (%s)",
				y, m, d, loc, dayStart.Format(time.RFC3339), next.Format(time.RFC3339)))
		}
		length := next.Sub(dayStart) // 23h, 24h or 25h in Europe/London

		// The part of this day that falls inside [from, to). Only the first and
		// last days can be partial; every day between is clipped to itself and
		// contributes exactly length/length = 1.
		lo, hi := dayStart, next
		if from.After(lo) {
			lo = from
		}
		if to.Before(hi) {
			hi = to
		}
		if hi.After(lo) {
			// Divide Durations directly (int64 nanoseconds converted once) rather
			// than via Hours(), so a whole day is bit-exactly 1.0.
			days += float64(hi.Sub(lo)) / float64(length)
		}

		d++
	}
	return days
}

// startOfDay returns the first instant of the civil date y-m-d in loc: the
// instant from which a clock in loc reads that date.
//
// Usually that is 00:00 local. Two zone-transition cases make it something else,
// and both are real in zones config.checkWholeHourZone accepts (America/Santiago
// and America/Havana change their clocks AT midnight):
//
//   - Clocks go BACK at midnight: the hour before midnight repeats and 00:00
//     itself happens once, which time.Date returns. Taken as is. (Were a zone
//     ever to repeat 00:00 itself, time.Date returns the first occurrence,
//     which is when the date begins — still right.)
//   - Midnight SKIPPED (clocks go forward at 00:00): there is no 00:00 that day;
//     the clock jumps from 23:59:59 the day before straight to 01:00. The date
//     begins at that transition instant. time.Date leaves which side of the gap
//     it resolves to unspecified (today it picks 23:00 the day BEFORE, which is
//     a different date), so the side is detected and the zone boundary on that
//     side taken — the same rule energy.resolveWall applies to series axes.
//     calendar cannot import energy (energy imports calendar), hence a sibling
//     implementation rather than a shared one.
//
// y, m, d may be out of range (d = 32, m = 13, …); they are normalised exactly
// as time.Date normalises them.
func startOfDay(y int, m time.Month, d int, loc *time.Location) time.Time {
	// Normalise the date on a zone with no transitions, so the comparison
	// below is against the date actually meant (2026-10-32 is 2026-11-01).
	y, m, d = time.Date(y, m, d, 12, 0, 0, 0, time.UTC).Date()

	t := time.Date(y, m, d, 0, 0, 0, 0, loc)
	ty, tm, td := t.Date()
	sameDate := ty == y && tm == m && td == d
	switch {
	case sameDate && t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0:
		// Ordinary, or the first of a repeated midnight.
		return t
	case !sameDate:
		// Resolved short of the gap, into the previous date: the date begins
		// where that zone period ends.
		_, end := t.ZoneBounds()
		return end
	default:
		// Resolved past the gap, later on the right date: the date begins where
		// this zone period starts.
		start, _ := t.ZoneBounds()
		return start
	}
}
