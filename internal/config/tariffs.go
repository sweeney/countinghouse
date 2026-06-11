package config

import "time"

// Tariff is a single fuel's pricing as stored in the remote
// `energy_tariffs` namespace. Rates are in GBP (£, not pence) and stored
// ex-VAT; cost math applies ×(1 + VATRate).
//
// v1 carries the current rate only. Effective-date history is a future
// extension: a later schema would add `effective_from`/`previous` and the
// cost layer would split windows at rate boundaries. The selection seam
// for that lives in milestone 4 (tariffFor(t)); these are types only.
type Tariff struct {
	UnitRate            float64 `json:"unit_rate"`             // £/kWh, ex-VAT
	DailyStandingCharge float64 `json:"daily_standing_charge"` // £/day, ex-VAT
	Unit                string  `json:"unit"`
	VATRate             float64 `json:"vat_rate"`
}

// EnergyTariffs is the payload of the `energy_tariffs` namespace. Keys are
// fuel names (e.g. "electricity", "gas"); countinghouse uses only
// "electricity".
type EnergyTariffs struct {
	Tariffs map[string]Tariff `json:"tariffs"`
}

// Electricity returns the electricity tariff and whether it is present.
// Countinghouse bills electricity only; gas (and any other fuel) is ignored.
func (e EnergyTariffs) Electricity() (Tariff, bool) {
	t, ok := e.Tariffs["electricity"]
	return t, ok
}

// TariffFor returns the electricity tariff effective at time t.
//
// This is the selection SEAM for effective-date history. In v1 the schema
// carries the current rate only, so t is ignored and TariffFor is equivalent
// to Electricity(). When the schema gains effective_from/previous, this method
// will select the tariff version effective at t, and callers spanning a rate
// boundary will split their window at the boundary and bill each sub-range with
// its own tariff. The signature takes time.Time now so those callers are
// future-proof and need no change when history lands.
func (e EnergyTariffs) TariffFor(t time.Time) (Tariff, bool) {
	_ = t // v1: no effective-date history; current rate only.
	return e.Electricity()
}

// Multiplier returns the VAT gross-up factor (1 + VATRate). Rates are stored
// ex-VAT; multiply ex-VAT money by this to get the VAT-inclusive amount.
func (t Tariff) Multiplier() float64 {
	return 1 + t.VATRate
}
