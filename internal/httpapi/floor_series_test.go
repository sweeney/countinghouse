package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// floorSeriesSetup wires the floorplan inventory + records behind a series-aware
// fake querier. Energy: winefridge 0.05 and washingmachine 0.10 kWh/bucket on
// floor1, immersion 0.20 (whole-property), meter 1.0; network-ups is the
// integral path and comes from power.
func floorSeriesSetup(t *testing.T) *Server {
	t.Helper()
	fp := floorplanRecords()
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: floorplanDevices(), tariffs: testTariffs()}
	s.Floorplan = fp
	s.Influx = seriesFakeQuerier(todayHourBuckets(t),
		map[string]float64{
			"winefridge":        0.05,
			"washingmachine":    0.10,
			"immersion":         0.20,
			"electricity_meter": 1.0,
		},
		map[string]float64{"network-ups": 100.0},
	)
	return s
}

func seriesByKey(t *testing.T, r seriesResp) map[string]seriesS {
	t.Helper()
	out := map[string]seriesS{}
	for _, s := range r.Series {
		out[s.Key] = s
	}
	return out
}

// group_by=floor is the missing rung between room and house. Energy is additive,
// so a floor is the sum of its rooms — no group_fn, no argument about whether the
// combining statistic means anything.
func TestSeries_GroupByFloor(t *testing.T) {
	s := floorSeriesSetup(t)
	w := doGET(t, s, "/series?window=today&group_by=floor")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeSeries(t, w)
	if r.GroupBy != "floor" {
		t.Errorf("group_by = %q, want floor", r.GroupBy)
	}
	got := seriesByKey(t, r)

	f1, ok := got["floor1"]
	if !ok {
		t.Fatalf("no floor1 series: %+v", r.Series)
	}
	// 0.05 + 0.10 per bucket.
	if !approx(f1.KWh[0], 0.15) {
		t.Errorf("floor1 kwh[0] = %v, want 0.15", f1.KWh[0])
	}
	// Labelled with the floorplan's name, not the id.
	if f1.Label != "Floor One" {
		t.Errorf("floor1 label = %q, want Floor One", f1.Label)
	}
	if _, ok := got["floor2"]; !ok {
		t.Errorf("no floor2 series: %+v", r.Series)
	}
	// The meter is excluded — it measures what every floor consumes.
	if _, ok := got["electricity_meter"]; ok {
		t.Errorf("meter leaked into floor grouping: %+v", r.Series)
	}
	// A whole-property device belongs to no storey.
	if h, ok := got["house"]; !ok || !approx(h.KWh[0], 0.20) {
		t.Errorf("house series = %+v, want the immersion heater's 0.20", h)
	}
}

// The headline of issue #19: a legend rendering `label || key` showed
// "floor1.room-c" to a human, and a series that IS a room reported room: null,
// so it could not be joined to a room catalog.
func TestSeries_GroupByRoomCarriesNamesAndRoomIDs(t *testing.T) {
	s := floorSeriesSetup(t)
	w := doGET(t, s, "/series?window=today&group_by=room")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	got := seriesByKey(t, decodeSeries(t, w))

	a, ok := got["floor1.room-a"]
	if !ok {
		t.Fatalf("no floor1.room-a series: %+v", got)
	}
	if a.Label != "Room A" {
		t.Errorf("label = %q, want Room A", a.Label)
	}
	if a.Room != "floor1.room-a" {
		t.Errorf("room = %q, want floor1.room-a — a room series must be joinable to /rooms", a.Room)
	}
	if b := got["floor2.room-b"]; b.Label != "Room B" || b.Room != "floor2.room-b" {
		t.Errorf("room-b label/room = %q/%q, want Room B/floor2.room-b", b.Label, b.Room)
	}
}

// With no floorplan configured the key is still the label: countinghouse relays
// names and falls back to the id, never deriving one.
func TestSeries_GroupByRoomFallsBackToIDLabels(t *testing.T) {
	s := floorSeriesSetup(t)
	s.Floorplan = nil
	w := doGET(t, s, "/series?window=today&group_by=room")
	got := seriesByKey(t, decodeSeries(t, w))
	a, ok := got["floor1.room-a"]
	if !ok {
		t.Fatalf("no floor1.room-a series: %+v", got)
	}
	if a.Label != "floor1.room-a" {
		t.Errorf("label = %q, want the id as the fallback", a.Label)
	}
	if a.Room != "floor1.room-a" {
		t.Errorf("room = %q, want the id regardless of whether a name is published", a.Room)
	}
}

func TestSeries_InvalidGroupByListsFloor(t *testing.T) {
	s := floorSeriesSetup(t)
	w := doGET(t, s, "/series?group_by=nonsense")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "floor") {
		t.Errorf("error does not offer floor: %s", w.Body.String())
	}
}

// rooms= used to be accepted and ignored: a client asking for one room got a
// 200 and a whole-house answer while believing it had filtered.
func TestSeries_RoomsFilterNarrowsTheSeries(t *testing.T) {
	s := floorSeriesSetup(t)
	w := doGET(t, s, "/series?window=today&group_by=room&rooms=floor1.room-a")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeSeries(t, w)
	if len(r.Series) != 1 || r.Series[0].Key != "floor1.room-a" {
		t.Fatalf("series = %+v, want only floor1.room-a", r.Series)
	}
	// It filters DEVICES, so it narrows every grouping, not just group_by=room.
	w = doGET(t, s, "/series?window=today&group_by=device&rooms=floor1.room-a")
	got := seriesByKey(t, decodeSeries(t, w))
	if len(got) != 2 {
		t.Fatalf("device series = %v, want winefridge + washingmachine only", got)
	}
	if _, ok := got["network-ups"]; ok {
		t.Errorf("a floor2 device survived rooms=floor1.room-a: %v", got)
	}
}

func TestSeries_FloorsFilterNarrowsTheSeries(t *testing.T) {
	s := floorSeriesSetup(t)
	w := doGET(t, s, "/series?window=today&group_by=device&floors=floor2")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	got := seriesByKey(t, decodeSeries(t, w))
	if len(got) != 1 {
		t.Fatalf("series = %v, want the floor2 device only", got)
	}
	if _, ok := got["network-ups"]; !ok {
		t.Errorf("series = %v, want network-ups", got)
	}
}

// An unknown room or floor is a typo, not an empty chart. Silently returning
// everything (the old behaviour) or nothing both hide the client's mistake.
func TestSeries_UnknownRoomOrFloorRejected(t *testing.T) {
	s := floorSeriesSetup(t)
	for _, q := range []string{
		"/series?group_by=room&rooms=floor9.nowhere",
		"/series?group_by=room&floors=floor9",
		// "house" is a coverage scope and a reserved series key, never a room id.
		"/series?group_by=room&rooms=house",
		// A room that exists in the floorplan but holds nothing this service
		// bills for does not exist as far as the energy API is concerned — and
		// /rooms does not list it, so a picker cannot produce it.
		"/series?group_by=room&rooms=floor1.room-c",
	} {
		w := doGET(t, s, q)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d: %s", q, w.Code, w.Body.String())
		}
	}
}

// Filters compose as AND. An intersection that is legally empty is a 200 with no
// series — the same answer as a window with no data — not an error.
func TestSeries_FiltersComposeAsAnd(t *testing.T) {
	s := floorSeriesSetup(t)
	w := doGET(t, s, "/series?window=today&group_by=device&rooms=floor1.room-a&floors=floor2")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if r := decodeSeries(t, w); len(r.Series) != 0 {
		t.Errorf("series = %+v, want none: no device is both in room-a and on floor2", r.Series)
	}
}

// The unmonitored catch-all is meter − ALL monitored devices: a house-scoped
// quantity. Against a filtered set it would silently absorb every device the
// filter excluded and report it as "rest of home", so the contradiction is
// rejected rather than answered wrongly.
func TestSeries_FiltersRejectedWithIncludeUnmonitored(t *testing.T) {
	s := floorSeriesSetup(t)
	for _, q := range []string{
		"/series?group_by=room&rooms=floor1.room-a&include_unmonitored=true",
		"/series?group_by=floor&floors=floor1&include_unmonitored=true",
	} {
		w := doGET(t, s, q)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d: %s", q, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "unmonitored") {
			t.Errorf("%s: error should explain the unmonitored conflict: %s", q, w.Body.String())
		}
	}
}

// group_by=house decomposes the WHOLE property into monitored/unmonitored/meter.
// Filtering the device set would shrink `monitored` while the meter stayed whole,
// inflating `unmonitored` by the excluded devices' consumption.
func TestSeries_FiltersRejectedWithGroupByHouse(t *testing.T) {
	s := floorSeriesSetup(t)
	w := doGET(t, s, "/series?group_by=house&rooms=floor1.room-a")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

// The stated choice for the wrinkle in issue #19: with include_unmonitored=true,
// the house-scoped residual is returned as its OWN series and is never attributed
// to a floor — exactly as group_by=room already handles it.
func TestSeries_GroupByFloorUnmonitoredIsItsOwnSeries(t *testing.T) {
	s := floorSeriesSetup(t)
	w := doGET(t, s, "/series?window=today&group_by=floor&include_unmonitored=true")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	got := seriesByKey(t, decodeSeries(t, w))
	u, ok := got["unmonitored"]
	if !ok {
		t.Fatalf("no unmonitored series: %+v", got)
	}
	if u.Room != "" {
		t.Errorf("unmonitored room = %q, want empty: it belongs to no place", u.Room)
	}
	// meter 1.0 − monitored (0.05 + 0.10 + 0.20 + ups) per bucket, and it is one
	// series regardless of how many floors there are.
	if u.KWh[0] <= 0 {
		t.Errorf("unmonitored kwh[0] = %v, want the positive residual", u.KWh[0])
	}
	if _, ok := got["floor1"]; !ok {
		t.Errorf("floor series lost when the catch-all was added: %+v", got)
	}
}

// When a request is BOTH invalid and contradictory, say both. Reporting only the
// conflict sends the operator to fix one thing, retry, and meet a second 400 for
// a typo that was visible the whole time.
func TestSeries_ReportsAnUnknownIDAndTheConflictTogether(t *testing.T) {
	s := floorSeriesSetup(t)
	w := doGET(t, s, "/series?group_by=room&rooms=floor9.typo&include_unmonitored=true")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "floor9.typo") {
		t.Errorf("the typo is invisible in: %s", body)
	}
	if !strings.Contains(body, "unmonitored") {
		t.Errorf("the conflict is invisible in: %s", body)
	}
}

// A value that is entirely separators is a client bug — almost always a join that
// produced nothing — and reading it as "no filter" answers with the whole house.
// That is the same silent widening the 400s here exist to remove.
func TestSeries_RejectsAFilterOfOnlySeparators(t *testing.T) {
	s := floorSeriesSetup(t)
	for _, q := range []string{
		"/series?group_by=room&rooms=,,",
		"/series?group_by=room&rooms=+%20+",
		"/series?group_by=device&floors=,",
	} {
		w := doGET(t, s, q)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d: %s", q, w.Code, w.Body.String())
		}
	}
}

// A bare `rooms=` is different: a client building the value from an empty
// selection means "no filter", and gets the unfiltered answer it asked for.
func TestSeries_BareEmptyFilterIsNoFilter(t *testing.T) {
	s := floorSeriesSetup(t)
	w := doGET(t, s, "/series?window=today&group_by=room&rooms=&floors=")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := len(decodeSeries(t, w).Series); got < 2 {
		t.Errorf("series = %d, want the unfiltered set", got)
	}
}
