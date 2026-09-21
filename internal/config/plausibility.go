package config

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// ---------------------------------------------------------------------------
// Config rates through the gate model the price archive already has
// (issue #36 finding 8).
//
// Unit rates arrive from remote config in one VAT basis and the service applies
// vat_rate on top. If a rate ever lands in the wrong basis, or with a decimal
// point in the wrong place, nothing downstream can tell: every response stays
// well-formed and internally consistent, just uniformly wrong. Given the
// project's stated preference for refusing at startup over serving a
// plausible-looking wrong answer, that gap is worth closing.
//
// internal/prices/validate.go already implements exactly the right model, with
// better vocabulary than "sanity band", so this reuses its shape rather than
// inventing a second one:
//
//	Gate A  structural    REFUSE AT BOOT  — the value cannot be money
//	Gate B  plausibility  FLAG, serve     — the value is surprising, not wrong
//	Gate C  set level     REPORT          — the DOCUMENT is odd, the rows are fine
//
// Gate A already existed here: Agreement.validate refuses a non-positive fixed
// unit rate, a negative VAT rate and a negative standing charge. This file adds
// the two gates above it, which FLAG rather than refuse — matching the fail-open
// posture remote config already has, where a later fetch failure degrades
// /healthz instead of aborting.
//
// EXPLICIT NON-GOAL, and the honest limit of the whole idea: a band CANNOT catch
// a wrong VAT basis. An archive price can be cross-checked against its own
// inc/exc pair (|inc − exc×(1+vat)| ≤ ε); a config rate arrives as a single
// number with nothing to check it against, and a 5% error sits comfortably inside
// any band wide enough not to fire on real market moves. Closing that needs
// either an inc/exc pair in the namespace document or a reconciliation against a
// real supplier bill. Nobody should later assume this covers it.
// ---------------------------------------------------------------------------

// Plausibility bands for electricity, chosen the way validate.go chooses its
// own: far outside anything the market has done, tight enough to catch a basis
// or scale error.
const (
	// MinPlausibleUnitRate and MaxPlausibleUnitRate bound £/kWh EX VAT. The 2022
	// price-cap peak was around £0.52/kWh inc VAT, so 1.50 leaves room for a
	// market nobody has seen while still catching a pence/pounds confusion (a
	// 20p/kWh rate entered as 20 would read as £20/kWh) or a factor of ten.
	MinPlausibleUnitRate = 0.01
	MaxPlausibleUnitRate = 1.50

	// MinPlausibleStandingCharge and MaxPlausibleStandingCharge bound £/day ex
	// VAT. Standing charges have run around £0.30–£0.70/day; the same reasoning
	// about scale errors applies.
	MinPlausibleStandingCharge = 0.05
	MaxPlausibleStandingCharge = 3.00

	// MaxPlausibleJump and MinPlausibleJump bound the step between one dated
	// block's unit rate and the next.
	//
	// This is the row that WOULD catch a wrong basis, and does not: a
	// VAT-basis error is a 1.05x step, which sits inside this band by design,
	// because a band tight enough to fire on 1.05x would fire on every real
	// price-cap movement. See the non-goal above.
	MaxPlausibleJump = 1.4
	MinPlausibleJump = 0.7
)

// PlausibleVATRates is the set a UK domestic electricity VAT rate can be.
//
// Zero is in the set and is not a mistake: the statutory zero rate on domestic
// electricity in Great Britain, 1 Oct 2026 – 31 Mar 2027, is a legitimate dated
// block. 0.20 is here because a document may describe a non-domestic supply.
var PlausibleVATRates = []float64{0, 0.05, 0.20}

// RateWarning is one Gate B or Gate C finding: something surprising, served
// anyway.
//
// Fail-open by construction — these never stop a boot. A rate outside a band is
// far more likely to be an unusual tariff than a corrupt document, and refusing
// to serve a whole home's energy history over a suspicious number would be a
// worse failure than flagging it.
type RateWarning struct {
	// Fuel and Where locate the finding for a human reading /healthz.
	Fuel  string `json:"fuel"`
	Where string `json:"where"`

	// Kind is a stable string, usable as a metric label — the same discipline
	// prices.WarningKind follows.
	Kind string `json:"kind"`

	// Detail says what was seen and what was expected.
	Detail string `json:"detail"`
}

// Warning kinds. Stable strings.
const (
	WarnUnitRateBand       = "unit_rate_out_of_band"
	WarnStandingChargeBand = "standing_charge_out_of_band"
	WarnVATRateUnexpected  = "vat_rate_unexpected"
	WarnRateJump           = "unit_rate_jump"
	WarnNoCurrentAgreement = "no_current_agreement"
)

// PlausibilityWarnings runs Gates B and C over the agreements document.
//
// now comes from the caller's injected clock — this package never calls
// time.Now. A ZERO now skips the Gate C coverage check rather than reporting
// every document as uncovered, so a caller with no clock still gets the bands.
func (e EnergyAgreements) PlausibilityWarnings(now time.Time) []RateWarning {
	fuels := make([]string, 0, len(e.Agreements))
	for fuel := range e.Agreements {
		fuels = append(fuels, fuel)
	}
	sort.Strings(fuels) // deterministic output: this reaches /healthz

	var out []RateWarning
	for _, fuel := range fuels {
		blocks := e.Agreements[fuel]
		out = append(out, plausibilityForFuel(fuel, blocks)...)

		// Gate C, set level: the individual blocks are fine, the DOCUMENT is odd.
		//
		// A fuel with no block covering now cannot be priced today, which is a
		// configuration mistake rather than a data one — and is exactly the state
		// that otherwise shows up much later as unpriced_kwh on a bill nobody was
		// watching. Reported, not refused: being between suppliers is a real state
		// the document is allowed to describe, the same reason validateNoOverlap
		// refuses overlaps but permits gaps.
		if !now.IsZero() && !coversInstant(blocks, now) {
			out = append(out, RateWarning{
				Fuel: fuel, Where: "(document)", Kind: WarnNoCurrentAgreement,
				Detail: "no agreement covers the present instant, so this fuel cannot be " +
					"priced today; energy in that stretch will surface as unpriced_kwh",
			})
		}
	}
	return out
}

// coversInstant reports whether any block covers t, half-open [From, To) as
// everywhere else in this service.
func coversInstant(blocks []Agreement, t time.Time) bool {
	for _, a := range blocks {
		if a.From != nil && t.Before(*a.From) {
			continue
		}
		if a.To != nil && !t.Before(*a.To) {
			continue
		}
		return true
	}
	return false
}

// plausibilityForFuel runs the per-block bands and the adjacent-block jump.
func plausibilityForFuel(fuel string, blocks []Agreement) []RateWarning {
	var out []RateWarning

	// Sorted by start so "the previous block" means the previous one in time
	// rather than in document order.
	ordered := make([]Agreement, len(blocks))
	copy(ordered, blocks)
	sort.SliceStable(ordered, func(i, j int) bool {
		switch {
		case ordered[i].From == nil:
			return true
		case ordered[j].From == nil:
			return false
		default:
			return ordered[i].From.Before(*ordered[j].From)
		}
	})

	var prevRate float64
	var prevName string
	for _, a := range ordered {
		where := a.Name
		if where == "" {
			where = "(unnamed)"
		}

		// A variable agreement carries no unit rate BY DESIGN — its price is in the
		// archive — so banding a zero there would flag every half-hourly tariff.
		if a.Type == TariffTypeFixed {
			if a.UnitRate < MinPlausibleUnitRate || a.UnitRate > MaxPlausibleUnitRate {
				out = append(out, RateWarning{
					Fuel: fuel, Where: where, Kind: WarnUnitRateBand,
					Detail: fmt.Sprintf("unit_rate %v £/kWh ex-VAT is outside the plausible band %v..%v; "+
						"a rate in pence rather than pounds would look like this",
						a.UnitRate, MinPlausibleUnitRate, MaxPlausibleUnitRate),
				})
			}
			if prevRate > 0 && a.UnitRate > 0 {
				if ratio := a.UnitRate / prevRate; ratio > MaxPlausibleJump || ratio < MinPlausibleJump {
					out = append(out, RateWarning{
						Fuel: fuel, Where: where, Kind: WarnRateJump,
						Detail: fmt.Sprintf("unit_rate moved %.2fx from %q (%v to %v £/kWh ex-VAT), "+
							"outside %v..%v", ratio, prevName, prevRate, a.UnitRate,
							MinPlausibleJump, MaxPlausibleJump),
					})
				}
			}
			prevRate, prevName = a.UnitRate, where
		}

		// Standing charges are present on both types: flat per day even when the
		// unit rate is half-hourly. A zero is legal (some tariffs have none), so
		// only a positive value outside the band is surprising.
		if a.DailyStandingCharge > 0 &&
			(a.DailyStandingCharge < MinPlausibleStandingCharge || a.DailyStandingCharge > MaxPlausibleStandingCharge) {
			out = append(out, RateWarning{
				Fuel: fuel, Where: where, Kind: WarnStandingChargeBand,
				Detail: fmt.Sprintf("daily_standing_charge %v £/day ex-VAT is outside the plausible "+
					"band %v..%v", a.DailyStandingCharge,
					MinPlausibleStandingCharge, MaxPlausibleStandingCharge),
			})
		}

		if !knownVATRate(a.VATRate) {
			out = append(out, RateWarning{
				Fuel: fuel, Where: where, Kind: WarnVATRateUnexpected,
				Detail: fmt.Sprintf("vat_rate %v is not one of %v; note that 0 is legitimate for the "+
					"statutory zero rate on domestic electricity, 1 Oct 2026 to 31 Mar 2027",
					a.VATRate, PlausibleVATRates),
			})
		}
	}
	return out
}

// knownVATRate reports whether r is one of the rates a UK domestic electricity
// supply can carry. Compared with a tolerance because these arrive as JSON
// floats.
func knownVATRate(r float64) bool {
	for _, k := range PlausibleVATRates {
		if math.Abs(r-k) < 1e-9 {
			return true
		}
	}
	return false
}
