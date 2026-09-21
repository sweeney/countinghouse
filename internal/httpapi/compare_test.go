package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
)

// ---------------------------------------------------------------------------
// Issue #36 friction #3: no comparison primitive, so counterfactuals got
// hand-rolled — five times, with two baseline errors.
// ---------------------------------------------------------------------------

type costedS struct {
	Label          string   `json:"label"`
	TariffCodes    []string `json:"tariff_codes"`
	KWh            float64  `json:"kwh"`
	EnergyCost     float64  `json:"energy_cost"`
	StandingCharge float64  `json:"standing_charge"`
	Total          float64  `json:"total"`
}

type altS struct {
	costedS
	Kind          string `json:"kind"`
	AvailableNow  *bool  `json:"available_now"`
	AvailableNote string `json:"available_note"`
	Delta         struct {
		Energy   float64 `json:"energy"`
		Standing float64 `json:"standing"`
		Total    float64 `json:"total"`
	} `json:"delta"`
	Verdict string `json:"verdict"`
}

type compareS struct {
	Window       string  `json:"window"`
	Days         float64 `json:"days"`
	Scope        string  `json:"scope"`
	Currency     string  `json:"currency"`
	Actual       costedS `json:"actual"`
	Alternatives []altS  `json:"alternatives"`
}

func getCompare(t *testing.T, s *Server, path string) (compareS, int, string) {
	t.Helper()
	w := doGET(t, s, path)
	var c compareS
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
	}
	return c, w.Code, w.Body.String()
}

// scope is required, and the refusal explains why there is no default rather
// than just naming the valid values.
func TestCompare_ScopeIsRequiredWithNoDefault(t *testing.T) {
	s := floorSeriesSetup(t)
	_, code, body := getCompare(t, s, "/compare?window=today&alt=window_mean")
	if code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", code, body)
	}
	for _, want := range []string{"household", "monitored", "no safe default"} {
		if !contains(body, want) {
			t.Errorf("refusal should mention %q: %s", want, body)
		}
	}
}

func TestCompare_RejectsAnUnknownScope(t *testing.T) {
	s := floorSeriesSetup(t)
	if _, code, _ := getCompare(t, s, "/compare?window=today&scope=house&alt=window_mean"); code != http.StatusBadRequest {
		t.Errorf("want 400 for an unknown scope, got %d", code)
	}
}

func TestCompare_RequiresAtLeastOneAlt(t *testing.T) {
	s := floorSeriesSetup(t)
	if _, code, _ := getCompare(t, s, "/compare?window=today&scope=monitored"); code != http.StatusBadRequest {
		t.Errorf("want 400 with no alt, got %d", code)
	}
}

// The invariant, asserted on the ROUNDED wire values: a consumer checking
// energy + standing == total must not find it broken by presentation.
func TestCompare_DeltaReconcilesOnTheWire(t *testing.T) {
	s := floorSeriesSetup(t)
	c, code, body := getCompare(t, s,
		"/compare?window=today&scope=monitored"+
			"&alt=flat:unit_rate=0.30,daily_standing_charge=0.60,vat_rate=0.05")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", code, body)
	}
	if len(c.Alternatives) != 1 {
		t.Fatalf("want 1 alternative, got %d", len(c.Alternatives))
	}
	a := c.Alternatives[0]
	if sum := a.Delta.Energy + a.Delta.Standing; math.Abs(a.Delta.Total-sum) > 1e-9 {
		t.Errorf("delta.total = %v but components sum to %v", a.Delta.Total, sum)
	}
	// And the alternative is priced on the SAME energy: a counterfactual changes
	// the price, not the consumption.
	if math.Abs(a.KWh-c.Actual.KWh) > 1e-9 {
		t.Errorf("alternative kwh %v != actual kwh %v", a.KWh, c.Actual.KWh)
	}
	// This alternative is dearer than the fixture's ~21p flat tariff.
	if a.Verdict != "actual_cheaper" {
		t.Errorf("verdict = %q, want actual_cheaper at 30p vs ~21p", a.Verdict)
	}
}

// Repeatable: five counterfactuals should be one call.
func TestCompare_AcceptsSeveralAlternatives(t *testing.T) {
	s := floorSeriesSetup(t)
	c, code, body := getCompare(t, s,
		"/compare?window=today&scope=monitored"+
			"&alt=window_mean"+
			"&alt=flat:unit_rate=0.10,daily_standing_charge=0.10,vat_rate=0.05,label=Cheap+quote"+
			"&alt=flat:unit_rate=0.50,daily_standing_charge=0.90,vat_rate=0.20")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", code, body)
	}
	if len(c.Alternatives) != 3 {
		t.Fatalf("want 3 alternatives, got %d", len(c.Alternatives))
	}
	if c.Alternatives[1].Label != "Cheap quote" {
		t.Errorf("label = %q, want the caller's label", c.Alternatives[1].Label)
	}
	if c.Alternatives[1].Verdict != "alternative_cheaper" {
		t.Errorf("10p should beat the fixture's ~21p, got %q", c.Alternatives[1].Verdict)
	}
	if c.Alternatives[2].Verdict != "actual_cheaper" {
		t.Errorf("50p should lose to ~21p, got %q", c.Alternatives[2].Verdict)
	}
}

// vat_rate is not assumed. A rate guessed wrong is a comparison that is
// well-formed, internally consistent and wrong by a few percent.
func TestCompare_FlatRefusesToAssumeAVATRate(t *testing.T) {
	s := floorSeriesSetup(t)
	_, code, body := getCompare(t, s,
		"/compare?window=today&scope=monitored&alt=flat:unit_rate=0.21,daily_standing_charge=0.42")
	if code != http.StatusBadRequest {
		t.Fatalf("want 400 without vat_rate, got %d: %s", code, body)
	}
	if !contains(body, "vat_rate") {
		t.Errorf("refusal should name the missing parameter: %s", body)
	}
}

func TestCompare_FlatRefusesAMalformedRate(t *testing.T) {
	s := floorSeriesSetup(t)
	if _, code, _ := getCompare(t, s,
		"/compare?window=today&scope=monitored&alt=flat:unit_rate=cheap,daily_standing_charge=0.42,vat_rate=0.05"); code != http.StatusBadRequest {
		t.Errorf("want 400 for a non-numeric rate, got %d", code)
	}
}

// window_mean holds the standing charge constant, so the whole difference is
// load shape — the number that says whether shifting load actually paid.
func TestCompare_WindowMeanIsolatesLoadShape(t *testing.T) {
	s, _, _ := scSetup(t, len(scBuckets(t)))
	c, code, body := getCompare(t, s, "/compare?window=today&scope=monitored&alt=window_mean")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", code, body)
	}
	a := c.Alternatives[0]
	if a.Kind != "window_mean" {
		t.Errorf("kind = %q", a.Kind)
	}
	if a.Delta.Standing != 0 {
		t.Errorf("delta.standing = %v, want 0: window_mean is the same tariff", a.Delta.Standing)
	}
	if math.Abs(a.Delta.Total-a.Delta.Energy) > 1e-9 {
		t.Error("with no standing difference, the total delta is the energy delta")
	}
	// Availability does not apply to the window's own mean.
	if a.AvailableNow != nil {
		t.Errorf("available_now = %v, want omitted", *a.AvailableNow)
	}
}

// A window the archive only partly covers has no honest mean.
func TestCompare_WindowMeanRefusesAnUnpricedWindow(t *testing.T) {
	s, _, _ := scSetup(t, 4) // only the first four half hours are held
	_, code, body := getCompare(t, s, "/compare?window=today&scope=monitored&alt=window_mean")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 for a partly-priced window, got %d: %s", code, body)
	}
	if !contains(body, "no honest mean") {
		t.Errorf("refusal should say why it will not average: %s", body)
	}
}

// The expired-baseline error, made a labelled field.
func TestCompare_NamedTariffReportsAvailability(t *testing.T) {
	s, _ := dataSetup(t)
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	ended := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	ag := config.EnergyAgreements{Agreements: map[string][]config.Agreement{"electricity": {
		{From: &past, To: &ended, Name: "Expired Fix", ID: "E-1R-OLD", Type: config.TariffTypeFixed,
			VATRate: 0.05, UnitRate: 0.30, DailyStandingCharge: 0.60},
	}}}
	s.Config = fakeConfig{devices: testDevices(), tariffs: testTariffs(), agreements: &ag}

	c, code, body := getCompare(t, s, "/compare?window=today&scope=monitored&alt=tariff:id=E-1R-OLD")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", code, body)
	}
	a := c.Alternatives[0]
	if a.AvailableNow == nil || *a.AvailableNow {
		t.Fatalf("available_now = %v, want false for an agreement that ended in 2021", a.AvailableNow)
	}
	if a.AvailableNote == "" {
		t.Error("an unavailable comparator should say why")
	}
	if a.Label != "Expired Fix" {
		t.Errorf("label = %q, want the agreement's name", a.Label)
	}
}

func TestCompare_UnknownTariffIs404(t *testing.T) {
	s := floorSeriesSetup(t)
	if _, code, _ := getCompare(t, s, "/compare?window=today&scope=monitored&alt=tariff:id=nope"); code != http.StatusNotFound {
		t.Errorf("want 404 for an unknown agreement, got %d", code)
	}
}

// A half-hourly comparator needs its own archived curve, which this service does
// not hold. Refusing beats pricing it from a unit rate that is deliberately zero.
func TestCompare_HalfHourlyComparatorIsRefusedHonestly(t *testing.T) {
	s, _ := dataSetup(t)
	from := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	ag := config.EnergyAgreements{Agreements: map[string][]config.Agreement{"electricity": {
		{From: &from, Name: "Agile", ID: "E-1R-AGILE-X", Type: config.TariffTypeVariable,
			VATRate: 0.05, DailyStandingCharge: 0.60},
	}}}
	s.Config = fakeConfig{devices: testDevices(), tariffs: testTariffs(), agreements: &ag}

	_, code, body := getCompare(t, s, "/compare?window=today&scope=monitored&alt=tariff:id=E-1R-AGILE-X")
	if code != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d: %s", code, body)
	}
	if !contains(body, "alt=flat") {
		t.Errorf("the refusal should offer the way round it: %s", body)
	}
}

// household scope without a meter is a refusal naming the alternative, not a
// quiet answer from the monitored devices relabelled as the whole house.
func TestCompare_HouseholdScopeNeedsAMeter(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: noMeterDevices(), tariffs: testTariffs()}
	s.Floorplan = floorplanRecords()
	s.Influx = seriesFakeQuerier(todayHourBuckets(t),
		map[string]float64{"winefridge": 0.05}, map[string]float64{"network-ups": 100.0})

	_, code, body := getCompare(t, s, "/compare?window=today&scope=household&alt=window_mean")
	if code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", code, body)
	}
	if !contains(body, "scope=monitored") {
		t.Errorf("refusal should name the workable alternative: %s", body)
	}
}

// The two scopes are different questions over different energy, which is the
// whole reason scope is required.
func TestCompare_ScopesCoverDifferentEnergy(t *testing.T) {
	s := floorSeriesSetup(t)
	mon, code, _ := getCompare(t, s, "/compare?window=today&scope=monitored&alt=window_mean")
	if code != http.StatusOK {
		t.Fatalf("monitored: %d", code)
	}
	house, code, body := getCompare(t, s, "/compare?window=today&scope=household&alt=window_mean")
	if code != http.StatusOK {
		t.Fatalf("household: %d %s", code, body)
	}
	if house.Actual.KWh <= mon.Actual.KWh {
		t.Errorf("household kwh %v should exceed monitored %v on this fixture",
			house.Actual.KWh, mon.Actual.KWh)
	}
	// And the baseline matches what /bill says, because it comes from that path.
	b := getBill(t, s, "/bill?window=today")
	if math.Abs(mon.Actual.EnergyCost-b.EnergyCost) > 1e-9 {
		t.Errorf("/compare actual %v != /bill energy_cost %v", mon.Actual.EnergyCost, b.EnergyCost)
	}
	if b.HouseholdEnergyCost != nil && math.Abs(house.Actual.EnergyCost-*b.HouseholdEnergyCost) > 1e-9 {
		t.Errorf("/compare household %v != /bill household_energy_cost %v",
			house.Actual.EnergyCost, *b.HouseholdEnergyCost)
	}
}

func TestCompare_RejectsAMalformedAltSpec(t *testing.T) {
	s := floorSeriesSetup(t)
	for _, alt := range []string{"", "nonsense", "flat:unit_rate"} {
		if _, code, _ := getCompare(t, s, "/compare?window=today&scope=monitored&alt="+alt); code != http.StatusBadRequest {
			t.Errorf("alt=%q: want 400, got %d", alt, code)
		}
	}
}

// /compare is defined as a difference from the bill, so it must cache like the
// bill (issue #36 N7). Two routes that are defined in terms of each other but
// cache differently can drift apart in a consumer's cache while each stays
// internally consistent.
func TestCompare_CachesLikeTheBillItDiffersFrom(t *testing.T) {
	s := floorSeriesSetup(t)
	const alt = "&alt=flat:unit_rate=0.30,daily_standing_charge=0.60,vat_rate=0.05"

	cmp := doGET(t, s, "/compare?window=today&scope=monitored"+alt)
	if cmp.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", cmp.Code, cmp.Body.String())
	}
	bill := doGET(t, s, "/bill?window=today")
	if got, want := cmp.Header().Get("Cache-Control"), bill.Header().Get("Cache-Control"); got != want {
		t.Errorf("Cache-Control = %q, want the bill's %q", got, want)
	}
	if et := cmp.Header().Get("ETag"); et != "" {
		t.Errorf("unexpected ETag %q — no honest strong validator here either", et)
	}
}
