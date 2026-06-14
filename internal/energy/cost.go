package energy

import "github.com/sweeney/countinghouse/internal/config"

// DeviceCost is one billable device's energy and money for a window. KWh is the
// metered energy; Cost is the VAT-inclusive £ cost for that energy at the
// window's tariff. DisplayName/Location/Class are descriptive passthrough from
// device config for the /bill breakdown.
type DeviceCost struct {
	DeviceID    string  `json:"device_id"`
	DisplayName string  `json:"display_name"`
	Location    string  `json:"location"`
	Class       string  `json:"class"`
	KWh         float64 `json:"kwh"`
	Cost        float64 `json:"cost"`
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

// AssembleBill builds a Bill from the billable devices, the whole-house meter
// total, and the tariff.
//
// devices are the BILLABLE devices (plug + UPS) with .KWh already filled in;
// the meter is NOT one of them. meterKWh is the whole-house total (from the
// electricity_meter counter / house_electricity), passed separately, and
// meterPresent reports whether such a meter is configured at all. Each device's
// .Cost is computed here from its .KWh.
//
// When meterPresent is false the meter-derived reconciliation fields are left
// nil (omitted from the wire) instead of being computed from a phantom meterKWh
// of 0 — see Reconciliation for why that distinction matters.
func AssembleBill(window Window, devices []DeviceCost, meterKWh float64, meterPresent bool, t config.Tariff) Bill {
	var energyCost, monitoredKWh float64
	for i := range devices {
		devices[i].Cost = DeviceCostFor(devices[i].KWh, t)
		energyCost += devices[i].Cost
		monitoredKWh += devices[i].KWh
	}

	standing := StandingChargeFor(window.Days(), t)

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
	}
}
