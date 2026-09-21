package config

import (
	"testing"
	"time"
)

func tp(s string) *time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return &t
}

func kinds(ws []RateWarning) map[string]int {
	out := map[string]int{}
	for _, w := range ws {
		out[w.Kind]++
	}
	return out
}

func elec(blocks ...Agreement) EnergyAgreements {
	return EnergyAgreements{Agreements: map[string][]Agreement{"electricity": blocks}}
}

// A plausible document says nothing. A warning list that is noisy on correct
// config is one operators learn to ignore.
func TestPlausibilityIsQuietOnAGoodDocument(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	got := elec(Agreement{
		From: tp("2025-01-01T00:00:00Z"), Name: "Fixed 12M", Type: TariffTypeFixed,
		UnitRate: 0.2089, DailyStandingCharge: 0.53, VATRate: 0.05,
	}).PlausibilityWarnings(now)

	if len(got) != 0 {
		t.Errorf("a plausible document should warn about nothing, got %+v", got)
	}
}

// The scale error this exists for: 20.89 p/kWh entered as a number of pence
// where pounds were expected.
func TestPlausibilityCatchesAPenceForPoundsUnitRate(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	got := elec(Agreement{
		From: tp("2025-01-01T00:00:00Z"), Name: "Wrong scale", Type: TariffTypeFixed,
		UnitRate: 20.89, DailyStandingCharge: 0.53, VATRate: 0.05,
	}).PlausibilityWarnings(now)

	if kinds(got)[WarnUnitRateBand] != 1 {
		t.Errorf("want one unit-rate band warning, got %+v", got)
	}
}

func TestPlausibilityCatchesAMisScaledStandingCharge(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	got := elec(Agreement{
		From: tp("2025-01-01T00:00:00Z"), Name: "Wrong scale", Type: TariffTypeFixed,
		UnitRate: 0.2089, DailyStandingCharge: 53, VATRate: 0.05,
	}).PlausibilityWarnings(now)

	if kinds(got)[WarnStandingChargeBand] != 1 {
		t.Errorf("want one standing-charge band warning, got %+v", got)
	}
}

// A zero VAT rate is LEGITIMATE — the statutory zero rate on domestic
// electricity, 1 Oct 2026 to 31 Mar 2027 — and flagging it would fire on
// correct config for six months.
func TestPlausibilityAcceptsTheStatutoryZeroVATRate(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	got := elec(Agreement{
		From: tp("2026-10-01T00:00:00Z"), To: tp("2027-04-01T00:00:00Z"),
		Name: "Zero rated", Type: TariffTypeFixed,
		UnitRate: 0.2089, DailyStandingCharge: 0.53, VATRate: 0,
	}).PlausibilityWarnings(now)

	if n := kinds(got)[WarnVATRateUnexpected]; n != 0 {
		t.Errorf("the statutory zero rate must not be flagged, got %+v", got)
	}
}

func TestPlausibilityFlagsATypoedVATRate(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	got := elec(Agreement{
		From: tp("2025-01-01T00:00:00Z"), Name: "Typo", Type: TariffTypeFixed,
		UnitRate: 0.2089, DailyStandingCharge: 0.53, VATRate: 0.5,
	}).PlausibilityWarnings(now)

	if kinds(got)[WarnVATRateUnexpected] != 1 {
		t.Errorf("want one vat_rate warning for 0.5, got %+v", got)
	}
}

// A big step between dated blocks is worth a look.
func TestPlausibilityFlagsASuddenRateJump(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	got := elec(
		Agreement{From: tp("2024-01-01T00:00:00Z"), To: tp("2025-01-01T00:00:00Z"),
			Name: "Old", Type: TariffTypeFixed, UnitRate: 0.20, DailyStandingCharge: 0.53, VATRate: 0.05},
		Agreement{From: tp("2025-01-01T00:00:00Z"),
			Name: "New", Type: TariffTypeFixed, UnitRate: 0.60, DailyStandingCharge: 0.53, VATRate: 0.05},
	).PlausibilityWarnings(now)

	if kinds(got)[WarnRateJump] != 1 {
		t.Errorf("want one jump warning for a 3x step, got %+v", got)
	}
}

// THE STATED LIMIT OF THE WHOLE IDEA, pinned as a test so nobody later assumes
// it is covered: a wrong VAT basis is a 1.05x step, which sits inside the jump
// band by design — a band tight enough to fire on 1.05x would fire on every real
// price-cap movement.
func TestPlausibilityCannotCatchAWrongVATBasis(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	got := elec(
		Agreement{From: tp("2024-01-01T00:00:00Z"), To: tp("2025-01-01T00:00:00Z"),
			Name: "Ex-VAT", Type: TariffTypeFixed, UnitRate: 0.20, DailyStandingCharge: 0.53, VATRate: 0.05},
		// The same rate, mistakenly entered inc-VAT.
		Agreement{From: tp("2025-01-01T00:00:00Z"),
			Name: "Inc-VAT by mistake", Type: TariffTypeFixed, UnitRate: 0.21, DailyStandingCharge: 0.53, VATRate: 0.05},
	).PlausibilityWarnings(now)

	if len(got) != 0 {
		t.Errorf("this is the documented NON-GOAL: a 1.05x basis error is invisible "+
			"to a band, and the test exists to record that. Got %+v", got)
	}
}

// A variable agreement carries no unit rate by design — its price is in the
// archive — so banding the zero would flag every half-hourly tariff.
func TestPlausibilityIgnoresAVariableAgreementsAbsentRate(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	got := elec(Agreement{
		From: tp("2025-01-01T00:00:00Z"), Name: "Agile", Type: TariffTypeVariable,
		ID: "E-1R-AGILE-24-10-01-C", DailyStandingCharge: 0.53, VATRate: 0.05,
	}).PlausibilityWarnings(now)

	if kinds(got)[WarnUnitRateBand] != 0 {
		t.Errorf("a variable agreement has no unit rate to band, got %+v", got)
	}
}

// Gate C: the blocks are individually fine, the document as a whole cannot price
// today.
func TestPlausibilityReportsNoCoveringAgreement(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	got := elec(Agreement{
		From: tp("2024-01-01T00:00:00Z"), To: tp("2025-01-01T00:00:00Z"),
		Name: "Long gone", Type: TariffTypeFixed,
		UnitRate: 0.2089, DailyStandingCharge: 0.53, VATRate: 0.05,
	}).PlausibilityWarnings(now)

	if kinds(got)[WarnNoCurrentAgreement] != 1 {
		t.Errorf("want a Gate C warning when nothing covers now, got %+v", got)
	}
}

// A zero clock skips Gate C rather than reporting every document as uncovered.
func TestPlausibilitySkipsGateCWithoutAClock(t *testing.T) {
	got := elec(Agreement{
		From: tp("2024-01-01T00:00:00Z"), To: tp("2025-01-01T00:00:00Z"),
		Name: "Long gone", Type: TariffTypeFixed,
		UnitRate: 0.2089, DailyStandingCharge: 0.53, VATRate: 0.05,
	}).PlausibilityWarnings(time.Time{})

	if kinds(got)[WarnNoCurrentAgreement] != 0 {
		t.Errorf("no clock means no coverage claim either way, got %+v", got)
	}
}

// Output reaches /healthz, so it must not reorder between calls.
func TestPlausibilityOutputIsDeterministic(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	doc := EnergyAgreements{Agreements: map[string][]Agreement{
		"gas": {{From: tp("2025-01-01T00:00:00Z"), Name: "G", Type: TariffTypeFixed,
			UnitRate: 99, DailyStandingCharge: 0.3, VATRate: 0.05}},
		"electricity": {{From: tp("2025-01-01T00:00:00Z"), Name: "E", Type: TariffTypeFixed,
			UnitRate: 99, DailyStandingCharge: 0.3, VATRate: 0.05}},
	}}
	first := doc.PlausibilityWarnings(now)
	for i := 0; i < 20; i++ {
		got := doc.PlausibilityWarnings(now)
		if len(got) != len(first) || got[0].Fuel != first[0].Fuel {
			t.Fatalf("output reordered between calls: %+v vs %+v", got, first)
		}
	}
	if first[0].Fuel != "electricity" {
		t.Errorf("fuels should be sorted, got %q first", first[0].Fuel)
	}
}
