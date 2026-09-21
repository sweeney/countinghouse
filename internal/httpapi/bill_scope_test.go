package httpapi

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/sweeney/countinghouse/internal/config"
)

// ---------------------------------------------------------------------------
// Issue #36 finding #5 / N3: /bill returns a `total` that is not the household's.
//
// energy_cost is the sum of the device rows, and total is that plus the standing
// charge. Both exclude the unmonitored remainder. On a home where the meter sees
// twice what the plugs do, quoting /bill.total understates the bill by half — and
// the endpoint is called /bill, so it gets quoted.
//
// Nothing existing changes. total means exactly what it always meant; `scope`
// now says what that is, and the household figures sit beside it.
// ---------------------------------------------------------------------------

// billS is the subset of the bill these tests assert on.
type billS struct {
	EnergyCost          float64  `json:"energy_cost"`
	StandingCharge      float64  `json:"standing_charge"`
	Total               float64  `json:"total"`
	Scope               string   `json:"scope"`
	HouseholdEnergyCost *float64 `json:"household_energy_cost"`
	HouseholdTotal      *float64 `json:"household_total"`
	Reconciliation      struct {
		MeterPresent         bool     `json:"meter_present"`
		MonitoredKWh         float64  `json:"monitored_kwh"`
		MeterKWh             *float64 `json:"meter_kwh"`
		UnmonitoredKWh       *float64 `json:"unmonitored_kwh"`
		UnmonitoredCost      *float64 `json:"unmonitored_cost"`
		UnmonitoredPricedKWh *float64 `json:"unmonitored_priced_kwh"`
		Coverage             *float64 `json:"coverage"`
	} `json:"reconciliation"`
}

func getBill(t *testing.T, s *Server, path string) billS {
	t.Helper()
	w := doGET(t, s, path)
	var b billS
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return b
}

// noMeterDevices is the floorplan inventory with the whole-house meter removed.
func noMeterDevices() map[string]config.DeviceConfig {
	out := map[string]config.DeviceConfig{}
	for id, d := range floorplanDevices() {
		if d.Class == "energy_meter" {
			continue
		}
		out[id] = d
	}
	return out
}

// total keeps its meaning; scope states it.
func TestBill_ScopeNamesWhatTotalCovers(t *testing.T) {
	s := floorSeriesSetup(t)
	b := getBill(t, s, "/bill?window=today")
	if b.Scope != "monitored_devices" {
		t.Errorf("scope = %q, want monitored_devices", b.Scope)
	}
	// Tolerance, not equality: totals are rounded from their full-precision values
	// rather than by summing the already-rounded parts, so the wire's 4dp figures
	// can differ from each other in the last place. That is deliberate.
	if math.Abs(b.Total-(b.EnergyCost+b.StandingCharge)) > 2e-4 {
		t.Errorf("total = %v, want energy_cost + standing_charge = %v",
			b.Total, b.EnergyCost+b.StandingCharge)
	}
}

// The number a person would call "the bill", reachable in one call.
func TestBill_HouseholdTotalIncludesTheUnmonitoredRemainder(t *testing.T) {
	s := floorSeriesSetup(t) // meter 1.0/bucket against ~0.45 monitored
	b := getBill(t, s, "/bill?window=today")

	if b.HouseholdEnergyCost == nil || b.HouseholdTotal == nil {
		t.Fatal("a metered home must report the household figures")
	}
	if b.Reconciliation.UnmonitoredCost == nil {
		t.Fatal("the remainder must be priced, not merely counted in kWh")
	}
	// household energy == monitored + the remainder, exactly.
	want := b.EnergyCost + *b.Reconciliation.UnmonitoredCost
	if math.Abs(*b.HouseholdEnergyCost-want) > 5e-4 {
		t.Errorf("household_energy_cost = %v, want energy_cost + unmonitored_cost = %v",
			*b.HouseholdEnergyCost, want)
	}
	if math.Abs(*b.HouseholdTotal-(*b.HouseholdEnergyCost+b.StandingCharge)) > 5e-4 {
		t.Errorf("household_total = %v, want household_energy_cost + standing_charge",
			*b.HouseholdTotal)
	}
	// The finding itself: on a partly-covered home these differ, and materially.
	if *b.HouseholdTotal <= b.Total {
		t.Errorf("household_total %v should exceed total %v when the meter sees more "+
			"than the plugs", *b.HouseholdTotal, b.Total)
	}
}

// On a flat tariff the whole thing must reconcile to meter × rate: any other
// answer means the remainder was priced differently from the devices.
func TestBill_HouseholdEnergyReconcilesToTheMeter(t *testing.T) {
	s := floorSeriesSetup(t)
	b := getBill(t, s, "/bill?window=today")

	if b.Reconciliation.MeterKWh == nil || b.HouseholdEnergyCost == nil {
		t.Fatal("fixture should have a meter")
	}
	// testTariffs() is flat, so the household energy cost is meter kWh at one rate.
	rate := *b.HouseholdEnergyCost / *b.Reconciliation.MeterKWh
	deviceRate := b.EnergyCost / b.Reconciliation.MonitoredKWh
	// Both sides divide 4dp-rounded money, so agreement is to within the rounding
	// carried into the quotient, not to the bit.
	if math.Abs(rate-deviceRate) > 5e-5 {
		t.Errorf("household energy implies %v £/kWh but the devices were billed at %v: "+
			"the remainder was priced differently", rate, deviceRate)
	}
}

// The claim that makes this safe: /bill's remainder and /series?group_by=house's
// unmonitored series are the same quantity through the same pricer. If they can
// drift, the household figure is a second opinion rather than the answer.
func TestBill_UnmonitoredCostAgreesWithSeries(t *testing.T) {
	s := floorSeriesSetup(t)
	b := getBill(t, s, "/bill?window=today")

	var seriesCost float64
	for _, ser := range decodeSeries(t, doGET(t, s, "/series?window=today&group_by=house")).Series {
		if ser.Key == "unmonitored" {
			for _, c := range ser.Cost {
				seriesCost += c
			}
		}
	}
	if b.Reconciliation.UnmonitoredCost == nil {
		t.Fatal("no unmonitored cost on the bill")
	}
	if math.Abs(*b.Reconciliation.UnmonitoredCost-seriesCost) > 5e-3 {
		t.Errorf("/bill says the remainder cost %v, /series says %v — these must not "+
			"be able to disagree", *b.Reconciliation.UnmonitoredCost, seriesCost)
	}
}

// With no meter there is no remainder to price, so the household figures are
// OMITTED rather than sent as a confident copy of the monitored total.
func TestBill_NoMeterOmitsTheHouseholdFigures(t *testing.T) {
	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: noMeterDevices(), tariffs: testTariffs()}
	s.Floorplan = floorplanRecords()
	s.Influx = seriesFakeQuerier(todayHourBuckets(t),
		map[string]float64{"winefridge": 0.05}, map[string]float64{"network-ups": 100.0})

	b := getBill(t, s, "/bill?window=today")
	if b.Reconciliation.MeterPresent {
		t.Fatal("fixture should have no meter")
	}
	if b.HouseholdEnergyCost != nil || b.HouseholdTotal != nil {
		t.Errorf("no meter means no household figure, got %v/%v",
			b.HouseholdEnergyCost, b.HouseholdTotal)
	}
	// scope still says what total covers.
	if b.Scope != "monitored_devices" {
		t.Errorf("scope = %q, want monitored_devices even with no meter", b.Scope)
	}
}

// Under a half-hourly tariff the remainder is priced per half hour at each half
// hour's own rate, through the same build the device rows came from. That is the
// case where "price it at one average rate" would be wrong, and it is the case
// the tariff exists for.
func TestBill_HouseholdFiguresUnderAHalfHourlyTariff(t *testing.T) {
	s, _, _ := scSetup(t, len(scBuckets(t)))
	b := getBill(t, s, "/bill?window=today")

	if b.HouseholdTotal == nil || b.Reconciliation.UnmonitoredCost == nil {
		t.Fatal("a metered home on a half-hourly tariff must report the household figures")
	}
	if *b.Reconciliation.UnmonitoredCost <= 0 {
		t.Fatalf("unmonitored_cost = %v, want positive: the meter sees more than the plugs",
			*b.Reconciliation.UnmonitoredCost)
	}
	// The same quantity /series reports, from the same pricer.
	var seriesCost float64
	for _, ser := range decodeSeries(t, doGET(t, s,
		"/series?window=today&interval=30m&group_by=house")).Series {
		if ser.Key == "unmonitored" {
			for _, c := range ser.Cost {
				seriesCost += c
			}
		}
	}
	if math.Abs(*b.Reconciliation.UnmonitoredCost-seriesCost) > 5e-3 {
		t.Errorf("/bill remainder %v vs /series %v", *b.Reconciliation.UnmonitoredCost, seriesCost)
	}

	// And the priced kWh is reported, so the pair is self-consistent rather than
	// implying an effective rate nobody charged.
	if b.Reconciliation.UnmonitoredPricedKWh == nil {
		t.Error("unmonitored_cost without the kWh it was computed from")
	}
}
