package energy

import "github.com/sweeney/countinghouse/internal/config"

// Attribution names how the energy cost in a response was derived.
//
// It is on the wire because under a half-hourly tariff the METHOD is load-bearing
// for how closely a per-device figure should be read. A consumer that cannot tell
// slot-priced counter deltas from a flat multiplication has no way to know that a
// single day's fridge cost carries a few percent of quantisation noise.
const (
	// AttributionFlatRate: kWh x one unit rate. Exact over any window.
	AttributionFlatRate = "flat_rate"

	// AttributionCounterSlot: each half hour's counter delta priced at that half
	// hour's own rate — decision C1, docs/per-device-attribution.md. Sums exactly
	// to the monitored house cost, preserves WHEN a device ran (the entire point of
	// a half-hourly tariff), and is noisy for a low-draw device over a single day
	// because a 0.1 kWh counter tick lands in whichever slot reported it.
	AttributionCounterSlot = "counter_slot"
)

// DeviceCost is one billable device's energy and money for a window. KWh is the
// metered energy; Cost is the VAT-inclusive £ cost for that energy at the
// window's tariff. DisplayName/Room/Class are descriptive passthrough from
// device config for the /bill breakdown.
type DeviceCost struct {
	DeviceID    string `json:"device_id"`
	DisplayName string `json:"display_name"`
	Room        string `json:"room"`
	// Covers is set when the device's readings describe the whole property rather
	// than the room it sits in, which is why its room may legitimately be empty.
	// Without it the bill cannot tell "no room configured" from "meters the house".
	Covers string  `json:"covers,omitempty"`
	Class  string  `json:"class"`
	KWh    float64 `json:"kwh"`
	Cost   float64 `json:"cost"`

	// EffectiveRate is the VAT-inclusive £/kWh actually paid: Cost / KWh. Under
	// counter_slot attribution this is what makes a per-device figure readable —
	// it says whether this device ran cheap or dear against the day — and so it is
	// what turns the quantisation noise from puzzling into interpretable. Zero
	// energy has no effective rate and reports 0 rather than NaN.
	EffectiveRate float64 `json:"effective_rate"`

	// UnpricedKWh is this device's energy in half hours the price archive holds no
	// rate for. It is deliberately NOT folded into Cost: charging nothing for real
	// energy is the silent failure this whole path exists to prevent, so the gap is
	// reported instead. Omitted when zero, which is the normal case.
	UnpricedKWh float64 `json:"unpriced_kwh,omitempty"`
}

// Reconciliation compares the sum of monitored devices against the whole-house
// meter for the same window. UnmonitoredKWh is the remainder the meter saw that
// no monitored device accounts for; Coverage is the monitored fraction.
//
// MeterPresent records whether an energy_meter is configured at all. When it is
// false the meter-derived fields (MeterKWh, UnmonitoredKWh, Coverage) are nil
// and omitted from the wire: with no meter there is nothing to reconcile
// against, so inventing an "unmonitored" remainder (which would be a misleading
// NEGATIVE 0-monitored) or a coverage of 0 (despite full monitoring) would
// actively mislead consumers. The "no meter configured" case and the genuine
// "meter present but read 0" case (a degenerate window with no data, surfaced
// as-is) are thus distinguishable.
//
// Coverage, when present, is intentionally NOT clamped to [0,1]: with
// solar/battery the meter can read less than monitored consumption (export) or
// even net-negative, which legitimately pushes coverage above 1 or below 0
// (PLAN §5 watch-outs). The number is surfaced as-is rather than hidden.
type Reconciliation struct {
	MeterPresent   bool     `json:"meter_present"`
	MonitoredKWh   float64  `json:"monitored_kwh"`
	MeterKWh       *float64 `json:"meter_kwh,omitempty"`
	UnmonitoredKWh *float64 `json:"unmonitored_kwh,omitempty"`
	Coverage       *float64 `json:"coverage,omitempty"`
}

// Bill is the assembled /bill response for one window: per-device breakdown,
// money totals (VAT-inclusive £), and meter reconciliation.
type Bill struct {
	Window         string         `json:"window"`
	Currency       string         `json:"currency"`
	Devices        []DeviceCost   `json:"devices"`
	EnergyCost     float64        `json:"energy_cost"`
	StandingCharge float64        `json:"standing_charge"`
	Total          float64        `json:"total"`
	Reconciliation Reconciliation `json:"reconciliation"`

	// Attribution is how the energy costs above were derived — see the constants.
	Attribution string `json:"attribution"`

	// EffectiveRate is the VAT-inclusive £/kWh across all monitored energy. On a
	// half-hourly tariff it is the single number that says how well the house
	// played the curve, which is the question the tariff exists to ask.
	EffectiveRate float64 `json:"effective_rate"`

	// UnpricedKWh is monitored energy no rate was held for, summed across devices.
	// Non-zero means this bill is INCOMPLETE, not that the energy was free.
	UnpricedKWh float64 `json:"unpriced_kwh,omitempty"`
}

// BillPricing is the money context for a bill: the standing charge for the window
// and the attribution method naming how each device's Cost was arrived at.
//
// Device costs arrive ALREADY COMPUTED in the DeviceCost values rather than being
// derived here, because only the caller knows what it has to price from — a single
// configured rate, or per-half-hour counter deltas against the archive. Assembling
// a bill and pricing energy are different jobs, and a half-hourly tariff is what
// made keeping them in one function untenable.
type BillPricing struct {
	StandingCharge float64
	Attribution    string
}

// DeviceCostFor returns the VAT-inclusive £ cost of kwh at tariff t:
// kWh × unit_rate × (1 + vat_rate). Rates are stored ex-VAT; the gross-up is
// applied here so all money this package produces is VAT-inclusive.
func DeviceCostFor(kwh float64, t config.Tariff) float64 {
	return kwh * t.UnitRate * t.Multiplier()
}

// StandingChargeFor returns the VAT-inclusive £ standing charge for a window of
// the given fractional days at tariff t: days × daily_standing_charge ×
// (1 + vat_rate).
//
// Rounding policy: days is used as-is (the window's fractional Days()). A
// period-to-date window ends at "now" and so covers a partial day; that partial
// day is billed proportionally rather than rounded up to a whole day. This keeps
// the running total monotonic and matches how the meter accrues — no rounding or
// ceiling is applied at this layer.
func StandingChargeFor(days float64, t config.Tariff) float64 {
	return days * t.DailyStandingCharge * t.Multiplier()
}

// StandingChargeAcross sums the standing charge over tariff segments, each at its
// own daily charge and its own VAT rate.
//
// A window spanning a tariff change is not a corner case — the first bill after a
// switchover necessarily is one — and the two sides can differ in both numbers.
// Because PeriodsBetween tiles the window exactly, the apportioned parts add up to
// the window with no gap and no double charge.
func StandingChargeAcross(segments []config.Segment) float64 {
	var total float64
	for _, seg := range segments {
		total += StandingChargeFor(seg.Days(), seg.Tariff)
	}
	return total
}

// PriceFlat fills in each device's Cost at one flat rate.
//
// The flat path, unchanged in behaviour: kWh x unit_rate x (1 + vat_rate). A
// half-hourly tariff must NOT come through here — its UnitRate is zero, so this
// would return the confident £0.00 that the whole slot-costing path exists to
// prevent. FlatPricerFor refuses such a tariff for the same reason.
func PriceFlat(devices []DeviceCost, t config.Tariff) {
	for i := range devices {
		devices[i].Cost = DeviceCostFor(devices[i].KWh, t)
	}
}

// AssembleBill builds a Bill from the billable devices, the whole-house meter
// total, and the window's pricing context.
//
// devices are the BILLABLE devices (plug + UPS) with .KWh, .Cost and any
// .UnpricedKWh already filled in; the meter is NOT one of them. meterKWh is the
// whole-house total (from the electricity_meter counter / house_electricity),
// passed separately, and meterPresent reports whether such a meter is configured
// at all. Effective rates — per device and for the bill — are derived here, so
// there is one definition of them however the costs were priced.
//
// The standing charge is reported ONCE, on the bill, and is never apportioned
// across devices: no device causes it, so splitting it would invent a number that
// looks like a measurement. That is decision D2 in docs/per-device-attribution.md.
//
// When meterPresent is false the meter-derived reconciliation fields are left
// nil (omitted from the wire) instead of being computed from a phantom meterKWh
// of 0 — see Reconciliation for why that distinction matters.
func AssembleBill(window Window, devices []DeviceCost, meterKWh float64, meterPresent bool, pricing BillPricing) Bill {
	var energyCost, monitoredKWh, unpriced float64
	for i := range devices {
		devices[i].EffectiveRate = EffectiveRate(devices[i].Cost, devices[i].KWh)
		energyCost += devices[i].Cost
		monitoredKWh += devices[i].KWh
		unpriced += devices[i].UnpricedKWh
	}

	standing := pricing.StandingCharge

	rec := Reconciliation{MeterPresent: meterPresent, MonitoredKWh: monitoredKWh}
	if meterPresent {
		unmonitored := meterKWh - monitoredKWh
		coverage := 0.0
		if meterKWh != 0 {
			coverage = monitoredKWh / meterKWh
		}
		mk := meterKWh
		rec.MeterKWh = &mk
		rec.UnmonitoredKWh = &unmonitored
		rec.Coverage = &coverage
	}

	return Bill{
		Window:         window.Label,
		Currency:       "GBP",
		Devices:        devices,
		EnergyCost:     energyCost,
		StandingCharge: standing,
		Total:          energyCost + standing,
		Reconciliation: rec,
		Attribution:    pricing.Attribution,
		// Priced energy only: dividing by kWh that carried no price would quietly
		// understate the rate actually paid.
		EffectiveRate: EffectiveRate(energyCost, monitoredKWh-unpriced),
		UnpricedKWh:   unpriced,
	}
}
