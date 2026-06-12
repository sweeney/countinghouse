package energy

import "time"

// Series payload shapes, selectable by the `shape` query param on the series
// endpoints.
//
//   - ShapeColumns (default): the columnar SeriesResponse — a shared Buckets axis
//     plus per-series value arrays. Ideal for web charting libraries, where each
//     array drops straight into a dataset.
//   - ShapeRows: the row-oriented RowsResponse — a flat list of one row per
//     (series, bucket). Idiomatic for Codable consumers (decode []SeriesPoint)
//     and grouped native charts, e.g. Swift Charts' foregroundStyle(by: key).
const (
	ShapeColumns = "columns"
	ShapeRows    = "rows"
)

// ValidShape reports whether s is an accepted shape ("" defaults to columns).
func ValidShape(s string) bool {
	return s == "" || s == ShapeColumns || s == ShapeRows
}

// SeriesPoint is one (series, bucket) sample in the row-oriented form. Values are
// already rounded (kWh 3dp, cost 4dp GBP, avg_w 1dp W) by AssembleSeries.
type SeriesPoint struct {
	Key  string    `json:"key"`  // series key (device id, location, class, or "monitored"/"meter")
	Time time.Time `json:"time"` // bucket start, RFC3339 with the configured tz offset
	KWh  float64   `json:"kwh"`
	Cost float64   `json:"cost"`  // GBP, VAT-inclusive
	AvgW float64   `json:"avg_w"` // mean power over the bucket, watts
}

// SeriesMeta is per-series metadata (no per-bucket arrays) carried alongside the
// rows so consumers still have labels and totals for legends without scanning the
// row list.
type SeriesMeta struct {
	Key       string  `json:"key"`
	Label     string  `json:"label"`
	Location  string  `json:"location,omitempty"`
	Class     string  `json:"class,omitempty"`
	TotalKWh  float64 `json:"total_kwh"`
	TotalCost float64 `json:"total_cost"`
}

// RowsResponse is the row-oriented ("tidy"/long) form of a SeriesResponse: one
// flat row per (series, bucket), plus lightweight per-series metadata. Rows are
// ordered by series (in the columnar response's series order) then by bucket
// time, so each series' points are contiguous and already time-sorted.
type RowsResponse struct {
	Window   string        `json:"window"`
	From     string        `json:"from"`
	To       string        `json:"to"`
	Interval string        `json:"interval"`
	GroupBy  string        `json:"group_by"`
	Shape    string        `json:"shape"` // "rows"
	Series   []SeriesMeta  `json:"series"`
	Rows     []SeriesPoint `json:"rows"`
}

// Rows converts the columnar SeriesResponse into the row-oriented RowsResponse.
// It is a pure reshape: values are copied as-is (already rounded), and every
// series contributes exactly len(Buckets) rows (missing entries are zero-filled,
// matching the columnar invariant that all arrays align to the axis).
func (r SeriesResponse) Rows() RowsResponse {
	out := RowsResponse{
		Window:   r.Window,
		From:     r.From,
		To:       r.To,
		Interval: r.Interval,
		GroupBy:  r.GroupBy,
		Shape:    ShapeRows,
		Series:   make([]SeriesMeta, 0, len(r.Series)),
		Rows:     make([]SeriesPoint, 0, len(r.Series)*len(r.Buckets)),
	}
	for _, s := range r.Series {
		out.Series = append(out.Series, SeriesMeta{
			Key:       s.Key,
			Label:     s.Label,
			Location:  s.Location,
			Class:     s.Class,
			TotalKWh:  s.TotalKWh,
			TotalCost: s.TotalCost,
		})
		for i, t := range r.Buckets {
			out.Rows = append(out.Rows, SeriesPoint{
				Key:  s.Key,
				Time: t,
				KWh:  at(s.KWh, i),
				Cost: at(s.Cost, i),
				AvgW: at(s.AvgW, i),
			})
		}
	}
	return out
}

// at returns a[i] or 0 if i is out of range (defensive: arrays should already be
// len(Buckets) after AssembleSeries' zero-fill).
func at(a []float64, i int) float64 {
	if i >= 0 && i < len(a) {
		return a[i]
	}
	return 0
}
