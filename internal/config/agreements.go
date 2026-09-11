package config

import (
	"fmt"
	"sort"
	"time"

	"github.com/sweeney/countinghouse/internal/octopus"
)

// Tariff types.
//
// The distinction is not about how the supplier markets the product — it is
// about whether a £/kWh can be read out of config at all:
//
//   - fixed:    the rate is here. One number, valid for the whole block.
//   - variable: the rate is NOT here. It varies within the block (half-hourly),
//     so it lives in the price archive and a consumer asks the price API.
//
// Worth being precise, because "variable" is an overloaded word in energy. A
// supplier's standard variable rate — one number that changes every few months —
// is NOT this type. It has no intra-block curve to look up, so it is expressed
// as successive `fixed` blocks, one per rate. `variable` means specifically
// "priced per half hour from the archive".
const (
	TariffTypeFixed    = "fixed"
	TariffTypeVariable = "variable"
)

// Agreement is one dated tariff commitment: what we were on, between when and
// when, and what it cost.
//
// The name is the supplier's own: an agreement is exactly this — a tariff bound
// to a date range. It replaces the earlier open-ended `periods` shape, and the
// explicit `To` is the reason. With only a start date, blocks implicitly tile
// all of time, which makes two real states inexpressible:
//
//   - a GAP, where nothing covers an instant because we were not a customer.
//     Legitimate, so valid in the document — but refused at pricing time rather
//     than papered over with a neighbouring rate.
//   - an OVERLAP, where two agreements cover one instant. Never legitimate, so
//     the document is refused.
//
// Each block is self-contained: Unit and VATRate are carried per block rather
// than inherited from a parent, so a resolved tariff is complete on its own and
// a historical VAT change is expressed by simply being in the block it applied to.
type Agreement struct {
	// From is the INCLUSIVE start. To is the EXCLUSIVE end, nil when the
	// agreement is still current. Half-open [From, To) throughout, matching
	// every other interval in this service.
	//
	// From is a pointer so ABSENT is representable and distinct from year 1.
	// Validate requires it on a real document — an agreement with no start cannot
	// be placed in history — but the synthesised presentation of a legacy
	// single-rate document legitimately has none, because that document asserts
	// nothing about when its rate began. A nil From means "since before our
	// records", which is exactly what the legacy rate is, and it serialises as an
	// absent field rather than as 0001-01-01.
	From *time.Time `json:"from,omitempty"`
	To   *time.Time `json:"to,omitempty"`

	// Name is for humans: "Agile Octopus". Required, because an unnamed
	// agreement surfaces as its code in any UI, and a code where a name belongs
	// reads as data rather than as a missing label.
	Name string `json:"name"`

	// Type is fixed or variable. See the constants.
	Type string `json:"type"`

	// ID is the supplier's tariff code. REQUIRED on a variable agreement — it is
	// the key its prices are archived under, so without it the block cannot be
	// priced at all. Optional on a fixed agreement, where the rate is already
	// here, but validated when present because it is used for audit.
	ID string `json:"id,omitempty"`

	Unit    string  `json:"unit,omitempty"`
	VATRate float64 `json:"vat_rate"`

	// UnitRate is £/kWh ex-VAT, and must be present on a fixed agreement and
	// ABSENT on a variable one. A rate sitting next to a variable block would be
	// read by somebody, and it would not be the price.
	UnitRate float64 `json:"unit_rate,omitempty"`

	// DailyStandingCharge is £/day ex-VAT. Present on both types: a standing
	// charge is flat per day even when the unit rate is half-hourly.
	DailyStandingCharge float64 `json:"daily_standing_charge"`
}

// EnergyAgreements is the payload of the `energy_agreements` namespace, keyed by
// fuel name. Countinghouse bills "electricity"; other fuels are read, validated
// and ignored.
type EnergyAgreements struct {
	Agreements map[string][]Agreement `json:"agreements"`
}

// TariffSource is what the cost layer needs from a tariff document: price this
// instant, and split this window.
//
// Both document shapes implement it, which is what keeps the migration out of
// the HTTP layer — handlers ask the same two questions regardless of which
// namespace is in use.
type TariffSource interface {
	// TariffFor returns the tariff effective at t, or false when nothing covers
	// it. False is a real answer, not an error: a window may predate our records
	// or fall in a gap, and the caller must surface that rather than bill it.
	TariffFor(t time.Time) (Tariff, bool)

	// PeriodsBetween splits [from, to) into segments each covered by exactly one
	// tariff. It errors rather than returning a partial answer when any part of
	// the window is uncovered.
	PeriodsBetween(from, to time.Time) ([]Segment, error)
}

// resolve turns an agreement into the resolved Tariff the cost layer uses.
func (a Agreement) resolve() Tariff {
	t := Tariff{
		Unit:                a.Unit,
		VATRate:             a.VATRate,
		DailyStandingCharge: a.DailyStandingCharge,
		Name:                a.Name,
	}
	if a.Type == TariffTypeVariable {
		// TariffCode is what marks the resolved tariff half-hourly, so the cost
		// layer needs no knowledge of agreement types at all.
		t.TariffCode = a.ID
		return t
	}
	t.UnitRate = a.UnitRate
	return t
}

// covers reports whether the agreement covers t. A nil From is unbounded in the
// past, a nil To unbounded in the future.
func (a Agreement) covers(t time.Time) bool {
	if a.From != nil && t.Before(*a.From) {
		return false
	}
	return a.To == nil || t.Before(*a.To)
}

// start returns the agreement's inclusive start, or the zero time when unbounded.
func (a Agreement) start() time.Time {
	if a.From == nil {
		return time.Time{}
	}
	return *a.From
}

// end returns the agreement's exclusive end, or the zero time when unbounded.
func (a Agreement) end() time.Time {
	if a.To == nil {
		return time.Time{}
	}
	return *a.To
}

// TariffFor implements TariffSource.
func (e EnergyAgreements) TariffFor(t time.Time) (Tariff, bool) {
	for _, a := range e.electricity() {
		if a.covers(t) {
			return a.resolve(), true
		}
	}
	// Either t predates every agreement, or it falls in a gap. Both mean we do
	// not know what a kWh cost, and the caller must say so rather than reach for
	// the nearest rate.
	return Tariff{}, false
}

// PeriodsBetween implements TariffSource.
//
// Segments tile [from, to) exactly, so an apportioned standing charge adds up to
// the window. A window any part of which has no agreement is an error rather
// than a partial answer: returning only the covered parts would silently
// under-bill, and billing the uncovered part at a neighbouring rate would be
// invisible and wrong.
func (e EnergyAgreements) PeriodsBetween(from, to time.Time) ([]Segment, error) {
	if !to.After(from) {
		return nil, fmt.Errorf("config: agreement window stop (%s) must be after start (%s)",
			to.Format(time.RFC3339), from.Format(time.RFC3339))
	}
	agreements := e.electricity()
	if len(agreements) == 0 {
		return nil, fmt.Errorf("config: no electricity agreements configured")
	}

	var segs []Segment
	cursor := from
	for _, a := range agreements {
		if a.end().IsZero() || a.end().After(cursor) {
			// The first agreement reaching past the cursor must also START at or
			// before it, or the stretch in between is uncovered.
			if a.start().After(cursor) {
				break // leaves cursor short of `to`; reported below
			}
			if !a.covers(cursor) {
				continue
			}
			stop := to
			if e := a.end(); !e.IsZero() && e.Before(stop) {
				stop = e
			}
			segs = append(segs, Segment{Start: cursor, Stop: stop, Tariff: a.resolve()})
			cursor = stop
			if !cursor.Before(to) {
				return segs, nil
			}
		}
	}

	// Anything left means a gap, or running off the end of our records.
	return nil, fmt.Errorf("config: no agreement covers %s (within the window [%s, %s)); "+
		"refusing to price a window we have no tariff for",
		cursor.Format(time.RFC3339), from.Format(time.RFC3339), to.Format(time.RFC3339))
}

// electricity returns the electricity agreements, oldest first.
//
// Sorted on read because config is hand-authored and the order somebody typed
// the blocks in is not a contract.
func (e EnergyAgreements) electricity() []Agreement {
	in := e.Agreements["electricity"]
	out := make([]Agreement, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool { return out[i].start().Before(out[j].start()) })
	return out
}

// Validate refuses the documents that would make money wrong or ambiguous.
//
// Called when the namespace is applied, so a bad document never prices anything.
// An invalid document is treated exactly like a failed fetch — keep the
// last-known snapshot, degrade health — and with the existing cold-start rule
// that gives the project's usual shape: boot needs truth, running keeps the last
// truth.
//
// Every fuel is validated, not only the one we bill: a document is either
// coherent or it is not, and letting a broken gas block through would leave it
// for whatever bills gas next.
func (e EnergyAgreements) Validate() error {
	fuels := make([]string, 0, len(e.Agreements))
	for fuel := range e.Agreements {
		fuels = append(fuels, fuel)
	}
	sort.Strings(fuels) // deterministic error messages

	for _, fuel := range fuels {
		blocks := e.Agreements[fuel]
		for i, a := range blocks {
			if err := a.validate(fmt.Sprintf("%s agreement %d (%q)", fuel, i, a.Name)); err != nil {
				return err
			}
		}
		if err := validateNoOverlap(fuel, blocks); err != nil {
			return err
		}
	}
	return nil
}

// validate checks one block in isolation.
func (a Agreement) validate(where string) error {
	if a.From == nil {
		return fmt.Errorf("config: %s has no `from`; an agreement without a start date "+
			"cannot be placed in history", where)
	}
	if a.To != nil && !a.To.After(*a.From) {
		return fmt.Errorf("config: %s has `to` (%s) at or before `from` (%s), so it covers nothing",
			where, a.To.Format(time.RFC3339), a.From.Format(time.RFC3339))
	}
	if a.Name == "" {
		return fmt.Errorf("config: %s has no `name`; an unnamed agreement surfaces as its "+
			"code, which reads as data rather than as a missing label", where)
	}

	switch a.Type {
	case TariffTypeFixed:
		if a.UnitRate <= 0 {
			return fmt.Errorf("config: %s is `fixed` but has no `unit_rate`, so it prices nothing", where)
		}
	case TariffTypeVariable:
		if a.UnitRate != 0 {
			return fmt.Errorf("config: %s is `variable` but also sets `unit_rate` (%v); "+
				"a variable agreement's price comes from the price archive, and a number here "+
				"would be read by somebody and would not be the price", where, a.UnitRate)
		}
		if a.ID == "" {
			return fmt.Errorf("config: %s is `variable` but has no `id`; without the tariff code "+
				"its prices are archived under, it cannot be priced at all", where)
		}
	case "":
		return fmt.Errorf("config: %s has no `type`; want %q or %q", where, TariffTypeFixed, TariffTypeVariable)
	default:
		return fmt.Errorf("config: %s has unknown `type` %q; want %q or %q",
			where, a.Type, TariffTypeFixed, TariffTypeVariable)
	}

	// Validated whenever present, for either type: a malformed code is refused at
	// load, while somebody is watching, rather than at fetch time.
	if a.ID != "" {
		if _, err := octopus.ParseTariffCode(a.ID); err != nil {
			return fmt.Errorf("config: %s has an invalid `id`: %w", where, err)
		}
	}
	if a.VATRate < 0 {
		return fmt.Errorf("config: %s has a negative `vat_rate` (%v)", where, a.VATRate)
	}
	if a.DailyStandingCharge < 0 {
		return fmt.Errorf("config: %s has a negative `daily_standing_charge` (%v)",
			where, a.DailyStandingCharge)
	}
	return nil
}

// validateNoOverlap refuses two agreements covering one instant.
//
// Gaps are deliberately NOT refused — being between suppliers is a real state,
// and the document should be able to say so. Overlaps are, because two prices
// for one kWh has no defensible resolution: picking one by an arbitrary rule
// like "last wins" would be a silent decision nobody would remember making.
func validateNoOverlap(fuel string, blocks []Agreement) error {
	sorted := make([]Agreement, len(blocks))
	copy(sorted, blocks)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].start().Before(sorted[j].start()) })

	for i := 1; i < len(sorted); i++ {
		prev, cur := sorted[i-1], sorted[i]
		// An unbounded earlier agreement overlaps everything after it.
		if prev.To == nil {
			return fmt.Errorf("config: %s agreements %q and %q overlap: %q is open-ended "+
				"but %q starts at %s; only the most recent agreement may omit `to`",
				fuel, prev.Name, cur.Name, prev.Name, cur.Name, cur.start().Format(time.RFC3339))
		}
		if prev.To.After(cur.start()) {
			return fmt.Errorf("config: %s agreements %q and %q overlap between %s and %s; "+
				"two tariffs cannot both price one kWh",
				fuel, prev.Name, cur.Name,
				cur.start().Format(time.RFC3339), prev.To.Format(time.RFC3339))
		}
	}
	return nil
}

// AsAgreements presents the legacy single-rate document in the new shape.
//
// It exists so /tariffs can serve ONE response shape whichever namespace backs
// it, rather than making every consumer handle both. The synthesised block has
// NEITHER bound: the legacy document asserts nothing about when its rate started
// or ends, and inventing dates would be fabrication. Unbounded at both ends is
// also exactly how that rate behaves — it prices every instant.
func (e EnergyTariffs) AsAgreements() EnergyAgreements {
	out := EnergyAgreements{Agreements: map[string][]Agreement{}}
	for fuel, t := range e.Tariffs {
		out.Agreements[fuel] = []Agreement{{
			Name:                legacyName(fuel),
			Type:                TariffTypeFixed,
			Unit:                t.Unit,
			VATRate:             t.VATRate,
			UnitRate:            t.UnitRate,
			DailyStandingCharge: t.DailyStandingCharge,
		}}
	}
	return out
}

// legacyName labels a synthesised block. The legacy document has no name field,
// and the label says where the block came from rather than inventing a product.
func legacyName(fuel string) string {
	return fmt.Sprintf("%s (from energy_tariffs)", fuel)
}

// VariableTariffCodes returns every distinct electricity tariff code whose prices
// must be archived, sorted for determinism.
//
// It returns ALL of them, not just the currently effective one, because a
// SUPERSEDED half-hourly agreement's prices are still needed to bill its window.
// Dropping an old code would make a past month quietly unbillable the moment the
// tariff changed — and the symptom would appear long after the cause.
//
// Electricity only: countinghouse does not bill gas, so a half-hourly gas
// agreement must not conscript a collector.
func (e EnergyAgreements) VariableTariffCodes() []string {
	seen := map[string]bool{}
	for _, a := range e.Agreements["electricity"] {
		if a.Type == TariffTypeVariable && a.ID != "" {
			seen[a.ID] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for code := range seen {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

// CheckArchiveRequired refuses the one combination that would fail silently: a
// half-hourly agreement with nowhere to keep prices.
//
// A flat-only deployment needs no archive and must not be made to configure one.
// But a half-hourly agreement prices nothing without the archive, so a service
// starting in that state would look healthy and then refuse every window after the
// switchover — with the cause nowhere near the symptom. Refusing at boot puts the
// two together.
//
// Called after the first successful fetch, because it is a question about the
// remote document and the local config TOGETHER; neither alone can answer it.
func CheckArchiveRequired(archivePath string, agreements EnergyAgreements) error {
	if archivePath != "" {
		return nil
	}
	for _, a := range agreements.Agreements["electricity"] {
		if a.Type != TariffTypeVariable {
			continue
		}
		return fmt.Errorf("config: electricity agreement %q (%s) is half-hourly, so its prices "+
			"come from the archive — but no prices.db_path is configured, so nothing can be "+
			"stored and every window it covers would be unpriceable", a.Name, a.ID)
	}
	return nil
}
