package config

import (
	"fmt"
	"sort"
	"time"
)

// Tariff is a RESOLVED tariff: what a kWh and a day cost at one instant.
//
// It is what the cost layer consumes, and it is produced by both tariff
// documents — the legacy `energy_tariffs` single-rate shape and the dated blocks
// of `energy_agreements`. Keeping one resolved type is what lets handlers ask
// `TariffFor(t)` without knowing which namespace is in use.
//
// Rates are GBP (£, not pence) and ex-VAT; cost math applies ×(1 + VATRate).
type Tariff struct {
	UnitRate            float64 `json:"unit_rate,omitempty"`             // £/kWh, ex-VAT
	DailyStandingCharge float64 `json:"daily_standing_charge,omitempty"` // £/day, ex-VAT
	Unit                string  `json:"unit,omitempty"`
	VATRate             float64 `json:"vat_rate"`

	// Name is the agreement's human label, when the document carries one.
	Name string `json:"name,omitempty"`

	// TariffCode is set when the unit rate is HALF-HOURLY: it varies within the
	// period, so it comes from the price archive rather than from config, and this
	// is the key it is archived under. UnitRate is zero in that case.
	//
	// Its presence, rather than a separate type field, is what marks a resolved
	// tariff as half-hourly — so the cost layer needs no knowledge of the
	// document's vocabulary.
	TariffCode string `json:"tariff_code,omitempty"`
}

// IsHalfHourly reports whether the unit rate must be looked up per half hour
// rather than read from UnitRate.
//
// Callers that read UnitRate MUST check this first. On a half-hourly tariff
// UnitRate is zero, so ignoring it bills energy at nothing — which is at least
// loud. The alternative design, carrying a "representative" rate, would bill a
// plausible wrong number instead.
func (t Tariff) IsHalfHourly() bool { return t.TariffCode != "" }

// Multiplier returns the VAT gross-up factor (1 + VATRate). Rates are stored
// ex-VAT; multiply ex-VAT money by this to get the VAT-inclusive amount.
func (t Tariff) Multiplier() float64 { return 1 + t.VATRate }

// Segment is one sub-range of a window over which a single tariff applies.
// Start is inclusive, Stop exclusive.
type Segment struct {
	Start, Stop time.Time
	Tariff      Tariff
}

// Days returns the segment's length in days, for apportioning the standing
// charge. Fractional, and computed from real elapsed time so a segment spanning
// a DST changeover counts the 23 or 25 hours it actually lasted.
func (s Segment) Days() float64 { return s.Stop.Sub(s.Start).Hours() / 24 }

// EnergyTariffs is the payload of the LEGACY `energy_tariffs` namespace: one
// current rate per fuel, with no dates.
//
// It is kept working unchanged because it is what is deployed. It cannot express
// history — every instant resolves to the same rate — which is precisely why
// `energy_agreements` exists. New deployments should use that; this shape is not
// extended.
type EnergyTariffs struct {
	Tariffs map[string]Tariff `json:"tariffs"`
}

// Electricity returns the electricity tariff and whether it is present.
// Countinghouse bills electricity only; other fuels are ignored.
func (e EnergyTariffs) Electricity() (Tariff, bool) {
	t, ok := e.Tariffs["electricity"]
	return t, ok
}

// TariffFor implements TariffSource.
//
// The legacy document carries no dates, so the configured rate applies at every
// instant and t is genuinely irrelevant. That is a limitation, not a feature: a
// historical window is priced at today's rate, which is wrong whenever the rate
// has ever changed. `energy_agreements` is the fix; this is the compatible path
// for a deployment that has not migrated.
func (e EnergyTariffs) TariffFor(t time.Time) (Tariff, bool) {
	_ = t // no dates in this document; see the doc comment
	return e.Electricity()
}

// PeriodsBetween implements TariffSource: one segment covering the whole window,
// since there are no rate boundaries to split at.
func (e EnergyTariffs) PeriodsBetween(from, to time.Time) ([]Segment, error) {
	if !to.After(from) {
		return nil, fmt.Errorf("config: tariff window stop (%s) must be after start (%s)",
			to.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	t, ok := e.Electricity()
	if !ok {
		return nil, fmt.Errorf("config: no electricity tariff configured")
	}
	return []Segment{{Start: from, Stop: to, Tariff: t}}, nil
}

// Validate checks the legacy document.
//
// There is little to check — the shape cannot be ambiguous, because it cannot
// express anything but one rate — but a negative VAT rate would silently reduce
// every bill, so it is refused.
func (e EnergyTariffs) Validate() error {
	fuels := make([]string, 0, len(e.Tariffs))
	for fuel := range e.Tariffs {
		fuels = append(fuels, fuel)
	}
	sort.Strings(fuels) // deterministic error messages

	for _, fuel := range fuels {
		t := e.Tariffs[fuel]
		if t.VATRate < 0 {
			return fmt.Errorf("config: %s vat_rate %v is negative", fuel, t.VATRate)
		}
		if t.DailyStandingCharge < 0 {
			return fmt.Errorf("config: %s daily_standing_charge %v is negative", fuel, t.DailyStandingCharge)
		}
	}
	return nil
}
