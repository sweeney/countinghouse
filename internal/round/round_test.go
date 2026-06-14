package round

import (
	"math"
	"testing"
)

// TestTo is the single canonical rounding table test, replacing the two
// per-package roundTo tests. It covers the decimal-place behaviour, the
// half-away-from-zero rule on a negative input, and the NaN/Inf→0 policy that
// keeps every emitted value JSON-safe.
func TestTo(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		dp   int
		want float64
	}{
		{"kwh tail", 0.24127950000000187, KWhDP, 0.241},
		{"money tail", 1.10000000000000853, MoneyDP, 1.1},
		{"watts", 52.149, WDP, 52.1},
		{"half away from zero positive", 0.0625, KWhDP, 0.063},
		{"half away from zero negative", -0.0625, KWhDP, -0.063},
		{"negative tail", -0.0004999, KWhDP, -0.0},
		{"nan to zero", math.NaN(), KWhDP, 0},
		{"pos inf to zero", math.Inf(1), MoneyDP, 0},
		{"neg inf to zero", math.Inf(-1), WDP, 0},
		{"zero dp", 2.5, 0, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := To(c.in, c.dp)
			if got != c.want {
				t.Fatalf("To(%v, %d) = %v, want %v", c.in, c.dp, got, c.want)
			}
		})
	}
}
