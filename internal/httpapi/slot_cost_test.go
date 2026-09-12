package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/prices"
)

// ---------------------------------------------------------------------------
// Slot-resolution costing at the HTTP boundary.
//
// The bug these tests exist for: every cost in this service was
// `kWh × tariff.UnitRate × VAT`, and a half-hourly tariff has no UnitRate — it is
// deliberately zero, because the rate lives in the price archive. So the moment the
// agreements namespace is switched on, /bill and /devices/{id}/cost would have
// answered £0.00 for real consumption. Not an error, not an empty response: a
// confident zero in the shape of a correct answer.
//
// The prices here are a REAL published day — internal/octopus/testdata's recorded
// mixed-sign day, 48 slots of which 10 are negative. Money code must be tested
// against both signs: a day of 20p slots would never catch a stray abs(), clamp or
// "costs are positive" assumption, and negative slots are not an edge case on this
// tariff. The fixture's own dates are re-stamped onto the test day because what is
// being borrowed is the SHAPE of a real price curve, not its calendar.
//
// Energy comes from seriesFakeQuerier, which reproduces both Influx row shapes the
// cost path can take — one window total, or the 30-minute bucket axis — so a test
// cannot pass by accident on the wrong one.
// ---------------------------------------------------------------------------

// scBuckets is the 30-minute axis window=today yields under the dataSetup clock
// (2026-06-11 14:00 BST): local midnight to 14:00, 28 full buckets.
func scBuckets(t *testing.T) []time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	out := make([]time.Time, 0, 28)
	for i := 0; i < 28; i++ {
		out = append(out, start.Add(time.Duration(i)*30*time.Minute))
	}
	return out
}

// scRates loads the recorded mixed-sign day's inc-VAT pence, oldest first.
func scRates(t *testing.T) []float64 {
	t.Helper()
	raw, err := os.ReadFile("../octopus/testdata/unit_rates_mixed_sign_day.json")
	if err != nil {
		t.Skipf("price fixture unavailable: %v", err)
	}
	var doc struct {
		Results []struct {
			ValueIncVAT float64 `json:"value_inc_vat"`
			ValidFrom   string  `json:"valid_from"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	sort.Slice(doc.Results, func(i, j int) bool { return doc.Results[i].ValidFrom < doc.Results[j].ValidFrom })
	out := make([]float64, 0, len(doc.Results))
	for _, r := range doc.Results {
		out = append(out, r.ValueIncVAT)
	}
	if len(out) != 48 {
		t.Fatalf("fixture has %d slots, want a full day", len(out))
	}
	return out
}

// scAgreements puts a half-hourly tariff in force across the whole test window.
func scAgreements() config.EnergyAgreements {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return config.EnergyAgreements{Agreements: map[string][]config.Agreement{
		"electricity": {{
			From: &from, Name: "Agile", Type: config.TariffTypeVariable,
			ID: pxTariff, Unit: "kWh", VATRate: 0.05, DailyStandingCharge: 0.591606,
		}},
	}}
}

// scConfig is a ConfigProvider with dated agreements AND a device inventory.
type scConfig struct {
	agreements config.EnergyAgreements
	devices    map[string]config.DeviceConfig
}

func (c scConfig) Devices() map[string]config.DeviceConfig { return c.devices }
func (c scConfig) Tariffs() config.TariffSource            { return c.agreements }
func (c scConfig) Agreements() config.EnergyAgreements     { return c.agreements }
func (c scConfig) TariffNamespace() string                 { return "energy_agreements" }

// scSetup wires a half-hourly Server: real prices in the archive for the first
// `priced` buckets of the day, and a steady per-bucket load on each device.
//
// Returns the rates actually stored (inc VAT pence, per bucket on the axis) so each
// test can compute what the bill ought to be from the fixture itself rather than
// from the code under test.
func scSetup(t *testing.T, priced int) (*Server, []time.Time, []float64) {
	t.Helper()
	buckets := scBuckets(t)
	rates := scRates(t)[:len(buckets)]

	s := scServer(t, buckets)

	slots := make([]prices.Slot, 0, priced)
	for i := 0; i < priced && i < len(buckets); i++ {
		from := buckets[i].UTC()
		to := from.Add(prices.SlotLength)
		slots = append(slots, prices.Slot{
			TariffCode: pxTariff, ValidFrom: from, ValidTo: &to,
			ExcVATPence: rates[i] / 1.05, IncVATPence: rates[i], RetrievedAt: from,
		})
	}
	s.PriceReader = fakePriceReader{slots: map[string][]prices.Slot{pxTariff: slots}}
	return s, buckets, rates
}

// scServer is dataSetup with a half-hourly tariff and a fake programmed on the
// 30-MINUTE axis — the resolution the cost path queries at. Programming it on the
// hourly axis /series defaults to would leave half the buckets empty and quietly
// halve every figure, which is the kind of agreement between fixture and code that
// has to be deliberate.
func scServer(t *testing.T, buckets []time.Time) *Server {
	t.Helper()
	s, _ := dataSetup(t)
	s.Influx = seriesFakeQuerier(buckets, scEnergyPer, scPowerPer)
	s.Config = scConfig{agreements: scAgreements(), devices: testDevices()}
	return s
}

// The per-bucket load. Counter devices report kWh per bucket; the UPS reports
// watts, which the integral query turns into kWh over the bucket's half hour.
var (
	scEnergyPer = map[string]float64{"winefridge": 0.1, "electricity_meter": 0.4}
	scPowerPer  = map[string]float64{"network-ups": 120}
)

// scUPSKWh is what the UPS's steady watts come to per 30-minute bucket.
const scUPSKWh = 120 * 0.5 / 1000

// ---------------------------------------------------------------------------

// The headline regression, and the assertion that the figure is not merely
// non-zero but RIGHT: the cost must equal the sum of each bucket's energy at that
// bucket's own published rate, computed here from the fixture.
func TestDeviceCostIsSlotPricedUnderAHalfHourlyTariff(t *testing.T) {
	s, buckets, rates := scSetup(t, 28)

	m := decode(t, mustGET(t, s, "/devices/winefridge/cost?window=today"))

	var want, wantKWh float64
	for i := range buckets {
		want += scEnergyPer["winefridge"] * rates[i] / 100
		wantKWh += scEnergyPer["winefridge"]
	}
	if want == 0 {
		t.Fatal("the fixture produced a zero expected cost; it cannot detect the bug")
	}

	if kwh, _ := m["kwh"].(float64); math.Abs(kwh-wantKWh) > 1e-6 {
		t.Errorf("kwh = %v, want %v", kwh, wantKWh)
	}
	cost, ok := m["cost"].(float64)
	if !ok {
		t.Fatalf("no cost in response: %v", m)
	}
	if cost == 0 {
		t.Fatalf("cost = 0 for %v kWh — the silent-zero bug: UnitRate is 0 on a "+
			"half-hourly tariff, so kWh x UnitRate x VAT returns a confident nothing", wantKWh)
	}
	if math.Abs(cost-want) > 5e-5 { // the response is rounded to sub-penny
		t.Errorf("cost = %v, want %v (Σ bucket kWh × that bucket's rate)", cost, want)
	}

	// The response must say HOW it was priced, and at what rate that works out to:
	// under slot pricing there is no unit_rate to report, so the effective rate is
	// the only thing that makes the figure interpretable.
	if m["attribution"] != "counter_slot" {
		t.Errorf("attribution = %v, want counter_slot", m["attribution"])
	}
	eff, ok := m["effective_rate"].(float64)
	if !ok {
		t.Fatalf("no effective_rate: %v", m)
	}
	if math.Abs(eff-want/wantKWh) > 1e-4 {
		t.Errorf("effective_rate = %v, want %v", eff, want/wantKWh)
	}
	// And must NOT report a unit rate it does not have. A zero here would be read
	// as the price, which is the same lie in a different field.
	if _, present := m["tariff"]; present {
		t.Errorf("a half-hourly tariff reported a single rate: %v", m["tariff"])
	}
	t.Logf("28 slots, %v kWh → £%.4f at %.3fp/kWh", wantKWh, cost, eff*100)
}

// /bill must likewise be slot-priced, and its per-device costs must SUM to the
// energy total. That sum is the property decision C1 rests on: the bill adds up by
// construction rather than by approximation.
func TestBillIsSlotPricedAndSums(t *testing.T) {
	s, buckets, rates := scSetup(t, 28)

	m := decode(t, mustGET(t, s, "/bill?window=today"))

	if m["attribution"] != "counter_slot" {
		t.Errorf("attribution = %v, want counter_slot", m["attribution"])
	}

	// Expected per-device, from the fixture.
	want := map[string]float64{}
	for i := range buckets {
		want["winefridge"] += scEnergyPer["winefridge"] * rates[i] / 100
		want["network-ups"] += scUPSKWh * rates[i] / 100
	}

	devices, _ := m["devices"].([]any)
	if len(devices) == 0 {
		t.Fatal("no devices on the bill")
	}
	var sum float64
	for _, d := range devices {
		dm := d.(map[string]any)
		id, _ := dm["device_id"].(string)
		cost, _ := dm["cost"].(float64)
		sum += cost
		if cost == 0 {
			t.Errorf("%s: cost = 0 — the silent-zero bug", id)
		}
		if w, known := want[id]; known && math.Abs(cost-w) > 5e-5 {
			t.Errorf("%s: cost = %v, want %v", id, cost, w)
		}
		if _, present := dm["effective_rate"]; !present {
			t.Errorf("%s: no effective_rate", id)
		}
	}
	energyCost, _ := m["energy_cost"].(float64)
	if math.Abs(sum-energyCost) > 5e-4 {
		t.Errorf("device costs sum to %v but energy_cost is %v; the bill must add up", sum, energyCost)
	}

	// The whole-house meter is reconciled, never billed as a device.
	for _, d := range devices {
		if d.(map[string]any)["device_id"] == "electricity_meter" {
			t.Error("the whole-house meter appears as a billed device")
		}
	}
}

// The standing charge is charged ONCE on the bill and never apportioned across
// devices. No device causes a standing charge, so splitting it would invent a
// number that reads like a measurement — decision D2.
func TestBillDoesNotApportionTheStandingCharge(t *testing.T) {
	s, _, _ := scSetup(t, 28)

	m := decode(t, mustGET(t, s, "/bill?window=today"))
	devices, _ := m["devices"].([]any)
	for _, d := range devices {
		dm := d.(map[string]any)
		for _, forbidden := range []string{"standing_charge", "standing", "apportioned_standing_charge"} {
			if _, present := dm[forbidden]; present {
				t.Errorf("device %v carries %q; the standing charge belongs on the total only",
					dm["device_id"], forbidden)
			}
		}
	}

	// It is still charged, and still from config: a standing charge is flat per day
	// whether or not the unit rate varies. 14 of 24 hours elapsed, grossed up 5%.
	standing, ok := m["standing_charge"].(float64)
	if !ok {
		t.Fatalf("the bill total must carry the standing charge: %v", m)
	}
	want := 14.0 / 24 * 0.591606 * 1.05
	if math.Abs(standing-want) > 1e-4 {
		t.Errorf("standing_charge = %v, want %v (partial day, apportioned)", standing, want)
	}
}

// Energy in a half hour the archive holds no rate for must be REPORTED, not charged
// at nothing. Same fail-loud rule as the collector and the validation gates: a
// visible gap beats a plausible total.
func TestBillReportsUnpricedEnergy(t *testing.T) {
	const priced = 4
	s, buckets, rates := scSetup(t, priced)

	m := decode(t, mustGET(t, s, "/bill?window=today"))

	// Every bucket past the fourth is unpriced, for both devices.
	unpricedBuckets := float64(len(buckets) - priced)
	wantUnpriced := unpricedBuckets * (scEnergyPer["winefridge"] + scUPSKWh)
	got, ok := m["unpriced_kwh"].(float64)
	if !ok {
		t.Fatalf("a window with missing prices must report unpriced_kwh: %v", m)
	}
	if math.Abs(got-wantUnpriced) > 1e-3 {
		t.Errorf("unpriced_kwh = %v, want %v", got, wantUnpriced)
	}

	// And the priced remainder must still be charged correctly — not voided, and
	// not inflated by the energy it could not price.
	var want float64
	for i := 0; i < priced; i++ {
		want += (scEnergyPer["winefridge"] + scUPSKWh) * rates[i] / 100
	}
	energyCost, _ := m["energy_cost"].(float64)
	if math.Abs(energyCost-want) > 5e-4 {
		t.Errorf("energy_cost = %v, want %v (the priced four slots only)", energyCost, want)
	}

	// The bill's effective rate must be cost ÷ PRICED energy. Dividing by all the
	// energy would quietly understate the rate actually paid.
	var pricedKWh float64
	for i := 0; i < priced; i++ {
		pricedKWh += scEnergyPer["winefridge"] + scUPSKWh
	}
	eff, _ := m["effective_rate"].(float64)
	if math.Abs(eff-want/pricedKWh) > 1e-4 {
		t.Errorf("effective_rate = %v, want %v (priced energy only)", eff, want/pricedKWh)
	}
	t.Logf("%d of %d slots priced: £%.4f charged, %.3f kWh unpriced", priced, len(buckets), energyCost, got)
}

// A negative stretch must produce a CREDIT, not a charge. This is the assertion a
// synthetic all-positive fixture can never make, and being paid to consume is the
// tariff working as designed.
func TestBillCreditsNegativeSlots(t *testing.T) {
	buckets := scBuckets(t)
	rates := scRates(t)

	// Find the longest negative run in the real day and price ONLY that.
	best, bestLen, run, runStart := -1, 0, 0, 0
	for i, r := range rates {
		if r < 0 {
			if run == 0 {
				runStart = i
			}
			run++
			if run > bestLen {
				best, bestLen = runStart, run
			}
			continue
		}
		run = 0
	}
	if best < 0 {
		t.Fatal("the fixture has no negative slots; it cannot test a credit")
	}

	s := scServer(t, buckets)
	var slots []prices.Slot
	var want float64
	for i := best; i < best+bestLen && i < len(buckets); i++ {
		from := buckets[i].UTC()
		to := from.Add(prices.SlotLength)
		slots = append(slots, prices.Slot{
			TariffCode: pxTariff, ValidFrom: from, ValidTo: &to,
			ExcVATPence: rates[i] / 1.05, IncVATPence: rates[i], RetrievedAt: from,
		})
		want += (scEnergyPer["winefridge"] + scUPSKWh) * rates[i] / 100
	}
	s.PriceReader = fakePriceReader{slots: map[string][]prices.Slot{pxTariff: slots}}

	m := decode(t, mustGET(t, s, "/bill?window=today"))
	energyCost, _ := m["energy_cost"].(float64)
	if energyCost >= 0 {
		t.Errorf("energy_cost = %v; consuming only in negative slots must be a CREDIT", energyCost)
	}
	if math.Abs(energyCost-want) > 5e-4 {
		t.Errorf("energy_cost = %v, want %v", energyCost, want)
	}
	// A magnitude-summing bug would give this instead.
	var magnitudes float64
	for i := best; i < best+bestLen && i < len(buckets); i++ {
		magnitudes += (scEnergyPer["winefridge"] + scUPSKWh) * math.Abs(rates[i]) / 100
	}
	if math.Abs(energyCost-magnitudes) < 1e-9 {
		t.Error("energy_cost equals the sum of MAGNITUDES; signs are being discarded")
	}

	// The total still nets the standing charge against the credit, rather than
	// either hiding the credit or dropping the charge.
	standing, _ := m["standing_charge"].(float64)
	total, _ := m["total"].(float64)
	if math.Abs(total-(energyCost+standing)) > 1e-4 {
		t.Errorf("total = %v, want energy %v + standing %v", total, energyCost, standing)
	}
	t.Logf("%d consecutive negative slots: energy £%.4f, standing £%.4f, total £%.4f",
		bestLen, energyCost, standing, total)
}

// A flat tariff must behave exactly as before: one whole-window query times one
// rate. This change is about making half-hourly work, not about altering the
// numbers a flat deployment already reports.
func TestFlatTariffCostTakesTheScalarPath(t *testing.T) {
	s, fake := dataSetup(t)
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.Config = scConfig{
		devices: testDevices(),
		agreements: config.EnergyAgreements{Agreements: map[string][]config.Agreement{
			"electricity": {{
				From: &from, Name: "Fixed", Type: config.TariffTypeFixed,
				Unit: "kWh", VATRate: 0.05, UnitRate: 0.2089, DailyStandingCharge: 0.5294,
			}},
		}},
	}
	s.PriceReader = nil // a flat deployment needs no archive

	m := decode(t, mustGET(t, s, "/devices/winefridge/cost?window=today"))
	// dataSetup's fake answers the window-total query with 3 kWh flat.
	want := 3.0 * 0.2089 * 1.05
	if cost, _ := m["cost"].(float64); math.Abs(cost-want) > 1e-4 {
		t.Errorf("cost = %v, want %v (3 kWh × 20.89p × 1.05)", m["cost"], want)
	}
	if m["attribution"] != "flat_rate" {
		t.Errorf("attribution = %v, want flat_rate", m["attribution"])
	}
	// A flat tariff DOES have a single rate, and must still report it.
	if _, present := m["tariff"]; !present {
		t.Errorf("a flat tariff must still report its unit rate: %v", m)
	}

	// And it must not have gone bucketing: the scalar path is one query per device
	// with no aggregateWindow in it.
	for _, q := range fake.Queries {
		if strings.Contains(q, "aggregateWindow") {
			t.Error("a flat tariff issued a bucketed query; the scalar path is exact and cheaper")
		}
	}

	if w := doGET(t, s, "/bill?window=today"); w.Code != http.StatusOK {
		t.Errorf("/bill on a flat tariff got %d: %s", w.Code, w.Body.String())
	}
}

// A month at half-hourly resolution is 1488 buckets, over MaxBuckets (1000). That
// cap is a RESPONSE-SIZE guard for /series; the cost path sums those buckets to a
// few scalars that never reach the wire, so it must not apply here — otherwise a
// month's bill becomes unanswerable the moment the tariff goes half-hourly.
func TestMonthlyBillIsNotBlockedByTheBucketCap(t *testing.T) {
	s, _, _ := scSetup(t, 28)

	w := doGET(t, s, "/bill?window=month")
	if w.Code == http.StatusBadRequest {
		t.Fatalf("a monthly bill was refused, probably by MaxBuckets: %s", w.Body.String())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	// And it must have actually costed the month's energy rather than answering an
	// empty bill, which would pass a status check while proving nothing.
	if cost, _ := decode(t, w)["energy_cost"].(float64); cost == 0 {
		t.Error("the monthly bill came back with no energy cost")
	}
}

// mustGET is doGET with a 200 assertion, since most tests here read the body.
func mustGET(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := doGET(t, s, path)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: want 200, got %d: %s", path, w.Code, w.Body.String())
	}
	return w
}

// ---------------------------------------------------------------------------

// The switchover bill: a window that starts on the old fixed tariff and ends on
// Agile. This is not a corner case — it is the shape of the very next bill after
// the move — and three things have to be right at once. Each side prices on its own
// tariff, each side's standing charge applies for its own days, and the boundary
// instant belongs to the LATER tariff (half-open), because getting that backwards
// bills one half hour on the wrong tariff at every switchover: small, and
// permanently irreproducible.
func TestBillSpanningASwitchover(t *testing.T) {
	buckets := scBuckets(t)
	rates := scRates(t)[:len(buckets)]

	// Fixed until 07:00 local, Agile from then on — mid-window, mid-day.
	const switchAt = 14 // bucket index: 07:00 BST
	fixedFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	boundary := buckets[switchAt].UTC()

	s := scServer(t, buckets)
	s.Config = scConfig{
		devices: testDevices(),
		agreements: config.EnergyAgreements{Agreements: map[string][]config.Agreement{
			"electricity": {
				{
					From: &fixedFrom, To: &boundary, Name: "Fixed", Type: config.TariffTypeFixed,
					Unit: "kWh", VATRate: 0.05, UnitRate: 0.2089, DailyStandingCharge: 0.5294,
				},
				{
					From: &boundary, Name: "Agile", Type: config.TariffTypeVariable,
					ID: pxTariff, Unit: "kWh", VATRate: 0.05, DailyStandingCharge: 0.591606,
				},
			},
		}},
	}

	// The archive holds prices for the Agile side ONLY. A rate for a half hour the
	// fixed tariff governed must not be reached for.
	var slots []prices.Slot
	for i := switchAt; i < len(buckets); i++ {
		from := buckets[i].UTC()
		to := from.Add(prices.SlotLength)
		slots = append(slots, prices.Slot{
			TariffCode: pxTariff, ValidFrom: from, ValidTo: &to,
			ExcVATPence: rates[i] / 1.05, IncVATPence: rates[i], RetrievedAt: from,
		})
	}
	s.PriceReader = fakePriceReader{slots: map[string][]prices.Slot{pxTariff: slots}}

	m := decode(t, mustGET(t, s, "/bill?window=today"))

	// Expected energy cost: the fixed rate before the boundary, the published slot
	// rate after it. The per-bucket load is the same throughout, so any boundary
	// error shows up as exactly one bucket's worth of difference.
	perBucket := scEnergyPer["winefridge"] + scUPSKWh
	var want float64
	for i := range buckets {
		if i < switchAt {
			want += perBucket * 0.2089 * 1.05
			continue
		}
		want += perBucket * rates[i] / 100
	}
	energyCost, _ := m["energy_cost"].(float64)
	if math.Abs(energyCost-want) > 5e-4 {
		t.Errorf("energy_cost = %v, want %v", energyCost, want)
	}
	// Nothing may be unpriced: both sides of the boundary had a rate.
	if u, present := m["unpriced_kwh"]; present {
		t.Errorf("unpriced_kwh = %v; every half hour in this window had a rate", u)
	}

	// The boundary must land on the LATER tariff. Pricing it at the fixed rate
	// instead shifts the total by one bucket at the difference between the two
	// rates, so assert against that specific wrong answer rather than a tolerance.
	wrong := want - perBucket*rates[switchAt]/100 + perBucket*0.2089*1.05
	if math.Abs(energyCost-wrong) < 1e-6 && math.Abs(want-wrong) > 1e-6 {
		t.Errorf("the boundary half hour was billed on the OLD tariff; [from, to) is half-open")
	}

	// Two standing charges, each for its own stretch of the window: 7 hours fixed,
	// 7 hours Agile. One charge for the whole window would be wrong by their
	// difference, and charging both in full would double the day.
	wantStanding := 7.0/24*0.5294*1.05 + 7.0/24*0.591606*1.05
	standing, _ := m["standing_charge"].(float64)
	if math.Abs(standing-wantStanding) > 1e-4 {
		t.Errorf("standing_charge = %v, want %v (7h fixed + 7h Agile)", standing, wantStanding)
	}

	// A mixed window is slot-priced, because part of it has to be.
	if m["attribution"] != "counter_slot" {
		t.Errorf("attribution = %v, want counter_slot for a window spanning a switchover", m["attribution"])
	}
	t.Logf("switchover bill: energy £%.4f (£%.4f fixed side), standing £%.4f",
		energyCost, float64(switchAt)*perBucket*0.2089*1.05, standing)
}

// Two FLAT tariffs either side of a boundary must also split at it, rather than
// billing the whole window at whichever rate happened to be in force at the start.
// The old code asked TariffFor(win.Start) and got exactly that wrong.
func TestBillSpanningTwoFlatTariffs(t *testing.T) {
	buckets := scBuckets(t)
	const switchAt = 14
	fixedFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	boundary := buckets[switchAt].UTC()

	s := scServer(t, buckets)
	s.Config = scConfig{
		devices: testDevices(),
		agreements: config.EnergyAgreements{Agreements: map[string][]config.Agreement{
			"electricity": {
				{
					From: &fixedFrom, To: &boundary, Name: "Old", Type: config.TariffTypeFixed,
					Unit: "kWh", VATRate: 0.05, UnitRate: 0.2089, DailyStandingCharge: 0.5294,
				},
				{
					From: &boundary, Name: "New", Type: config.TariffTypeFixed,
					Unit: "kWh", VATRate: 0.05, UnitRate: 0.3000, DailyStandingCharge: 0.5294,
				},
			},
		}},
	}
	s.PriceReader = nil // neither side is half-hourly; no archive needed

	m := decode(t, mustGET(t, s, "/bill?window=today"))

	perBucket := scEnergyPer["winefridge"] + scUPSKWh
	want := float64(switchAt)*perBucket*0.2089*1.05 +
		float64(len(buckets)-switchAt)*perBucket*0.3000*1.05
	energyCost, _ := m["energy_cost"].(float64)
	if math.Abs(energyCost-want) > 5e-4 {
		t.Errorf("energy_cost = %v, want %v (split at the boundary)", energyCost, want)
	}
	// The whole window at the opening rate is the specific wrong answer.
	allOld := float64(len(buckets)) * perBucket * 0.2089 * 1.05
	if math.Abs(energyCost-allOld) < 1e-6 {
		t.Error("the whole window was billed at the tariff in force at its START")
	}
}
