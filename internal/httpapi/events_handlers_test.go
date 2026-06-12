package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/influx"
)

// --- activity fake ---

// activityFakeQuerier programs a FakeQuerier to answer the two queries
// events.BuildTimeline issues: the in-window activity query and the carry-in
// query. We distinguish them by the carry-in builder's epoch range start.
//
// activity maps device_id → ordered transitions inside the window (each a
// from/to label pair at a time); carryIn maps device_id → the raw `to` label of
// the last transition before the window (the carried-in opening state, "" if
// none).
func activityFakeQuerier(activity map[string][]transition, carryIn map[string]string) *influx.FakeQuerier {
	q := &influx.FakeQuerier{PingOK: true}
	q.QueryFunc = func(flux string) ([]influx.Row, error) {
		isCarryIn := strings.Contains(flux, "range(start: 1970-01-01T00:00:00Z")
		var rows []influx.Row
		if isCarryIn {
			for id, label := range carryIn {
				if !strings.Contains(flux, `"`+id+`"`) {
					continue
				}
				// One from/to pair (the carry-in only cares about `to`).
				rows = append(rows,
					influx.Row{DeviceID: id, Class: "binary_state_device", Field: "from", Text: "", Time: time.Unix(0, 0)},
					influx.Row{DeviceID: id, Class: "binary_state_device", Field: "to", Text: label, Time: time.Unix(0, 0)},
				)
			}
			return rows, nil
		}
		for id, trs := range activity {
			if !strings.Contains(flux, `"`+id+`"`) {
				continue
			}
			for _, tr := range trs {
				rows = append(rows,
					influx.Row{DeviceID: id, Class: "binary_state_device", Field: "from", Text: tr.from, Time: tr.t},
					influx.Row{DeviceID: id, Class: "binary_state_device", Field: "to", Text: tr.to, Time: tr.t},
				)
			}
		}
		return rows, nil
	}
	return q
}

type transition struct {
	from, to string
	t        time.Time
}

// londonTime builds a 2026-06-11 BST instant for fixtures.
func londonTime(t *testing.T, h, m, s int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return time.Date(2026, 6, 11, h, m, s, 0, loc)
}

// eventsSetup wires a Server with the activity fake installed.
func eventsSetup(t *testing.T, activity map[string][]transition, carryIn map[string]string) *Server {
	t.Helper()
	s, _ := dataSetup(t)
	s.Influx = activityFakeQuerier(activity, carryIn)
	return s
}

// --- decode helpers ---

type wireEvent struct {
	Time string `json:"time"`
	From string `json:"from"`
	To   string `json:"to"`
	On   *bool  `json:"on"`
}

type deviceEventsResp struct {
	DeviceID string      `json:"device_id"`
	Window   string      `json:"window"`
	From     string      `json:"from"`
	To       string      `json:"to"`
	Events   []wireEvent `json:"events"`
}

func decodeDeviceEvents(t *testing.T, w *httptest.ResponseRecorder) deviceEventsResp {
	t.Helper()
	var r deviceEventsResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return r
}

type wireInterval struct {
	Start     string  `json:"start"`
	End       *string `json:"end"`
	State     string  `json:"state"`
	On        *bool   `json:"on"`
	DurationS float64 `json:"duration_s"`
	Open      bool    `json:"open"`
}

type deviceIntervalsResp struct {
	DeviceID     string         `json:"device_id"`
	StateAtStart string         `json:"state_at_start"`
	Intervals    []wireInterval `json:"intervals"`
	Stats        struct {
		OnCount        int     `json:"on_count"`
		TotalOnSeconds float64 `json:"total_on_seconds"`
		Duty           float64 `json:"duty"`
	} `json:"stats"`
}

func decodeDeviceIntervals(t *testing.T, w *httptest.ResponseRecorder) deviceIntervalsResp {
	t.Helper()
	var r deviceIntervalsResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return r
}

type eventsResp struct {
	Window  string `json:"window"`
	GroupBy string `json:"group_by"`
	Series  []struct {
		Key    string      `json:"key"`
		Label  string      `json:"label"`
		Class  string      `json:"class"`
		Events []wireEvent `json:"events"`
	} `json:"series"`
}

func decodeEvents(t *testing.T, w *httptest.ResponseRecorder) eventsResp {
	t.Helper()
	var r eventsResp
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return r
}

// --- /devices/{id}/events ---

func TestDeviceEvents_OrderedEdges(t *testing.T) {
	act := map[string][]transition{
		"hot_water": {
			{from: "idle", to: "active", t: londonTime(t, 5, 30, 1)},
			{from: "active", to: "idle", t: londonTime(t, 6, 15, 0)},
		},
	}
	s := eventsSetup(t, act, nil)
	w := doGET(t, s, "/devices/hot_water/events?window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeDeviceEvents(t, w)
	if r.DeviceID != "hot_water" || r.Window != "today" {
		t.Errorf("device_id/window = %q/%q", r.DeviceID, r.Window)
	}
	if len(r.Events) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(r.Events), r.Events)
	}
	if r.Events[0].To != "active" || r.Events[0].On == nil || !*r.Events[0].On {
		t.Errorf("event[0] = %+v want active/on=true", r.Events[0])
	}
	if r.Events[1].To != "idle" || r.Events[1].On == nil || *r.Events[1].On {
		t.Errorf("event[1] = %+v want idle/on=false", r.Events[1])
	}
	// Ordered ascending (RFC3339 strings sort chronologically).
	if r.Events[0].Time >= r.Events[1].Time {
		t.Errorf("events not ordered: %v then %v", r.Events[0].Time, r.Events[1].Time)
	}
}

func TestDeviceEvents_Unknown(t *testing.T) {
	s := eventsSetup(t, nil, nil)
	w := doGET(t, s, "/devices/nope/events?window=today")
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeviceEvents_Empty(t *testing.T) {
	// No activity for hot_water in the window → 200 with empty [].
	s := eventsSetup(t, nil, nil)
	w := doGET(t, s, "/devices/hot_water/events?window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeDeviceEvents(t, w)
	if r.Events == nil {
		t.Errorf("events should be [] not null: %s", w.Body.String())
	}
	if len(r.Events) != 0 {
		t.Errorf("want 0 events, got %d", len(r.Events))
	}
	// The wire form must carry an empty array literal, never null.
	if strings.Contains(w.Body.String(), `"events":null`) {
		t.Errorf("events serialised as null: %s", w.Body.String())
	}
}

func TestDeviceEvents_InfluxError(t *testing.T) {
	s := eventsSetup(t, nil, nil)
	s.Influx = &influx.FakeQuerier{PingOK: true, Err: errFake}
	w := doGET(t, s, "/devices/hot_water/events?window=today")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", w.Code, w.Body.String())
	}
}

// --- /devices/{id}/intervals ---

func TestDeviceIntervals_Basic(t *testing.T) {
	act := map[string][]transition{
		"hot_water": {
			{from: "idle", to: "active", t: londonTime(t, 5, 30, 1)},
			{from: "active", to: "idle", t: londonTime(t, 6, 15, 0)},
		},
	}
	s := eventsSetup(t, act, map[string]string{"hot_water": "idle"})
	w := doGET(t, s, "/devices/hot_water/intervals?window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeDeviceIntervals(t, w)
	if r.StateAtStart != "idle" {
		t.Errorf("state_at_start = %q want idle", r.StateAtStart)
	}
	if len(r.Intervals) != 1 {
		t.Fatalf("want 1 interval, got %d: %+v", len(r.Intervals), r.Intervals)
	}
	iv := r.Intervals[0]
	if iv.End == nil || iv.Open {
		t.Errorf("interval should be closed: %+v", iv)
	}
	// 05:30:01 -> 06:15:00 = 2699s.
	if iv.DurationS != 2699 {
		t.Errorf("duration_s = %v want 2699", iv.DurationS)
	}
	if r.Stats.OnCount != 1 || r.Stats.TotalOnSeconds != 2699 {
		t.Errorf("stats = %+v want on_count 1 / total 2699", r.Stats)
	}
	if r.Stats.Duty <= 0 {
		t.Errorf("duty should be > 0: %v", r.Stats.Duty)
	}
}

func TestDeviceIntervals_CarryInOpensFirst(t *testing.T) {
	// Device was ON at window start (carry-in active), turns off mid-window:
	// the first interval opens at the window start (00:00 BST).
	act := map[string][]transition{
		"hot_water": {
			{from: "active", to: "idle", t: londonTime(t, 2, 0, 0)},
		},
	}
	s := eventsSetup(t, act, map[string]string{"hot_water": "active"})
	w := doGET(t, s, "/devices/hot_water/intervals?window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeDeviceIntervals(t, w)
	if r.StateAtStart != "active" {
		t.Errorf("state_at_start = %q want active", r.StateAtStart)
	}
	if len(r.Intervals) != 1 {
		t.Fatalf("want 1 interval, got %d: %+v", len(r.Intervals), r.Intervals)
	}
	iv := r.Intervals[0]
	if iv.Open {
		t.Errorf("first interval should be closed by 02:00 transition: %+v", iv)
	}
	// Opens clamped to window start 00:00 BST, closes 02:00 BST = 7200s.
	if iv.DurationS != 7200 {
		t.Errorf("duration_s = %v want 7200 (00:00..02:00)", iv.DurationS)
	}
	if !strings.HasPrefix(iv.Start, "2026-06-11T00:00:00") {
		t.Errorf("interval start not clamped to window start: %q", iv.Start)
	}
}

func TestDeviceIntervals_OpenTrailing(t *testing.T) {
	// Turns on mid-window and never off: trailing interval is open (end null).
	act := map[string][]transition{
		"hot_water": {
			{from: "idle", to: "active", t: londonTime(t, 13, 0, 0)},
		},
	}
	s := eventsSetup(t, act, map[string]string{"hot_water": "idle"})
	w := doGET(t, s, "/devices/hot_water/intervals?window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeDeviceIntervals(t, w)
	if len(r.Intervals) != 1 {
		t.Fatalf("want 1 interval, got %d: %+v", len(r.Intervals), r.Intervals)
	}
	iv := r.Intervals[0]
	if !iv.Open || iv.End != nil {
		t.Errorf("trailing interval should be open with null end: %+v", iv)
	}
	// 13:00 BST .. now (14:00 BST) = 3600s.
	if iv.DurationS != 3600 {
		t.Errorf("duration_s = %v want 3600 (13:00..now 14:00)", iv.DurationS)
	}
}

// --- /events ---

func TestEvents_DefaultBinarySet(t *testing.T) {
	act := map[string][]transition{
		"hot_water": {{from: "idle", to: "active", t: londonTime(t, 5, 0, 0)}},
		"boiler":    {{from: "idle", to: "active", t: londonTime(t, 6, 0, 0)}},
	}
	s := eventsSetup(t, act, nil)
	w := doGET(t, s, "/events?window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeEvents(t, w)
	if r.GroupBy != "device" {
		t.Errorf("group_by = %q want device", r.GroupBy)
	}
	keys := map[string]bool{}
	for _, ser := range r.Series {
		keys[ser.Key] = true
	}
	// Default set is exactly the binary_state_device devices.
	if !keys["hot_water"] || !keys["boiler"] {
		t.Errorf("default set missing binary devices: %v", keys)
	}
	if keys["winefridge"] || keys["electricity_meter"] || keys["hallway-sensor"] {
		t.Errorf("default set should be binary only: %v", keys)
	}
	if len(r.Series) != 2 {
		t.Fatalf("want 2 series, got %d", len(r.Series))
	}
	// Sorted by id: boiler before hot_water.
	if r.Series[0].Key != "boiler" || r.Series[1].Key != "hot_water" {
		t.Errorf("series not sorted by id: %q, %q", r.Series[0].Key, r.Series[1].Key)
	}
	if r.Series[1].Label != "Hot Water" {
		t.Errorf("hot_water label = %q want Hot Water", r.Series[1].Label)
	}
}

func TestEvents_ExplicitDevices(t *testing.T) {
	act := map[string][]transition{
		"hot_water": {{from: "idle", to: "active", t: londonTime(t, 5, 0, 0)}},
		"boiler":    {{from: "idle", to: "active", t: londonTime(t, 6, 0, 0)}},
	}
	s := eventsSetup(t, act, nil)
	w := doGET(t, s, "/events?window=today&devices=hot_water")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeEvents(t, w)
	if len(r.Series) != 1 || r.Series[0].Key != "hot_water" {
		t.Fatalf("want only hot_water, got %+v", r.Series)
	}
	if len(r.Series[0].Events) != 1 {
		t.Errorf("want 1 event, got %d", len(r.Series[0].Events))
	}
}

func TestEvents_UnknownDeviceInList(t *testing.T) {
	s := eventsSetup(t, nil, nil)
	w := doGET(t, s, "/events?window=today&devices=hot_water,ghost")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(decode(t, w)["error"].(string), "ghost") {
		t.Errorf("error should name the unknown device: %s", w.Body.String())
	}
}

func TestEvents_ClassFilter(t *testing.T) {
	act := map[string][]transition{
		"hot_water": {{from: "idle", to: "active", t: londonTime(t, 5, 0, 0)}},
		"boiler":    {{from: "idle", to: "active", t: londonTime(t, 6, 0, 0)}},
	}
	s := eventsSetup(t, act, nil)
	w := doGET(t, s, "/events?window=today&class=binary_state_device")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeEvents(t, w)
	if len(r.Series) != 2 {
		t.Fatalf("want 2 series for class, got %d", len(r.Series))
	}

	// A class with no devices yields an empty (but non-null) series array.
	w2 := doGET(t, s, "/events?window=today&class=nonexistent_class")
	if w2.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w2.Code)
	}
	r2 := decodeEvents(t, w2)
	if r2.Series == nil {
		t.Errorf("series should be [] not null: %s", w2.Body.String())
	}
	if len(r2.Series) != 0 {
		t.Errorf("want 0 series, got %d", len(r2.Series))
	}
}

func TestEvents_GroupByClassMerges(t *testing.T) {
	act := map[string][]transition{
		"hot_water": {{from: "idle", to: "active", t: londonTime(t, 6, 0, 0)}},
		"boiler":    {{from: "idle", to: "active", t: londonTime(t, 5, 0, 0)}},
	}
	s := eventsSetup(t, act, nil)
	w := doGET(t, s, "/events?window=today&group_by=class")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	r := decodeEvents(t, w)
	if r.GroupBy != "class" {
		t.Errorf("group_by = %q want class", r.GroupBy)
	}
	if len(r.Series) != 1 {
		t.Fatalf("want 1 merged class series, got %d: %+v", len(r.Series), r.Series)
	}
	ser := r.Series[0]
	if ser.Key != "binary_state_device" {
		t.Errorf("class key = %q", ser.Key)
	}
	// Both devices' events merged and time-ordered: boiler (05:00) before
	// hot_water (06:00).
	if len(ser.Events) != 2 {
		t.Fatalf("want 2 merged events, got %d", len(ser.Events))
	}
	if ser.Events[0].Time >= ser.Events[1].Time {
		t.Errorf("merged events not time-ordered: %v then %v", ser.Events[0].Time, ser.Events[1].Time)
	}
}

func TestEvents_BadGroupBy(t *testing.T) {
	s := eventsSetup(t, nil, nil)
	w := doGET(t, s, "/events?window=today&group_by=galaxy")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEvents_InfluxError(t *testing.T) {
	s := eventsSetup(t, nil, nil)
	s.Influx = &influx.FakeQuerier{PingOK: true, Err: errFake}
	w := doGET(t, s, "/events?window=today")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", w.Code, w.Body.String())
	}
}

// --- /devices catalog ---

func TestDevicesCatalog(t *testing.T) {
	s, _ := dataSetup(t)
	w := doGET(t, s, "/devices")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var doc struct {
		Devices []struct {
			ID           string   `json:"id"`
			DisplayName  string   `json:"display_name"`
			Location     string   `json:"location"`
			Class        string   `json:"class"`
			Capabilities []string `json:"capabilities"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byID := map[string]struct {
		class string
		caps  []string
	}{}
	var ids []string
	for _, d := range doc.Devices {
		byID[d.ID] = struct {
			class string
			caps  []string
		}{d.Class, d.Capabilities}
		ids = append(ids, d.ID)
	}

	// Sorted by id.
	for i := 1; i < len(ids); i++ {
		if ids[i-1] > ids[i] {
			t.Errorf("catalog not sorted: %v", ids)
			break
		}
	}

	// Metered plug → energy.
	if got := byID["winefridge"].caps; len(got) != 1 || got[0] != "energy" {
		t.Errorf("winefridge caps = %v want [energy]", got)
	}
	// UPS is metered (integral) → energy.
	if got := byID["network-ups"].caps; len(got) != 1 || got[0] != "energy" {
		t.Errorf("network-ups caps = %v want [energy]", got)
	}
	// Energy meter → energy.
	if got := byID["electricity_meter"].caps; len(got) != 1 || got[0] != "energy" {
		t.Errorf("electricity_meter caps = %v want [energy]", got)
	}
	// Binary device → events (not energy).
	if got := byID["hot_water"].caps; len(got) != 1 || got[0] != "events" {
		t.Errorf("hot_water caps = %v want [events]", got)
	}
	// Non-metered, non-binary → empty capabilities (but never null).
	if got := byID["hallway-sensor"].caps; got == nil || len(got) != 0 {
		t.Errorf("hallway-sensor caps = %v want []", got)
	}
	if strings.Contains(w.Body.String(), `"capabilities":null`) {
		t.Errorf("capabilities serialised as null: %s", w.Body.String())
	}
}

// Regression: fire_alarm devices emit device_activity events but were not flagged
// events-capable (only binary_state_device was), making them undiscoverable to a
// consumer picking overlay devices by capability.
func TestDevicesCatalog_FireAlarmIsEventsCapable(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{
		devices: map[string]config.DeviceConfig{
			"firealarm_office": {Class: "fire_alarm", Location: "office", DisplayName: "Office Fire Alarm"},
			"climate_x":        {Class: "environmental_sensor", Location: "hall", DisplayName: "Sensor"},
		},
		tariffs: testTariffs(),
	}
	w := doGET(t, s, "/devices")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var doc struct {
		Devices []struct {
			ID           string   `json:"id"`
			Capabilities []string `json:"capabilities"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	caps := map[string][]string{}
	for _, d := range doc.Devices {
		caps[d.ID] = d.Capabilities
	}
	if got := caps["firealarm_office"]; len(got) != 1 || got[0] != "events" {
		t.Errorf("fire_alarm caps = %v, want [events]", got)
	}
	if got := caps["climate_x"]; len(got) != 0 {
		t.Errorf("environmental_sensor caps = %v, want []", got)
	}
}

// --- auth applies to the new routes ---

func TestEventsRoutes_RequireAuth(t *testing.T) {
	priv := genTestKey(t)
	kid := "testkey"
	fakeID := fakeJWKSServer(t, &priv.PublicKey, kid)
	s := eventsSetup(t, nil, nil)
	s.IdentityURL = fakeID.URL
	mux := newMux(s)

	for _, path := range []string{
		"/devices",
		"/devices/hot_water/events?window=today",
		"/devices/hot_water/intervals?window=today",
		"/events?window=today",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s no-token: want 401, got %d", path, w.Code)
		}
	}
}
