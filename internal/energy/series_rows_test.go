package energy

import (
	"testing"
	"time"
)

func TestValidShape(t *testing.T) {
	for _, s := range []string{"", "columns", "rows"} {
		if !ValidShape(s) {
			t.Errorf("ValidShape(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"row", "cols", "wide", "tidy", "ROWS", "x"} {
		if ValidShape(s) {
			t.Errorf("ValidShape(%q) = true, want false", s)
		}
	}
}

func sampleColumnar() SeriesResponse {
	t0 := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	return SeriesResponse{
		Window: "today", From: t0.Format(time.RFC3339), Interval: "1h",
		GroupBy: "device", Shape: ShapeColumns,
		Buckets: []time.Time{t0, t0.Add(time.Hour), t0.Add(2 * time.Hour)},
		Series: []Series{
			{Key: "winefridge", Label: "Wine Fridge", Location: "kitchen", Class: "continuous_power_device",
				KWh: []float64{0.05, 0, 0.1}, Cost: []float64{0.011, 0, 0.022}, AvgW: []float64{52, 0, 99},
				TotalKWh: 0.15, TotalCost: 0.033},
			{Key: "bigfridge", Label: "Kitchen Fridge", Location: "kitchen", Class: "continuous_power_device",
				KWh: []float64{0.06, 0.04, 0}, Cost: []float64{0.013, 0.009, 0}, AvgW: []float64{60, 40, 0},
				TotalKWh: 0.1, TotalCost: 0.022},
		},
	}
}

func TestSeriesResponse_Rows(t *testing.T) {
	sr := sampleColumnar()
	rr := sr.Rows()

	if rr.Shape != ShapeRows {
		t.Errorf("shape = %q, want rows", rr.Shape)
	}
	// Headers carried through.
	if rr.Window != "today" || rr.Interval != "1h" || rr.GroupBy != "device" || rr.From != sr.From {
		t.Errorf("headers not carried: %+v", rr)
	}
	// Per-series metadata present (no value arrays), totals carried.
	if len(rr.Series) != 2 {
		t.Fatalf("series meta count = %d, want 2", len(rr.Series))
	}
	if rr.Series[0].Key != "winefridge" || rr.Series[0].Label != "Wine Fridge" || rr.Series[0].TotalKWh != 0.15 {
		t.Errorf("meta[0] = %+v", rr.Series[0])
	}
	// One row per (series, bucket): 2 series * 3 buckets.
	if len(rr.Rows) != 6 {
		t.Fatalf("rows = %d, want 6", len(rr.Rows))
	}
	// Ordering: series in response order, buckets contiguous and time-sorted.
	r0 := rr.Rows[0]
	if r0.Key != "winefridge" || !r0.Time.Equal(sr.Buckets[0]) || r0.KWh != 0.05 || r0.Cost != 0.011 || r0.AvgW != 52 {
		t.Errorf("rows[0] = %+v", r0)
	}
	if rr.Rows[2].Key != "winefridge" || !rr.Rows[2].Time.Equal(sr.Buckets[2]) || rr.Rows[2].KWh != 0.1 {
		t.Errorf("rows[2] = %+v", rr.Rows[2])
	}
	// Second series begins at index 3, back at bucket 0.
	if rr.Rows[3].Key != "bigfridge" || !rr.Rows[3].Time.Equal(sr.Buckets[0]) || rr.Rows[3].KWh != 0.06 {
		t.Errorf("rows[3] = %+v", rr.Rows[3])
	}
}

// Rows() must zero-fill defensively if a value array is shorter than Buckets
// (AssembleSeries guarantees equal length, but the reshape must not panic).
func TestSeriesResponse_Rows_ShortArrayZeroFills(t *testing.T) {
	t0 := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	sr := SeriesResponse{
		Shape:   ShapeColumns,
		Buckets: []time.Time{t0, t0.Add(time.Hour)},
		Series:  []Series{{Key: "x", KWh: []float64{0.5}, Cost: nil, AvgW: []float64{}}},
	}
	rr := sr.Rows()
	if len(rr.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rr.Rows))
	}
	if rr.Rows[0].KWh != 0.5 || rr.Rows[1].KWh != 0 || rr.Rows[1].Cost != 0 || rr.Rows[1].AvgW != 0 {
		t.Errorf("zero-fill failed: %+v", rr.Rows)
	}
}
