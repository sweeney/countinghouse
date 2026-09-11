package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/prices"
)

// ---------------------------------------------------------------------------
// The /prices surface: the half of this service that can change behaviour.
//
// Everything else reports what happened. These endpoints say when electricity is
// cheap, which is the only thing that can move a kWh to a different hour. So the
// payload is shaped for a consumer that has to DECIDE, not merely display:
// bands and ranks are served rather than left to each dashboard to invent, and a
// gap in the prices is stated rather than left to be inferred from a short array.
// ---------------------------------------------------------------------------

const pxTariff = "E-1R-AGILE-24-10-01-A"

// fakePriceReader serves a programmable archive.
type fakePriceReader struct {
	slots map[string][]prices.Slot
	err   error
}

func (f fakePriceReader) Range(_ context.Context, code string, from, to time.Time) ([]prices.Slot, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []prices.Slot
	for _, s := range f.slots[code] {
		end := s.ValidFrom.Add(prices.SlotLength)
		if s.ValidTo != nil {
			end = *s.ValidTo
		}
		if s.ValidFrom.Before(to) && end.After(from) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f fakePriceReader) KnownTo(_ context.Context, code string) (time.Time, error) {
	if f.err != nil {
		return time.Time{}, f.err
	}
	var newest time.Time
	for _, s := range f.slots[code] {
		if s.ValidTo != nil && s.ValidTo.After(newest) {
			newest = *s.ValidTo
		}
	}
	return newest, nil
}

// pxNow is the instant every test runs at, so band and rank assertions are stable.
func pxNow(t *testing.T) time.Time {
	t.Helper()
	return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
}

// halfHourlyAgreements is a config where the tariff in force right now is
// half-hourly, so the price endpoints have a tariff code to read under.
func halfHourlyAgreements(t *testing.T) config.EnergyAgreements {
	t.Helper()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return config.EnergyAgreements{Agreements: map[string][]config.Agreement{
		"electricity": {{
			From: &from, Name: "Agile", Type: config.TariffTypeVariable,
			ID: pxTariff, Unit: "kWh", VATRate: 0.05, DailyStandingCharge: 0.59,
		}},
	}}
}

// pxSlots builds a contiguous run of half-hourly prices from start.
func pxSlots(start time.Time, exc ...float64) []prices.Slot {
	var out []prices.Slot
	for i, v := range exc {
		s := start.Add(time.Duration(i) * prices.SlotLength)
		e := s.Add(prices.SlotLength)
		out = append(out, prices.Slot{
			TariffCode: pxTariff, ValidFrom: s, ValidTo: &e,
			ExcVATPence: v, IncVATPence: v * 1.05, RetrievedAt: start,
		})
	}
	return out
}

// pxSetup wires a Server with half-hourly agreements and a given archive.
func pxSetup(t *testing.T, slots []prices.Slot) *Server {
	t.Helper()
	s, _ := dataSetup(t)
	s.Clock = fixedClock{pxNow(t)}
	s.Config = pxConfig{agreements: halfHourlyAgreements(t)}
	s.PriceReader = fakePriceReader{slots: map[string][]prices.Slot{pxTariff: slots}}
	return s
}

// pxConfig is a ConfigProvider whose tariff document is dated agreements.
type pxConfig struct{ agreements config.EnergyAgreements }

func (p pxConfig) Devices() map[string]config.DeviceConfig { return nil }
func (p pxConfig) Tariffs() config.TariffSource            { return p.agreements }
func (p pxConfig) Agreements() config.EnergyAgreements     { return p.agreements }
func (p pxConfig) TariffNamespace() string                 { return "energy_agreements" }

// ---------------------------------------------------------------------------
// /prices/upcoming — the dashboard endpoint
// ---------------------------------------------------------------------------

func TestUpcomingReturnsTheCurveWithDerivations(t *testing.T) {
	now := pxNow(t)
	// Six half hours from now: a cheap dip, a normal stretch, a peak.
	s := pxSetup(t, pxSlots(now, 10, 12, 30, 32, 60, 65))

	w := doGET(t, s, "/prices/upcoming?hours=3")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)

	if m["tariff_code"] != pxTariff {
		t.Errorf("tariff_code = %v", m["tariff_code"])
	}
	// Units and VAT are stated rather than assumed. A dashboard that guessed wrong
	// would be out by 5% on every number on screen.
	if m["unit"] != "p/kWh" {
		t.Errorf("unit = %v, want p/kWh", m["unit"])
	}
	if m["vat_included"] != true {
		t.Errorf("vat_included = %v, want true", m["vat_included"])
	}

	slots, ok := m["slots"].([]any)
	if !ok || len(slots) != 6 {
		t.Fatalf("got %v slots, want 6", len(slots))
	}
	first := slots[0].(map[string]any)
	for _, f := range []string{"valid_from", "valid_to", "price", "rank", "percentile", "band"} {
		if _, present := first[f]; !present {
			t.Errorf("slot missing %q: %v", f, first)
		}
	}
	// The cheapest slot is rank 1, so a consumer sorting by rank gets the answer
	// rather than the thing to avoid.
	if first["rank"].(float64) != 1 {
		t.Errorf("the first (cheapest) slot has rank %v, want 1", first["rank"])
	}
	if first["band"] != "cheap" {
		t.Errorf("band = %v, want cheap", first["band"])
	}

	summary, ok := m["summary"].(map[string]any)
	if !ok {
		t.Fatalf("no summary: %v", m)
	}
	for _, f := range []string{"slots", "min", "max", "mean", "current"} {
		if _, present := summary[f]; !present {
			t.Errorf("summary missing %q: %v", f, summary)
		}
	}
	// `current` is what a dashboard shows largest, so it must be the slot covering
	// now rather than the window's first slot.
	if !approx(summary["current"].(float64), 10*1.05) {
		t.Errorf("current = %v, want the price of the slot covering now", summary["current"])
	}
}

// The cheapest run per duration is the payload that actually changes behaviour:
// "run the dishwasher at 02:30" is actionable where a price curve is not.
func TestUpcomingIncludesCheapestRuns(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now, 40, 42, 8, 9, 50, 55, 60, 62))

	m := decode(t, doGET(t, s, "/prices/upcoming?hours=4"))
	cheapest, ok := m["cheapest"].(map[string]any)
	if !ok {
		t.Fatalf("no cheapest block: %v", m)
	}
	for _, d := range []string{"30m", "1h", "2h", "3h"} {
		run, present := cheapest[d].(map[string]any)
		if !present {
			t.Errorf("no cheapest run for %s", d)
			continue
		}
		for _, f := range []string{"from", "to", "mean_price"} {
			if _, ok := run[f]; !ok {
				t.Errorf("%s run missing %q: %v", d, f, run)
			}
		}
	}
	// The 30m answer must be the single cheapest slot.
	half := cheapest["30m"].(map[string]any)
	if !approx(half["mean_price"].(float64), 8*1.05) {
		t.Errorf("cheapest 30m = %v, want the 8p slot", half["mean_price"])
	}
}

// A duration longer than the window has no answer, and saying so is better than
// quoting a run that does not fit.
func TestUpcomingOmitsRunsThatDoNotFit(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now, 20, 21)) // one hour only

	m := decode(t, doGET(t, s, "/prices/upcoming?hours=1"))
	cheapest := m["cheapest"].(map[string]any)
	if _, present := cheapest["3h"]; present {
		t.Error("a 3h run cannot fit in a 1h window; it must be omitted, not invented")
	}
	if _, present := cheapest["1h"]; !present {
		t.Error("a 1h run does fit and should be present")
	}
}

// A gap must be stated. An endpoint that silently returned fewer slots than asked
// for would be the silent-gap failure in a new place.
func TestUpcomingReportsMissingSlots(t *testing.T) {
	now := pxNow(t)
	slots := pxSlots(now, 20, 21, 22, 23)
	slots = append(slots[:1], slots[2:]...) // drop the second half hour

	s := pxSetup(t, slots)
	m := decode(t, doGET(t, s, "/prices/upcoming?hours=2"))

	missing, ok := m["missing"].([]any)
	if !ok || len(missing) == 0 {
		t.Fatalf("missing should list the absent half hour: %v", m["missing"])
	}
	if m["complete"] != false {
		t.Errorf("complete = %v, want false", m["complete"])
	}
}

func TestUpcomingValidatesHours(t *testing.T) {
	s := pxSetup(t, pxSlots(pxNow(t), 20, 21))

	for _, q := range []string{"?hours=0", "?hours=-1", "?hours=abc", "?hours=999"} {
		w := doGET(t, s, "/prices/upcoming"+q)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", q, w.Code)
		}
	}
	// A sensible default when unspecified, so the common call is short.
	if w := doGET(t, s, "/prices/upcoming"); w.Code != http.StatusOK {
		t.Errorf("no hours param should default, got %d: %s", w.Code, w.Body.String())
	}
}

// A flat-rate tariff has no half-hourly curve to serve. That is not an error and
// not an empty curve — it is a different shape of answer, and conflating it with
// "we hold no prices" would send somebody hunting a collector bug.
func TestUpcomingOnAFlatTariff(t *testing.T) {
	s, _ := dataSetup(t)
	s.Clock = fixedClock{pxNow(t)}
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.Config = pxConfig{agreements: config.EnergyAgreements{
		Agreements: map[string][]config.Agreement{"electricity": {{
			From: &from, Name: "Fixed 12M", Type: config.TariffTypeFixed,
			Unit: "kWh", VATRate: 0.05, UnitRate: 0.208948, DailyStandingCharge: 0.53,
		}}},
	}}
	s.PriceReader = fakePriceReader{}

	w := doGET(t, s, "/prices/upcoming?hours=3")
	if w.Code != http.StatusOK {
		t.Fatalf("a flat tariff is a valid state, got %d: %s", w.Code, w.Body.String())
	}
	m := decode(t, w)
	if m["half_hourly"] != false {
		t.Errorf("half_hourly = %v, want false", m["half_hourly"])
	}
	// The flat rate is reported so a consumer still learns the price, in the same
	// pence-inc-VAT units as a curve would be.
	if m["flat_price"] == nil {
		t.Errorf("a flat tariff should report its rate: %v", m)
	}
	if slots, ok := m["slots"].([]any); ok && len(slots) != 0 {
		t.Errorf("a flat tariff has no curve; got %d slots", len(slots))
	}
}

// ---------------------------------------------------------------------------
// /prices/cheapest
// ---------------------------------------------------------------------------

func TestCheapestEndpoint(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now, 40, 42, 8, 9, 50, 55))

	m := decode(t, doGET(t, s, "/prices/cheapest?duration=1h"))
	if m["from"] == nil || m["to"] == nil || m["mean_price"] == nil {
		t.Fatalf("incomplete run: %v", m)
	}
	if !approx(m["mean_price"].(float64), (8+9)/2.0*1.05) {
		t.Errorf("mean_price = %v, want the 8p/9p pair", m["mean_price"])
	}
}

// The deadline is for FINISHING. A load that overruns into expensive time was not
// scheduled, it was merely started.
func TestCheapestRespectsBefore(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now, 40, 42, 8, 9, 50, 55))

	before := now.Add(2 * time.Hour).UTC().Format(time.RFC3339)
	m := decode(t, doGET(t, s, "/prices/cheapest?duration=1h&before="+before))
	to, err := time.Parse(time.RFC3339, m["to"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if to.After(now.Add(2 * time.Hour)) {
		t.Errorf("run ends %s, after the deadline", to)
	}
}

func TestCheapestWhenNothingFits(t *testing.T) {
	s := pxSetup(t, pxSlots(pxNow(t), 20, 21))
	w := doGET(t, s, "/prices/cheapest?duration=12h")
	// Not an error: the question was well formed and the answer is "no window that
	// long exists". 404 distinguishes that from a malformed request.
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404 when no run fits", w.Code)
	}
}

func TestCheapestValidatesDuration(t *testing.T) {
	s := pxSetup(t, pxSlots(pxNow(t), 20, 21))
	for _, q := range []string{"", "?duration=", "?duration=0", "?duration=banana", "?duration=-1h"} {
		if w := doGET(t, s, "/prices/cheapest"+q); w.Code != http.StatusBadRequest {
			t.Errorf("%q: got %d, want 400", q, w.Code)
		}
	}
}

// ---------------------------------------------------------------------------
// /prices — the raw curve over any window
// ---------------------------------------------------------------------------

func TestPricesOverACustomWindow(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now.Add(-2*time.Hour), 20, 21, 22, 23, 24, 25))

	from := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	to := now.Add(-time.Hour).UTC().Format(time.RFC3339)
	m := decode(t, doGET(t, s, "/prices?window=custom&from="+from+"&to="+to))

	slots := m["slots"].([]any)
	if len(slots) != 2 {
		t.Fatalf("got %d slots for a one-hour window, want 2", len(slots))
	}
	if m["from"] == nil || m["to"] == nil {
		t.Errorf("the window must be echoed: %v", m)
	}
}

func TestPricesRejectsAnInvertedWindow(t *testing.T) {
	s := pxSetup(t, pxSlots(pxNow(t), 20))
	from := pxNow(t).UTC().Format(time.RFC3339)
	to := pxNow(t).Add(-time.Hour).UTC().Format(time.RFC3339)
	if w := doGET(t, s, "/prices?window=custom&from="+from+"&to="+to); w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", w.Code)
	}
}

// ---------------------------------------------------------------------------
// /prices/stats
// ---------------------------------------------------------------------------

func TestPricesStats(t *testing.T) {
	now := pxNow(t)
	s := pxSetup(t, pxSlots(now.Add(-24*time.Hour), 20, 21, 22, -3, 60, 5))

	m := decode(t, doGET(t, s, "/prices/stats?window=7d"))
	days, ok := m["days"].([]any)
	if !ok || len(days) == 0 {
		t.Fatalf("no days: %v", m)
	}
	d := days[0].(map[string]any)
	for _, f := range []string{"day", "slots", "min", "max", "mean", "spread", "plunge_slots"} {
		if _, present := d[f]; !present {
			t.Errorf("day missing %q: %v", f, d)
		}
	}
}

// ---------------------------------------------------------------------------
// Failure modes
// ---------------------------------------------------------------------------

// An unreadable archive must not be reported as "no prices". Those lead to
// opposite actions: one is a collector problem, the other a storage problem.
func TestPriceEndpointsSurfaceArchiveErrors(t *testing.T) {
	s := pxSetup(t, nil)
	s.PriceReader = fakePriceReader{err: context.DeadlineExceeded}

	for _, path := range []string{
		"/prices/upcoming?hours=2", "/prices/cheapest?duration=1h",
		"/prices?window=today", "/prices/stats?window=7d",
	} {
		if w := doGET(t, s, path); w.Code != http.StatusInternalServerError &&
			w.Code != http.StatusBadGateway {
			t.Errorf("%s: got %d, want a 5xx when the archive cannot be read", path, w.Code)
		}
	}
}

// With no archive configured at all — a flat-rate-only deployment — the endpoints
// must say so rather than 500.
func TestPriceEndpointsWithoutAnArchive(t *testing.T) {
	s := pxSetup(t, nil)
	s.PriceReader = nil

	for _, path := range []string{"/prices/upcoming", "/prices/cheapest?duration=1h", "/prices?window=today"} {
		w := doGET(t, s, path)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: got %d, want 503 when no price archive is configured", path, w.Code)
		}
	}
}

// A dashboard polling every few seconds should not re-download a 48-slot payload
// to learn that nothing has changed.
func TestPriceEndpointsAreETagCacheable(t *testing.T) {
	s := pxSetup(t, pxSlots(pxNow(t), 10, 20, 30, 40))

	for _, path := range []string{
		"/prices/upcoming?hours=2", "/prices/cheapest?duration=1h",
		"/prices?window=today", "/prices/stats?window=7d",
	} {
		first := doGET(t, s, path)
		if first.Code != http.StatusOK {
			t.Fatalf("%s: got %d", path, first.Code)
		}
		etag := first.Header().Get("ETag")
		if etag == "" {
			t.Errorf("%s: no ETag", path)
			continue
		}
		if cc := first.Header().Get("Cache-Control"); cc == "" {
			t.Errorf("%s: no Cache-Control", path)
		}

		second := doGETWithHeader(t, s, path, "If-None-Match", etag)
		if second.Code != http.StatusNotModified {
			t.Errorf("%s: repeat with matching ETag got %d, want 304", path, second.Code)
		}
		if second.Body.Len() != 0 {
			t.Errorf("%s: a 304 must carry no body, got %d bytes", path, second.Body.Len())
		}
	}
}

// The ETag must track the CONTENT, not a timestamp — otherwise it can claim
// "unchanged" when a price has in fact moved.
func TestPriceETagChangesWithContent(t *testing.T) {
	a := pxSetup(t, pxSlots(pxNow(t), 10, 20))
	b := pxSetup(t, pxSlots(pxNow(t), 10, 99))

	ea := doGET(t, a, "/prices/upcoming?hours=1").Header().Get("ETag")
	eb := doGET(t, b, "/prices/upcoming?hours=1").Header().Get("ETag")
	if ea == "" || eb == "" {
		t.Fatal("missing ETags")
	}
	if ea == eb {
		t.Error("a changed price produced the same ETag; the tag is not tracking content")
	}
}

// doGETWithHeader is doGET with one extra request header, for conditional requests.
func doGETWithHeader(t *testing.T, s *Server, path, key, value string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set(key, value)
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, req)
	return w
}
