package config

import (
	"fmt"
	"sort"
	"time"

	"github.com/sweeney/countinghouse/internal/octopus"
)

// Tariff is a fuel's pricing as stored in the remote `energy_tariffs` namespace.
// Rates are in GBP (£, not pence) and stored ex-VAT; cost math applies
// ×(1 + VATRate).
//
// The type is used in TWO positions, which is why it is recursive:
//
//   - as a fuel entry, where Periods carries the effective-date history and the
//     top-level Unit/VATRate are inherited by each period;
//   - as one PERIOD inside that history, and as the resolved tariff TariffFor
//     returns for an instant.
//
// A fuel entry with no Periods is the legacy single-rate shape, which every
// existing deployment is on and which keeps working unchanged: the single rate
// applies at every instant.
type Tariff struct {
	UnitRate            float64 `json:"unit_rate,omitempty"`             // £/kWh, ex-VAT
	DailyStandingCharge float64 `json:"daily_standing_charge,omitempty"` // £/day, ex-VAT
	Unit                string  `json:"unit,omitempty"`
	VATRate             float64 `json:"vat_rate,omitempty"`

	// TariffCode marks a HALF-HOURLY period: the price of a kWh varies within
	// it, so UnitRate is meaningless and the rate comes from the price archive
	// instead. The standing charge is still flat per day and still lives here.
	//
	// Carrying the code rather than a `kind: agile` flag is deliberate. A flat
	// tariff and a half-hourly one are the same relation at different row
	// densities (docs/octopus-price-data-model.md §3), so what the cost path
	// needs is not a type discriminator but the key to look prices up under.
	TariffCode string `json:"tariff_code,omitempty"`

	// EffectiveFrom is the INCLUSIVE start of this period. It is required on a
	// period and meaningless on a fuel entry. A pointer so "absent" is
	// distinguishable from the zero time, which lets Validate reject an
	// undated period instead of silently treating it as beginning at year 1.
	EffectiveFrom *time.Time `json:"effective_from,omitempty"`

	// Periods is the effective-date history, oldest first after Validate has
	// sorted it. Only meaningful on a fuel entry.
	Periods []Tariff `json:"periods,omitempty"`
}

// IsHalfHourly reports whether this period's unit rate comes from the price
// archive rather than from config. Callers that read UnitRate MUST check this
// first: on a half-hourly period UnitRate is zero, and using it would price
// energy at nothing.
func (t Tariff) IsHalfHourly() bool { return t.TariffCode != "" }

// Multiplier returns the VAT gross-up factor (1 + VATRate). Rates are stored
// ex-VAT; multiply ex-VAT money by this to get the VAT-inclusive amount.
func (t Tariff) Multiplier() float64 { return 1 + t.VATRate }

// EnergyTariffs is the payload of the `energy_tariffs` namespace. Keys are fuel
// names (e.g. "electricity", "gas"); countinghouse bills only "electricity".
type EnergyTariffs struct {
	Tariffs map[string]Tariff `json:"tariffs"`
}

// Electricity returns the electricity FUEL ENTRY and whether it is present.
//
// Note this is the entry, not a resolved tariff: when a history is configured
// its UnitRate is empty and the rates live in Periods. Use TariffFor to price
// an instant. It remains exported because /tariffs surfaces the document.
func (e EnergyTariffs) Electricity() (Tariff, bool) {
	t, ok := e.Tariffs["electricity"]
	return t, ok
}

// Segment is one sub-range of a window over which a single tariff applies.
// Start is inclusive, Stop exclusive.
type Segment struct {
	Start, Stop time.Time
	Tariff      Tariff
}

// Days returns the segment's length in days, used for apportioning the standing
// charge. Fractional, and computed from real elapsed time so a segment spanning
// a DST changeover counts the 23 or 25 hours it actually lasted.
func (s Segment) Days() float64 { return s.Stop.Sub(s.Start).Hours() / 24 }

// TariffFor returns the electricity tariff effective at instant t.
//
// With no history configured, the single configured rate applies at every
// instant — the backwards-compatible path.
//
// With a history, the period selected is the latest one whose EffectiveFrom is
// at or before t. An instant BEFORE the earliest period reports not-ok: we do
// not know what a kWh cost then, and guessing with the oldest rate we happen to
// hold would produce a confident wrong number. The caller must surface that
// rather than bill it.
//
// The returned Tariff inherits Unit and VATRate from the fuel entry unless the
// period overrides them, so the common case stays terse while a historical VAT
// change can still be expressed.
func (e EnergyTariffs) TariffFor(t time.Time) (Tariff, bool) {
	fuel, ok := e.Electricity()
	if !ok {
		return Tariff{}, false
	}
	if len(fuel.Periods) == 0 {
		return fuel, true
	}

	periods := sortedPeriods(fuel.Periods)

	// Walk newest-first and take the first period that has already started.
	for i := len(periods) - 1; i >= 0; i-- {
		p := periods[i]
		if p.EffectiveFrom == nil {
			continue // Validate rejects these; defensive.
		}
		if !t.Before(*p.EffectiveFrom) {
			return resolve(fuel, p), true
		}
	}
	// t predates every period we know about.
	return Tariff{}, false
}

// PeriodsBetween splits [from, to) into segments, each covered by exactly one
// tariff, oldest first.
//
// This is what the cost layer needs to bill a window that spans a rate change —
// including the first bill after a switchover to a half-hourly tariff, which
// necessarily spans one. The segments tile the window exactly: no gaps, no
// overlaps, so the apportioned standing charge adds up to the window.
//
// If any part of the window predates the earliest known tariff, this is an
// error rather than a partial answer. Billing the uncovered head at the oldest
// rate we hold would be invisible and wrong; returning only the covered part
// would silently under-bill.
func (e EnergyTariffs) PeriodsBetween(from, to time.Time) ([]Segment, error) {
	if !to.After(from) {
		return nil, fmt.Errorf("config: tariff window stop (%s) must be after start (%s)", to, from)
	}
	fuel, ok := e.Electricity()
	if !ok {
		return nil, fmt.Errorf("config: no electricity tariff configured")
	}
	if len(fuel.Periods) == 0 {
		return []Segment{{Start: from, Stop: to, Tariff: fuel}}, nil
	}

	periods := sortedPeriods(fuel.Periods)
	if first := periods[0].EffectiveFrom; first != nil && from.Before(*first) {
		return nil, fmt.Errorf("config: no tariff covers %s; the earliest configured tariff begins %s",
			from.Format(time.RFC3339), first.Format(time.RFC3339))
	}

	// Period boundaries partition all of time, so intersecting each period with
	// [from, to) and dropping the empty results tiles the window exactly — no
	// cursor bookkeeping, no chance of a gap or an overlap.
	var segs []Segment
	for i, p := range periods {
		if p.EffectiveFrom == nil {
			continue // Validate refuses these; defensive.
		}
		start := *p.EffectiveFrom
		if from.After(start) {
			start = from
		}
		// This period runs until the next one begins, or forever.
		end := to
		if i+1 < len(periods) && periods[i+1].EffectiveFrom != nil {
			if next := *periods[i+1].EffectiveFrom; next.Before(end) {
				end = next
			}
		}
		if !end.After(start) {
			continue // no overlap with the window
		}
		segs = append(segs, Segment{Start: start, Stop: end, Tariff: resolve(fuel, p)})
	}

	if len(segs) == 0 {
		return nil, fmt.Errorf("config: no tariff covers [%s, %s)",
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	return segs, nil
}

// Validate checks the document for the ambiguities that would make money wrong.
//
// It is called when the namespace is applied, so a bad document is refused
// before it can price anything. Per the project's boot rule, a refusal at
// startup aborts (there is no last-known snapshot to fall back to) while a
// refusal later keeps the last-known document and degrades health.
//
// Every fuel is validated, not just electricity: a document is either coherent
// or it is not, and failing only on the fuel we happen to bill would let a
// broken gas entry through to whatever bills gas next.
func (e EnergyTariffs) Validate() error {
	fuels := make([]string, 0, len(e.Tariffs))
	for fuel := range e.Tariffs {
		fuels = append(fuels, fuel)
	}
	sort.Strings(fuels) // deterministic error messages

	for _, fuel := range fuels {
		entry := e.Tariffs[fuel]
		if entry.VATRate < 0 {
			return fmt.Errorf("config: %s vat_rate %v is negative", fuel, entry.VATRate)
		}

		seen := map[time.Time]bool{}
		for i, p := range entry.Periods {
			where := fmt.Sprintf("%s period %d", fuel, i)

			if p.EffectiveFrom == nil {
				return fmt.Errorf("config: %s has no effective_from; "+
					"a dated history cannot contain an undated period", where)
			}
			if seen[*p.EffectiveFrom] {
				return fmt.Errorf("config: %s has a duplicate effective_from %s; "+
					"two rates starting at the same instant is ambiguous",
					where, p.EffectiveFrom.Format(time.RFC3339))
			}
			seen[*p.EffectiveFrom] = true

			hasRate := p.UnitRate != 0
			hasCode := p.TariffCode != ""
			switch {
			case hasRate && hasCode:
				return fmt.Errorf("config: %s sets both unit_rate and tariff_code; "+
					"a period is either flat-rate or half-hourly, not both", where)
			case !hasRate && !hasCode:
				return fmt.Errorf("config: %s sets neither unit_rate nor tariff_code, "+
					"so it prices nothing", where)
			}
			if hasCode {
				// Refuse an unparseable code at load rather than at fetch time:
				// boot is when somebody is watching.
				if _, err := octopus.ParseTariffCode(p.TariffCode); err != nil {
					return fmt.Errorf("config: %s has an invalid tariff_code: %w", where, err)
				}
			}
			if p.VATRate < 0 {
				return fmt.Errorf("config: %s vat_rate %v is negative", where, p.VATRate)
			}
			if p.DailyStandingCharge < 0 {
				return fmt.Errorf("config: %s daily_standing_charge %v is negative", where, p.DailyStandingCharge)
			}
		}
	}
	return nil
}

// sortedPeriods returns the periods ordered oldest-first.
//
// Config is hand-authored, so the order somebody typed them in is not a
// contract. Undated periods sort first and are skipped by callers; Validate
// refuses them outright, so this only matters defensively.
func sortedPeriods(in []Tariff) []Tariff {
	out := make([]Tariff, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool {
		switch {
		case out[i].EffectiveFrom == nil:
			return out[j].EffectiveFrom != nil
		case out[j].EffectiveFrom == nil:
			return false
		default:
			return out[i].EffectiveFrom.Before(*out[j].EffectiveFrom)
		}
	})
	return out
}

// resolve fills a period's inherited fields from its fuel entry.
//
// Unit and VATRate default to the fuel's, so the common case needs them stated
// once. A period may override VATRate because VAT rates change and a historical
// bill must use the rate that applied on the day.
func resolve(fuel, period Tariff) Tariff {
	out := period
	out.Periods = nil // a resolved tariff is a leaf
	if out.Unit == "" {
		out.Unit = fuel.Unit
	}
	if out.VATRate == 0 {
		out.VATRate = fuel.VATRate
	}
	return out
}
