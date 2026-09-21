package httpapi

import (
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
)

// ---------------------------------------------------------------------------
// tariff_codes must name every tariff the window touches — including FIXED ones.
//
// Review finding on #41. tariffCodesFor read Tariff.TariffCode, which
// Agreement.resolve() deliberately sets only for VARIABLE agreements, because
// its presence is what marks a resolved tariff half-hourly (IsHalfHourly is
// literally TariffCode != ""). So a fixed agreement contributed nothing, and the
// field was doing double duty: a marker in the cost layer, a label here, with
// the two disagreeing about what emptiness means.
//
// Costs were never affected. The damage was to the label, and it was worst in
// exactly the case the array was made plural for.
// ---------------------------------------------------------------------------

func tcAt(s string) *time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return &t
}

// tcServer serves a window against the given dated agreements.
func tcServer(t *testing.T, blocks ...config.Agreement) *Server {
	t.Helper()
	s, _ := dataSetup(t)
	s.Config = pxConfig{agreements: config.EnergyAgreements{
		Agreements: map[string][]config.Agreement{"electricity": blocks},
	}}
	return s
}

var (
	tcFixed = config.Agreement{
		From: tcAt("2026-01-01T00:00:00Z"), To: tcAt("2026-09-10T00:00:00Z"),
		Name: "Octopus Fixed 12M", ID: "E-1R-OE-FIX-12M-25-09-09-N",
		Type: config.TariffTypeFixed, VATRate: 0.05,
		UnitRate: 0.208948, DailyStandingCharge: 0.53,
	}
	tcAgile = config.Agreement{
		From: tcAt("2026-09-10T00:00:00Z"),
		Name: "Agile Octopus", ID: "E-1R-AGILE-24-10-01-N",
		Type: config.TariffTypeVariable, VATRate: 0.05, DailyStandingCharge: 0.53,
	}
)

// The reported case: a window wholly inside a fixed agreement named no tariff at
// all, so the field vanished under omitempty — indistinguishable from a server
// predating the feature. Silence that reads as data.
func TestTariffCodes_NamesAFixedAgreement(t *testing.T) {
	s := tcServer(t, tcFixed, tcAgile)
	got := s.tariffCodesFor(
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC))

	if len(got) != 1 || got[0] != "E-1R-OE-FIX-12M-25-09-09-N" {
		t.Errorf("tariff_codes = %v, want the fixed agreement's id", got)
	}
}

// The regression the reviewer asked to pin, and the reason the array is plural:
// a consumer testing len(tariff_codes) == 1 to decide "one tariff, so I may
// treat this as one curve" got YES for a window spanning two.
func TestTariffCodes_SpanningAFixedToVariableSwitchoverNamesBoth(t *testing.T) {
	s := tcServer(t, tcFixed, tcAgile)
	got := s.tariffCodesFor(
		time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC))

	if len(got) != 2 {
		t.Fatalf("tariff_codes = %v, want both sides of the switchover", got)
	}
	if got[0] != "E-1R-OE-FIX-12M-25-09-09-N" || got[1] != "E-1R-AGILE-24-10-01-N" {
		t.Errorf("tariff_codes = %v, want [fixed, agile] in window order", got)
	}
}

// Unchanged behaviour for a half-hourly window.
func TestTariffCodes_VariableOnlyIsUnchanged(t *testing.T) {
	s := tcServer(t, tcFixed, tcAgile)
	got := s.tariffCodesFor(
		time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC))

	if len(got) != 1 || got[0] != "E-1R-AGILE-24-10-01-N" {
		t.Errorf("tariff_codes = %v, want just the agile code", got)
	}
}

// A VAT-only split is TWO agreement blocks describing ONE tariff — the zero-rate
// runbook case. Curve identity, not block count, is what makes a tariff change,
// so these must still collapse to one entry.
func TestTariffCodes_DeduplicatesAVATOnlySplit(t *testing.T) {
	before := config.Agreement{
		From: tcAt("2026-01-01T00:00:00Z"), To: tcAt("2026-10-01T00:00:00Z"),
		Name: "Octopus Fixed 12M", ID: "E-1R-OE-FIX-12M-25-09-09-N",
		Type: config.TariffTypeFixed, VATRate: 0.05,
		UnitRate: 0.208948, DailyStandingCharge: 0.53,
	}
	zeroRated := before
	zeroRated.From = tcAt("2026-10-01T00:00:00Z")
	zeroRated.To = tcAt("2027-04-01T00:00:00Z")
	zeroRated.VATRate = 0

	s := tcServer(t, before, zeroRated)
	got := s.tariffCodesFor(
		time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))

	if len(got) != 1 {
		t.Errorf("tariff_codes = %v, want one: a VAT split is one tariff described twice", got)
	}
}

// A fixed block may legally carry no id (the Agreement.ID doc comment says so),
// and then its name is the only identifier it has.
func TestTariffCodes_FallsBackToTheAgreementName(t *testing.T) {
	noID := tcFixed
	noID.ID = ""
	s := tcServer(t, noID)
	got := s.tariffCodesFor(
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC))

	if len(got) != 1 || got[0] != "Octopus Fixed 12M" {
		t.Errorf("tariff_codes = %v, want the agreement's name as the fallback label", got)
	}
}
