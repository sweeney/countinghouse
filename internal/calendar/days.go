// Package calendar answers "how many billing days is this span?" — the one
// question every standing-charge path in countinghouse has to ask.
//
// It exists as its own leaf package (importing nothing but the standard
// library) so that config, energy and prices can all share ONE answer. Before
// issue #56 each of them did its own `elapsed.Hours() / 24`, and the archive
// and configured-tariff paths agreeing with each other was a coincidence of
// identical bugs rather than a property of the code.
package calendar

import "time"

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
//     25/24 and 23/24 of a day — ~2.5p high in October, ~2.5p low in March,
//     cancelling only over a window that happened to contain both.
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

	// Start at the local midnight of the day containing `from`. Built with
	// time.Date, as everywhere else in this service, so the offset is correct for
	// that date (00:00 local is 23:00Z in BST, 00:00Z in GMT).
	lf := from.In(loc)
	dayStart := time.Date(lf.Year(), lf.Month(), lf.Day(), 0, 0, 0, 0, loc)

	var days float64
	for dayStart.Before(to) {
		// The next local midnight. AddDate on the calendar fields — never
		// Add(24*time.Hour) — is the whole point: on a clock-change day the two
		// differ by an hour. time.Date normalises day overflow (31 Oct + 1 is
		// 1 Nov), so month and year ends need no special case.
		ds := dayStart.In(loc)
		next := time.Date(ds.Year(), ds.Month(), ds.Day()+1, 0, 0, 0, 0, loc)
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

		dayStart = next
	}
	return days
}
