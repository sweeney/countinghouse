package prices

import (
	"fmt"
	"testing"
	"time"
)

// benchCurve builds a curve of n half-hourly slots with a varying price.
func benchCurve(n int) Curve {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	slots := make([]Slot, n)
	for i := range slots {
		f := start.Add(time.Duration(i) * SlotLength)
		to := f.Add(SlotLength)
		p := 20 + float64((i*7)%40) // deterministic, and genuinely varying
		slots[i] = Slot{
			TariffCode: "E-1R-AGILE-24-10-01-A", ValidFrom: f, ValidTo: &to,
			ExcVATPence: p, IncVATPence: p * 1.05, RetrievedAt: start,
		}
	}
	return NewCurve(start, start.Add(time.Duration(n)*SlotLength), slots)
}

// Rendering a curve was quadratic: RankOf is O(n) per slot, PercentileOf calls it, and
// BandOf re-sorted the whole window to find the median for every slot. `/prices` has no
// window cap, so a year-long custom window burned ten seconds of CPU. Measured before
// the fix: 48 slots ~0ms, 1,488 (a month) 57ms, 17,520 (a year) 10.3s.
func BenchmarkCurveRender(b *testing.B) {
	for _, n := range []int{48, 1488, 17520} {
		c := benchCurve(n)
		b.Run(fmt.Sprintf("%d-slots", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				for _, s := range c.Slots {
					_ = c.BandOf(s)
					_ = c.RankOf(s)
					_ = c.PercentileOf(s)
				}
			}
		})
	}
}

// RateAt was a linear scan per lookup, so a year-long /bill cost ~1.67s per device on
// top of Influx, times the fleet.
func BenchmarkCurveRateAt(b *testing.B) {
	c := benchCurve(17520)
	// Look up every slot's midpoint, which is what a full-window cost does.
	at := make([]time.Time, 0, len(c.Slots))
	for _, s := range c.Slots {
		at = append(at, s.ValidFrom.Add(15*time.Minute))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, t := range at {
			c.RateAt(t)
		}
	}
}

// NewCurve itself, because dropAmbiguous added a map build and a pass over every
// slot to the construction path — which runs on every /prices request, including
// the ones that end in a 304. Worth knowing the cost at the window caps the routes
// actually permit: a month is 1,488 half hours, a year 17,520.
func BenchmarkNewCurve(b *testing.B) {
	for _, n := range []int{48, 1488, 17520} {
		b.Run(fmt.Sprintf("%d-slots", n), func(b *testing.B) {
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			slots := make([]Slot, n)
			for i := range slots {
				f := start.Add(time.Duration(i) * SlotLength)
				to := f.Add(SlotLength)
				p := 20 + float64((i*7)%40)
				slots[i] = Slot{
					TariffCode: "E-1R-AGILE-24-10-01-A", ValidFrom: f, ValidTo: &to,
					ExcVATPence: p, IncVATPence: p * 1.05, RetrievedAt: start,
				}
			}
			stop := start.Add(time.Duration(n) * SlotLength)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = NewCurve(start, stop, slots)
			}
		})
	}
}
