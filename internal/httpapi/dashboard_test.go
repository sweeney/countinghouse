package httpapi

import (
	"sort"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/prices"
)

// ---------------------------------------------------------------------------
// What a live dashboard needs from /prices.
//
// A wall-mounted screen draws one span — some hours behind for perspective, the
// forward curve, a marker on now — and has to notice when the ~16:00 publication
// extends the horizon. Both of the fields that make that a ONE-call job were
// missing from /prices and present only on /prices/upcoming, which serves a
// forward-only window and so cannot draw the retrospective half.
// ---------------------------------------------------------------------------

// known_to is the publication detector: when it moves, the horizon grew and the
// chart's x-axis has more to show. Without it a dashboard cannot tell a
// publication from any other change, and has to call a second endpoint to ask.
func TestPricesCarriesKnownTo(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now.Truncate(24*time.Hour), 20, 21, 22, 23))

	m := decode(t, doGET(t, s, "/prices?window=today"))
	kt, present := m["known_to"]
	if !present {
		t.Fatalf("no known_to on /prices; a dashboard cannot see the horizon move: %v", keysOf(m))
	}
	if kt == nil {
		t.Fatal("known_to is null despite slots being held")
	}
}

// The shared PriceCurveSummary schema documents `current`, and /prices referenced
// it while never populating it — the spec promised a field the endpoint did not
// send. It is the number a kitchen screen shows largest.
func TestPricesCarriesCurrentWhenTheWindowCoversNow(t *testing.T) {
	now := pxNow(t)
	// A whole day around now, so the window genuinely contains it.
	full := make([]float64, 48)
	for i := range full {
		full[i] = 20 + float64(i%7)
	}
	s := pxSetup(t, pxSlots(londonMidnight(t, now), full...))

	sum := decode(t, doGET(t, s, "/prices?window=today"))["summary"].(map[string]any)
	if _, present := sum["current"]; !present {
		t.Fatalf("no summary.current on a window covering now: %v", sum)
	}
}

// A historical window has no "now" in it, so the field must be ABSENT rather than
// zero — exactly as it is on /prices/upcoming when the tail gap swallows the
// current slot. Zero is a real price.
func TestPricesOmitsCurrentForAHistoricalWindow(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now.Add(-72*time.Hour), 20, 21, 22, 23))

	w := doGET(t, s, "/prices?window=custom"+
		"&from="+now.Add(-72*time.Hour).Format(time.RFC3339)+
		"&to="+now.Add(-70*time.Hour).Format(time.RFC3339))
	sum := decode(t, w)["summary"].(map[string]any)
	if v, present := sum["current"]; present {
		t.Errorf("current = %v on a window ending three days ago; it must be absent", v)
	}
}

// The trap this repeats: a publication that extends the horizon WITHOUT touching
// the requested window changes known_to and nothing else. If the ETag does not
// track it, the dashboard is told "unchanged" while the field it polls for has
// moved — the exact failure the content fingerprint was introduced to fix.
func TestETagTracksKnownToNotJustTheWindowsSlots(t *testing.T) {
	now := pxNow(t)
	past := now.Add(-48 * time.Hour)
	window := "/prices?window=custom" +
		"&from=" + past.Format(time.RFC3339) +
		"&to=" + past.Add(2*time.Hour).Format(time.RFC3339)

	// The same four slots fall INSIDE the window in both servers. The second also
	// holds slots far in the future, which is what a publication adds: the window's
	// own content is untouched, only the horizon moved.
	inWindow := pxSlots(past, 20, 21, 22, 23)
	beforePub := pxSetup(t, inWindow)
	afterPub := pxSetup(t, append(append([]prices.Slot{}, inWindow...),
		pxSlots(now.Add(24*time.Hour), 30, 31)...))

	a := doGET(t, beforePub, window).Header().Get("ETag")
	b := doGET(t, afterPub, window).Header().Get("ETag")
	if a == "" || b == "" {
		t.Fatal("missing ETag")
	}
	if a == b {
		t.Error("the horizon moved but the ETag did not; a polling dashboard would " +
			"take a 304 and never notice the publication")
	}
}

// keysOf is for error messages only.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// londonMidnight is the local midnight of t's date, as UTC — the archive's zone.
func londonMidnight(t *testing.T, at time.Time) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	l := at.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc).UTC()
}
