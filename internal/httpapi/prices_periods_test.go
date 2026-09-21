package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Issue #36 idea 4: aggregation on /prices/stats.
//
// The endpoint answered per day, which is right for "was shifting load worth it
// yesterday" and wrong for "how often do cheap slots occur in winter versus
// summer" — that needs ~24 numbers and gets ~730 rows, so every consumer asking
// it wrote the same client-side rollup.
// ---------------------------------------------------------------------------

type periodS struct {
	Period        string  `json:"period"`
	Days          int     `json:"days"`
	Slots         int     `json:"slots"`
	Min           float64 `json:"min"`
	Max           float64 `json:"max"`
	Mean          float64 `json:"mean"`
	Median        float64 `json:"median"`
	MeanSpread    float64 `json:"mean_spread"`
	PlungeSlots   int     `json:"plunge_slots"`
	NegativeSlots int     `json:"negative_slots"`
	CheapSlots    *int    `json:"cheap_slots"`
	CheapDays     *int    `json:"cheap_days"`
}

type statsPeriodsS struct {
	GroupBy    string    `json:"group_by"`
	CheapBelow *float64  `json:"cheap_below"`
	Periods    []periodS `json:"periods"`
	Days       []any     `json:"days"`
}

func getStats(t *testing.T, s *Server, path string) (statsPeriodsS, int, string) {
	t.Helper()
	w := doGET(t, s, path)
	var out statsPeriodsS
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
	}
	return out, w.Code, w.Body.String()
}

// The default is unchanged: days[], exactly as before.
func TestStats_DefaultsToTheDailyShape(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now.Add(-24*time.Hour), 20, 21, 22, -3, 60, 5))

	got, code, body := getStats(t, s, "/prices/stats?window=7d")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", code, body)
	}
	if len(got.Days) == 0 {
		t.Error("the daily shape must be untouched by default")
	}
	if got.Periods != nil {
		t.Error("periods[] should not appear without group_by=month")
	}
}

func TestStats_GroupByMonthRollsUp(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now.Add(-24*time.Hour), 20, 21, 22, -3, 60, 5))

	got, code, body := getStats(t, s, "/prices/stats?window=7d&group_by=month")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", code, body)
	}
	if got.GroupBy != "month" {
		t.Errorf("group_by = %q, want month", got.GroupBy)
	}
	if len(got.Periods) == 0 {
		t.Fatalf("no periods: %s", body)
	}
	if got.Days != nil {
		t.Error("days[] should not appear alongside periods[]")
	}
	p := got.Periods[0]
	if p.Slots == 0 || p.Days == 0 {
		t.Errorf("period carries no counts: %+v", p)
	}
	// The fixture has a negative slot.
	if p.NegativeSlots == 0 {
		t.Errorf("negative_slots = 0, but the fixture has a −3p slot: %+v", p)
	}
}

// "Cheap" is a policy, so the counts are absent until the caller says what it
// means.
func TestStats_CheapCountsRequireAThreshold(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now.Add(-24*time.Hour), 20, 21, 22, -3, 60, 5))

	got, _, _ := getStats(t, s, "/prices/stats?window=7d&group_by=month")
	if got.Periods[0].CheapSlots != nil || got.Periods[0].CheapDays != nil {
		t.Error("no threshold given, but cheap counts were invented")
	}
	if got.CheapBelow != nil {
		t.Error("cheap_below echoed without being asked for")
	}

	withThreshold, code, body := getStats(t, s, "/prices/stats?window=7d&group_by=month&cheap_below=10")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", code, body)
	}
	if withThreshold.CheapBelow == nil || *withThreshold.CheapBelow != 10 {
		t.Errorf("cheap_below = %v, want the threshold echoed back", withThreshold.CheapBelow)
	}
	p := withThreshold.Periods[0]
	if p.CheapSlots == nil || p.CheapDays == nil {
		t.Fatalf("threshold given but no cheap counts: %+v", p)
	}
	// The fixture has slots at 5 and −3 under 10p.
	if *p.CheapSlots < 2 {
		t.Errorf("cheap_slots = %d, want at least 2", *p.CheapSlots)
	}
	if *p.CheapDays < 1 {
		t.Errorf("cheap_days = %d, want at least 1", *p.CheapDays)
	}
}

func TestStats_RejectsAnUnknownGrouping(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now, 20, 21))
	if _, code, _ := getStats(t, s, "/prices/stats?window=7d&group_by=week"); code != http.StatusBadRequest {
		t.Errorf("want 400 for group_by=week, got %d", code)
	}
}

func TestStats_RejectsAMalformedThreshold(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now, 20, 21))
	if _, code, _ := getStats(t, s, "/prices/stats?window=7d&group_by=month&cheap_below=cheap"); code != http.StatusBadRequest {
		t.Errorf("want 400 for a non-numeric threshold, got %d", code)
	}
}

// A negative threshold is legal: "below zero" is a real question on a
// half-hourly tariff.
func TestStats_AcceptsANegativeThreshold(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now.Add(-24*time.Hour), 20, 21, 22, -3, 60, 5))

	got, code, body := getStats(t, s, "/prices/stats?window=7d&group_by=month&cheap_below=0")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", code, body)
	}
	if p := got.Periods[0]; p.CheapSlots == nil || *p.CheapSlots != p.PlungeSlots {
		t.Errorf("cheap_below=0 should count exactly the plunge slots: %+v", p)
	}
}
