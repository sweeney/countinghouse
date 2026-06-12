package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// rowsResp mirrors the energy.RowsResponse wire shape.
type rowsResp struct {
	Window   string `json:"window"`
	Interval string `json:"interval"`
	GroupBy  string `json:"group_by"`
	Shape    string `json:"shape"`
	Buckets  []any  `json:"buckets"` // must be absent in rows shape
	Series   []struct {
		Key      string  `json:"key"`
		Label    string  `json:"label"`
		TotalKWh float64 `json:"total_kwh"`
	} `json:"series"`
	Rows []struct {
		Key  string  `json:"key"`
		Time string  `json:"time"`
		KWh  float64 `json:"kwh"`
		Cost float64 `json:"cost"`
		AvgW float64 `json:"avg_w"`
	} `json:"rows"`
}

func decodeRows(t *testing.T, w *httptest.ResponseRecorder) rowsResp {
	t.Helper()
	var r rowsResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode rows %q: %v", w.Body.String(), err)
	}
	return r
}

func TestSeries_ShapeRows(t *testing.T) {
	energyPer := map[string]float64{"winefridge": 0.05}
	powerPer := map[string]float64{"winefridge": 52.0, "network-ups": 100.0}
	s := seriesSetup(t, energyPer, powerPer)

	w := doGET(t, s, "/series?window=today&group_by=device&shape=rows")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeRows(t, w)

	if r.Shape != "rows" {
		t.Errorf("shape = %q, want rows", r.Shape)
	}
	if r.Interval != "1h" || r.GroupBy != "device" {
		t.Errorf("headers wrong: %+v", r)
	}
	if len(r.Buckets) != 0 {
		t.Errorf("rows shape must not carry a buckets axis, got %d", len(r.Buckets))
	}
	// Two metered non-meter devices (winefridge, network-ups), 14 buckets each.
	if len(r.Series) != 2 {
		t.Fatalf("series meta = %d, want 2", len(r.Series))
	}
	if len(r.Rows) != 28 {
		t.Fatalf("rows = %d, want 28 (2 series x 14 buckets)", len(r.Rows))
	}
	// Every row is self-describing: key + time present.
	for i, row := range r.Rows {
		if row.Key == "" || row.Time == "" {
			t.Fatalf("row %d missing key/time: %+v", i, row)
		}
	}
	// Rows are contiguous per series (series 0's 14 rows, then series 1's).
	if r.Rows[0].Key != r.Rows[13].Key {
		t.Errorf("first 14 rows should be one series: %q..%q", r.Rows[0].Key, r.Rows[13].Key)
	}
	if r.Rows[0].Key == r.Rows[14].Key {
		t.Errorf("row 14 should start the second series, still %q", r.Rows[14].Key)
	}
}

func TestDeviceSeries_ShapeRows(t *testing.T) {
	energyPer := map[string]float64{"winefridge": 0.05}
	powerPer := map[string]float64{"winefridge": 52.0}
	s := seriesSetup(t, energyPer, powerPer)

	w := doGET(t, s, "/devices/winefridge/series?window=today&shape=rows")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeRows(t, w)
	if r.Shape != "rows" {
		t.Errorf("shape = %q, want rows", r.Shape)
	}
	if len(r.Series) != 1 || r.Series[0].Key != "winefridge" {
		t.Fatalf("series meta = %+v, want one winefridge", r.Series)
	}
	if len(r.Rows) != 14 {
		t.Fatalf("rows = %d, want 14", len(r.Rows))
	}
}

func TestSeries_DefaultShapeIsColumns(t *testing.T) {
	s := seriesSetup(t, map[string]float64{"winefridge": 0.05}, map[string]float64{"winefridge": 52.0})
	// No shape param → columnar (has a buckets axis, shape="columns").
	w := doGET(t, s, "/series?window=today&group_by=device")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["shape"] != "columns" {
		t.Errorf("default shape = %v, want columns", m["shape"])
	}
	if _, ok := m["buckets"]; !ok {
		t.Errorf("columnar response must carry a buckets axis")
	}
}

func TestSeries_BadShape(t *testing.T) {
	s := seriesSetup(t, map[string]float64{"winefridge": 0.05}, map[string]float64{"winefridge": 52.0})
	for _, path := range []string{
		"/series?window=today&shape=wide",
		"/devices/winefridge/series?window=today&shape=tidy",
	} {
		w := doGET(t, s, path)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d: %s", path, w.Code, w.Body.String())
		}
	}
}
