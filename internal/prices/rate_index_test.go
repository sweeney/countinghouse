package prices

import (
	"fmt"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The index must answer exactly what the scan answers, on every instant.
//
// RateAt decides what a kWh cost, so this is a differential test rather than a
// set of expectations: the same curve is built twice, once through NewCurve and
// once as a literal (which leaves idx nil and keeps the scan), and the two are
// asked the same questions. Anything the index gets wrong shows up as a
// disagreement, including in the cases where it is supposed to refuse to exist.
// ---------------------------------------------------------------------------

// probes returns instants worth asking about across a window: every slot's start,
// its midpoint, the instant before and after each boundary, and points outside the
// window at both ends.
func probes(slots []Slot) []time.Time {
	var out []time.Time
	if len(slots) == 0 {
		return []time.Time{time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	}
	first := slots[0].ValidFrom
	out = append(out, first.Add(-time.Hour), first.Add(-time.Nanosecond))
	for _, s := range slots {
		out = append(out,
			s.ValidFrom,
			s.ValidFrom.Add(-time.Nanosecond),
			s.ValidFrom.Add(SlotLength/2),
			s.ValidFrom.Add(SlotLength-time.Nanosecond),
			s.ValidFrom.Add(SlotLength),
		)
	}
	last := slots[len(slots)-1].ValidFrom
	out = append(out, last.Add(SlotLength), last.Add(24*time.Hour))
	return out
}

// assertSameAnswers fails on any instant where the two curves disagree.
func assertSameAnswers(t *testing.T, indexed, scanned Curve) {
	t.Helper()
	for _, at := range probes(scanned.Slots) {
		wantRate, wantOK := scanned.RateAt(at)
		gotRate, gotOK := indexed.RateAt(at)
		if gotOK != wantOK || gotRate != wantRate {
			t.Errorf("RateAt(%s): indexed (%v, %v), scan (%v, %v)",
				at.Format(time.RFC3339Nano), gotRate, gotOK, wantRate, wantOK)
		}
	}
}

func TestIndexedRateAtMatchesTheScan(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	// contiguous builds n abutting half-hourly slots from start.
	contiguous := func(n int) []Slot {
		out := make([]Slot, n)
		for i := range out {
			from := start.Add(time.Duration(i) * SlotLength)
			to := from.Add(SlotLength)
			out[i] = Slot{ValidFrom: from, ValidTo: &to, IncVATPence: float64(10 + i)}
		}
		return out
	}

	// withGap drops the slots in [from, to) from a contiguous run, leaving a hole
	// the archive does not hold — which must stay unpriced rather than becoming free.
	withGap := func(n, from, to int) []Slot {
		all := contiguous(n)
		var out []Slot
		for i, s := range all {
			if i >= from && i < to {
				continue
			}
			out = append(out, s)
		}
		return out
	}

	cases := []struct {
		name  string
		slots []Slot
	}{
		{"empty", nil},
		{"one slot", contiguous(1)},
		{"a full day", contiguous(48)},
		{"an interior gap", withGap(48, 20, 24)},
		{"a gap at the start", withGap(48, 0, 3)},
		{"a gap at the end", withGap(48, 45, 48)},
		{"every other slot missing", func() []Slot {
			all := contiguous(48)
			var out []Slot
			for i, s := range all {
				if i%2 == 0 {
					out = append(out, s)
				}
			}
			return out
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			indexed := NewCurve(start, start.Add(24*time.Hour), tc.slots)
			scanned := Curve{From: start, To: start.Add(24 * time.Hour), Slots: tc.slots}

			if len(tc.slots) > 0 && indexed.idx == nil {
				t.Fatalf("no index was built for a well-formed curve; the fast path is dead")
			}
			assertSameAnswers(t, indexed, scanned)
		})
	}
}

// The shapes where a binary search CANNOT reproduce the scan, and so must not be
// indexed at all. Each one would otherwise be a wrong price rather than a slow one.
func TestRateIndexRefusesWhatItCannotAnswer(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	mid := start.Add(SlotLength)
	end := start.Add(2 * SlotLength)

	cases := []struct {
		name  string
		slots []Slot
		why   string
	}{
		{
			name: "an open-ended slot",
			slots: []Slot{
				{ValidFrom: start, ValidTo: nil, IncVATPence: 24.2},
			},
			why: "unbounded, so it overlaps everything after it",
		},
		{
			name: "two payment methods for one half hour",
			slots: []Slot{
				{ValidFrom: start, ValidTo: &mid, PaymentMethod: "DIRECT_DEBIT", IncVATPence: 24.2},
				{ValidFrom: start, ValidTo: &mid, PaymentMethod: "NON_DIRECT_DEBIT", IncVATPence: 26.9},
			},
			why: "the same half hour at two prices",
		},
		{
			name: "a long slot swallowing a short one",
			slots: []Slot{
				{ValidFrom: start, ValidTo: &end, IncVATPence: 24.2},
				{ValidFrom: mid, ValidTo: &end, IncVATPence: 26.9},
			},
			why: "overlapping intervals",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			indexed := NewCurve(start, end, tc.slots)
			if indexed.idx != nil {
				t.Errorf("an index was built for %s (%s); a binary search cannot "+
					"reproduce the scan's first-match rule here", tc.name, tc.why)
			}
			// And it must still answer, via the scan.
			assertSameAnswers(t, indexed, Curve{From: start, To: end, Slots: tc.slots})
		})
	}
}

// Unsorted input must not produce a wrong answer. The store returns slots ordered,
// but nothing in the type says so, and an index that trusted the order would price
// the wrong half hour rather than merely being slow.
func TestIndexedRateAtMatchesTheScanWhenInputIsUnsorted(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	slots := make([]Slot, 48)
	for i := range slots {
		from := start.Add(time.Duration(i) * SlotLength)
		to := from.Add(SlotLength)
		slots[i] = Slot{ValidFrom: from, ValidTo: &to, IncVATPence: float64(10 + i)}
	}
	// Reverse them: newest first, which is the order the supplier's API uses.
	for i, j := 0, len(slots)-1; i < j; i, j = i+1, j-1 {
		slots[i], slots[j] = slots[j], slots[i]
	}

	indexed := NewCurve(start, start.Add(24*time.Hour), slots)
	scanned := Curve{From: start, To: start.Add(24 * time.Hour), Slots: slots}
	assertSameAnswers(t, indexed, scanned)

	// And it must be INDEXED, not merely correct. Without the sort in newRateIndex
	// this input reads as one long overlap and the index is refused — which answers
	// correctly via the scan, so a test asserting only the answers would pass while
	// the sort quietly stopped doing anything. Newest-first is the order the
	// supplier's own API returns, so this is the realistic way to hand it unsorted
	// slots.
	if indexed.idx == nil {
		t.Error("newest-first input was not indexed; the sort in newRateIndex is not doing its job")
	}
}

// The real recorded day, both ways.
func TestIndexedRateAtMatchesTheScanOnTheRealRecordedDay(t *testing.T) {
	slots := fixtureSlots(t, "unit_rates_mixed_sign_day.json")
	from, to := slots[0].ValidFrom, slots[len(slots)-1].ValidFrom.Add(SlotLength)

	indexed := NewCurve(from, to, slots)
	if indexed.idx == nil {
		t.Fatal("no index for a real published day")
	}
	assertSameAnswers(t, indexed, Curve{From: from, To: to, Slots: slots})
}

// What the cost path does: one RateAt per half-hourly bucket, per device.
func BenchmarkRateAtOverWindow(b *testing.B) {
	for _, n := range []int{48, 1488, 17520} {
		scanned := benchCurve(n)
		indexed := NewCurve(scanned.From, scanned.To, scanned.Slots)

		b.Run(fmt.Sprintf("scan/slots=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				for _, s := range scanned.Slots {
					if _, ok := scanned.RateAt(s.ValidFrom); !ok {
						b.Fatal("miss")
					}
				}
			}
		})
		b.Run(fmt.Sprintf("indexed/slots=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				for _, s := range indexed.Slots {
					if _, ok := indexed.RateAt(s.ValidFrom); !ok {
						b.Fatal("miss")
					}
				}
			}
		})
	}
}
