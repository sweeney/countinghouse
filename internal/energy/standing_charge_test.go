package energy

import (
	"math"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
)

// ---------------------------------------------------------------------------
// The standing charge across tariff segments.
//
// A standing charge is flat per day whether or not the unit rate varies, so it
// keeps coming from config even on a half-hourly tariff. What the switchover adds
// is that a single window can straddle TWO daily charges and two VAT rates, and the
// first Agile bill necessarily does. Getting it wrong either loses a day's charge
// or charges it twice — small money, and permanently irreproducible once the bill
// has been read.
// ---------------------------------------------------------------------------

// seg builds one segment of a given length in days at the given daily charge.
func seg(days, daily, vat float64) config.Segment {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return config.Segment{
		Start:  start,
		Stop:   start.Add(time.Duration(days * 24 * float64(time.Hour))),
		Tariff: config.Tariff{DailyStandingCharge: daily, VATRate: vat},
	}
}

func TestStandingChargeAcross(t *testing.T) {
	tests := []struct {
		name string
		segs []config.Segment
		want float64
	}{
		{
			// No tariff in force means nothing to charge — not a default, not a panic.
			name: "no segments charge nothing",
			segs: nil,
			want: 0,
		},
		{
			name: "one whole day",
			segs: []config.Segment{seg(1, 0.5294, 0.05)},
			want: 1 * 0.5294 * 1.05,
		},
		{
			// A period-to-date window ends at "now", so its last day is partial and is
			// billed proportionally rather than rounded up. That keeps a running total
			// monotonic, which a ceiling would not.
			name: "a partial day is apportioned, not rounded up",
			segs: []config.Segment{seg(0.25, 0.5294, 0.05)},
			want: 0.25 * 0.5294 * 1.05,
		},
		{
			// The switchover case. Each side pays its own daily charge for its own
			// days, and the parts add up to the window because PeriodsBetween tiles it.
			name: "two tariffs, each charged for its own days",
			segs: []config.Segment{seg(10, 0.5294, 0.05), seg(20, 0.591606, 0.05)},
			want: 10*0.5294*1.05 + 20*0.591606*1.05,
		},
		{
			// VAT is a property of the tariff in force, so a VAT change mid-window has
			// to be honoured per segment. Applying one rate to the whole window is the
			// bug this case exists to catch.
			name: "a VAT change is honoured per segment",
			segs: []config.Segment{seg(5, 0.5, 0.05), seg(5, 0.5, 0.20)},
			want: 5*0.5*1.05 + 5*0.5*1.20,
		},
		{
			// A zero standing charge is a real tariff shape, not a missing value.
			name: "a zero daily charge costs nothing",
			segs: []config.Segment{seg(30, 0, 0.05)},
			want: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := StandingChargeAcross(tc.segs)
			if math.Abs(got-tc.want) > 1e-12 {
				t.Errorf("StandingChargeAcross = %v, want %v", got, tc.want)
			}
		})
	}
}

// Splitting a window at a boundary must not change what it costs when both sides
// carry the same tariff. If it does, the apportionment is lossy and the first bill
// after any switchover is wrong by the rounding.
func TestStandingChargeAcrossIsAdditive(t *testing.T) {
	whole := StandingChargeAcross([]config.Segment{seg(30, 0.591606, 0.05)})
	split := StandingChargeAcross([]config.Segment{
		seg(11.5, 0.591606, 0.05),
		seg(18.5, 0.591606, 0.05),
	})
	if math.Abs(whole-split) > 1e-12 {
		t.Errorf("30 days costs %v whole but %v split in two; apportionment must be additive", whole, split)
	}
}

// ---------------------------------------------------------------------------

// PriceFlat must refuse to invent money for a half-hourly tariff. Its UnitRate is
// zero by design, so multiplying by it yields the confident £0.00 that the whole
// slot-costing path exists to prevent — and a test that only ever passes a flat
// tariff would never notice.
func TestPriceFlatOnAHalfHourlyTariffChargesNothing(t *testing.T) {
	// A tariff is half-hourly precisely when it names an archive tariff code.
	half := config.Tariff{TariffCode: "E-1R-AGILE-24-10-01-A", Unit: "kWh", VATRate: 0.05}
	if !half.IsHalfHourly() {
		t.Fatalf("fixture is not half-hourly; the case under test is unreachable")
	}
	devices := []DeviceCost{{DeviceID: "a", KWh: 12}}
	PriceFlat(devices, half)
	if devices[0].Cost != 0 {
		t.Errorf("cost = %v; a half-hourly tariff has no unit rate to multiply by", devices[0].Cost)
	}
	// This is exactly why the cost path must not route a half-hourly tariff here —
	// the number above is not a price, it is the absence of one. tariffPlan.scalar
	// is the guard, and internal/httpapi asserts it.
}

// The effective rate and unpriced totals AssembleBill derives, which are the two
// fields that make a slot-priced bill readable.
func TestAssembleBillDerivesEffectiveRateAndUnpriced(t *testing.T) {
	win := dayWindow("today", 1)
	devices := []DeviceCost{
		// 10 kWh priced at £2 plus 4 kWh nobody held a rate for.
		{DeviceID: "a", KWh: 14, Cost: 2.0, UnpricedKWh: 4},
		// 5 kWh priced at £1, fully priced.
		{DeviceID: "b", KWh: 5, Cost: 1.0},
	}
	bill := AssembleBill(win, devices, 20, true, BillPricing{
		StandingCharge: 0.5,
		Attribution:    AttributionCounterSlot,
	})

	if bill.Attribution != AttributionCounterSlot {
		t.Errorf("Attribution = %q, want %q", bill.Attribution, AttributionCounterSlot)
	}
	if bill.UnpricedKWh != 4 {
		t.Errorf("UnpricedKWh = %v, want 4", bill.UnpricedKWh)
	}
	// Per device: cost ÷ that device's energy.
	if got, want := bill.Devices[0].EffectiveRate, 2.0/14; math.Abs(got-want) > 1e-12 {
		t.Errorf("device a EffectiveRate = %v, want %v", got, want)
	}
	// For the bill: cost ÷ PRICED energy (19 − 4 = 15). Dividing by all 19 would
	// quietly understate the rate actually paid, which is the number a consumer
	// would read as "how well did we play the curve".
	if got, want := bill.EffectiveRate, 3.0/15; math.Abs(got-want) > 1e-12 {
		t.Errorf("bill EffectiveRate = %v, want %v (priced energy only)", got, want)
	}
	// Monitored kWh still counts ALL the energy: the meter saw it, priced or not.
	if bill.Reconciliation.MonitoredKWh != 19 {
		t.Errorf("MonitoredKWh = %v, want 19", bill.Reconciliation.MonitoredKWh)
	}
	if bill.Total != 3.0+0.5 {
		t.Errorf("Total = %v, want 3.5", bill.Total)
	}
}

// Zero energy has no effective rate. It must report 0, because neither NaN nor ±Inf
// survives JSON — a single one truncates the whole response mid-encode.
func TestAssembleBillZeroEnergyHasNoEffectiveRate(t *testing.T) {
	bill := AssembleBill(dayWindow("today", 1), []DeviceCost{{DeviceID: "a"}}, 0, false,
		BillPricing{Attribution: AttributionCounterSlot})
	if r := bill.Devices[0].EffectiveRate; r != 0 || math.IsNaN(r) {
		t.Errorf("EffectiveRate = %v, want a finite 0", r)
	}
	if r := bill.EffectiveRate; r != 0 || math.IsNaN(r) {
		t.Errorf("bill EffectiveRate = %v, want a finite 0", r)
	}
}

// A negative cost must pass through as a credit, with a negative effective rate to
// match. A bill that clamped either would hide the tariff working as intended.
func TestAssembleBillCarriesACredit(t *testing.T) {
	bill := AssembleBill(dayWindow("today", 1), []DeviceCost{
		{DeviceID: "a", KWh: 10, Cost: -0.25},
	}, 10, true, BillPricing{StandingCharge: 0.6, Attribution: AttributionCounterSlot})

	if bill.EnergyCost >= 0 {
		t.Errorf("EnergyCost = %v, want a credit", bill.EnergyCost)
	}
	if bill.Devices[0].EffectiveRate >= 0 {
		t.Errorf("EffectiveRate = %v, want negative", bill.Devices[0].EffectiveRate)
	}
	// The standing charge is still due; the total nets the two.
	if want := -0.25 + 0.6; math.Abs(bill.Total-want) > 1e-12 {
		t.Errorf("Total = %v, want %v", bill.Total, want)
	}
}
