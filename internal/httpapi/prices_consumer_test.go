package httpapi

import (
	"net/http"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Three choices revisited for the consumer's sake, while these routes still have
// no consumers. Each was defensible in isolation and each cost a reader
// something: a band that could not be reproduced, a VAT basis that differed from
// every sibling route, and a "today" that stopped at lunchtime.
// ---------------------------------------------------------------------------

// The banding centre must be SERVED, not merely described. A consumer that
// dislikes our thresholds is told to derive its own from the summary — which is
// impossible when the summary omits the figure the bands are actually measured
// from. min/max/mean are all present; the median, the one that matters, was not.
func TestCurveSummaryCarriesTheBandingCentre(t *testing.T) {
	now := pxNow(t)
	// Mean and median differ sharply here: one huge price drags the mean up, which
	// is exactly why banding uses the median. If the summary reported only the mean
	// a consumer reproducing our bands would get different answers.
	s := pxSetup(t, pxSlots(now, 10, 10, 10, 10, 200))

	m := decode(t, doGET(t, s, "/prices/upcoming?hours=3"))
	sum, ok := m["summary"].(map[string]any)
	if !ok {
		t.Fatal("no summary")
	}
	med, present := sum["median"]
	if !present {
		t.Fatalf("summary has no median, so the bands cannot be reproduced: %v", sum)
	}
	// 10,10,10,10,200 ex-VAT -> inc-VAT median is 10*1.05.
	if got := med.(float64); got < 10.4 || got > 10.6 {
		t.Errorf("median = %v, want ~10.5 (the middle price, not the mean)", got)
	}
	if mean := sum["mean"].(float64); mean < 40 {
		t.Errorf("mean = %v; the fixture is meant to separate mean from median", mean)
	}
}

// Every other price route reports VAT-inclusive pence. /prices/stats reported
// ex-VAT under the same key names, so a dashboard plotting a daily mean against a
// live price was out by 5% with nothing on the wire to say so. The unsuffixed
// keys now mean what they mean everywhere else, and the analytical ex-VAT figures
// keep an explicit suffix.
func TestStatsUsesTheSameVATBasisAsEverySiblingRoute(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now.Truncate(24*time.Hour), 20, 20, 20, 20))

	m := decode(t, doGET(t, s, "/prices/stats?window=today"))
	if m["vat_included"] != true {
		t.Errorf("vat_included = %v, want true to match /prices and /prices/upcoming", m["vat_included"])
	}
	days := m["days"].([]any)
	if len(days) == 0 {
		t.Fatal("no days")
	}
	d := days[0].(map[string]any)

	// 20p ex-VAT is 21p inc-VAT. The unsuffixed key must be the inc-VAT one.
	if got := d["mean"].(float64); got < 20.9 || got > 21.1 {
		t.Errorf("mean = %v, want ~21 (inc VAT), not 20 (ex VAT)", got)
	}
	exc, present := d["mean_exc_vat"]
	if !present {
		t.Fatalf("the analytical ex-VAT figure must stay available under an explicit key: %v", d)
	}
	if got := exc.(float64); got < 19.9 || got > 20.1 {
		t.Errorf("mean_exc_vat = %v, want ~20", got)
	}
	// The old inc-VAT suffix would now be a second name for the unsuffixed key.
	if _, present := d["mean_inc_vat"]; present {
		t.Errorf("mean_inc_vat duplicates mean and should be gone: %v", d)
	}
}

// "Today" is period-TO-DATE everywhere else, because nobody has consumed
// tomorrow's electricity yet. Prices are the exception: today's are published in
// full before today starts, so a curve that stops at `now` hands a dashboard half
// a chart and calls it complete.
func TestPriceWindowTodayCoversTheWholeLocalDay(t *testing.T) {
	now := pxNow(t) // 12:00 UTC — midday, so a to-date window loses half the day
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	// Built on the SERVER's local day, not on UTC midnight: in BST those differ by
	// an hour, and a fixture laid out on the wrong one is short by two slots for
	// reasons that have nothing to do with what is under test.
	local := now.In(loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)

	full := make([]float64, 48)
	for i := range full {
		full[i] = 20
	}
	s := pxSetup(t, pxSlots(midnight, full...))

	w := doGET(t, s, "/prices?window=today")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)

	slots := m["slots"].([]any)
	if len(slots) != 48 {
		t.Errorf("got %d slots for window=today, want the whole day's 48", len(slots))
	}
	if m["complete"] != true {
		t.Errorf("complete = %v, want true", m["complete"])
	}
}
