package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
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

// The cap message used to splice a noun into a fixed sentence, which read
// correctly for /prices and garbled here: "one of its daily rows per half hour
// or per day". These messages were singled out in issue #36 as unusually good.
func TestStats_CapMessageReadsCleanly(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now, 20, 21))

	w := doGET(t, s, "/prices/stats?window=custom&from=2020-01-01T00:00:00Z&to=2024-01-01T00:00:00Z")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 over the cap, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "daily rows per half hour") {
		t.Errorf("the spliced-noun garble is back: %s", body)
	}
	if !strings.Contains(body, "a row per day") {
		t.Errorf("want the day-grouping clause, got: %s", body)
	}
}

// The cap is grouping-dependent, because the rationale is. 700 days at month
// grouping is 24 rows, so "an unbounded window is an unbounded response" is true
// of the day grouping and false of the month one — and a caller refused at the
// day cap has no way to learn the same window answers one grouping over unless
// the wire says the constraint depends on it.
func TestStats_MonthGroupingHasItsOwnCap(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now, 20, 21))

	const twoYears = "window=custom&from=2024-01-01T00:00:00Z&to=2026-01-01T00:00:00Z"

	// Refused at day grouping: 731 rows.
	w := doGET(t, s, "/prices/stats?"+twoYears)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("two years of daily rows should exceed the cap, got %d", w.Code)
	}
	var body struct {
		Limits struct {
			GroupBy string `json:"group_by"`
			MaxDays int    `json:"max_days"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The wire says which grouping the cap applied to, so the refusal is
	// actionable rather than final.
	if body.Limits.GroupBy != "day" {
		t.Errorf("limits.group_by = %q, want day", body.Limits.GroupBy)
	}
	if body.Limits.MaxDays != 366 {
		t.Errorf("limits.max_days = %d, want 366 at day grouping", body.Limits.MaxDays)
	}

	// The same window gets PAST the cap at month grouping: 24 rows, not 731.
	//
	// It then meets this fixture's own agreement boundary (the configured tariff
	// starts in 2026), which is a different and correct refusal — so the
	// assertion is that the CAP no longer fires, not that the request succeeds.
	// Asserting 200 here would be asserting something about the fixture's
	// agreements rather than about the cap.
	w2 := doGET(t, s, "/prices/stats?"+twoYears+"&group_by=month")
	if w2.Code == http.StatusBadRequest && strings.Contains(w2.Body.String(), "over the cap") {
		t.Errorf("two years of MONTHLY rows is 24 rows and must clear the cap, got: %s",
			w2.Body.String())
	}
}

// Still bounded, just by the archive read rather than the response.
func TestStats_MonthGroupingIsStillCapped(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now, 20, 21))

	w := doGET(t, s,
		"/prices/stats?window=custom&from=2000-01-01T00:00:00Z&to=2026-01-01T00:00:00Z&group_by=month")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("26 years should still be refused, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "a row per month") {
		t.Errorf("want the month-grouping clause, got: %s", w.Body.String())
	}
}
