package config

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The temporary zero rate of VAT on domestic electricity in Great Britain:
// 0% for supplies from 1 October 2026 to 31 March 2027, 5% either side.
//
// A dated, announced, two-sided rate change is the case the dated-agreements
// document exists for, and the second transition — back UP to 5% on 1 April
// 2027 — is the one that gets forgotten, because by then the first one worked
// and nobody is watching.
//
// Both boundaries fall inside British Summer Time, so each is local midnight at
// 23:00 UTC the day before. A document written in UTC midnights would apply the
// new rate an hour early, to two half-hour slots nobody would ever check.
// ---------------------------------------------------------------------------

func london(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// zeroRateWindow returns the two instants the rate changes at, as the statute
// means them: local midnight.
func zeroRateWindow(t *testing.T) (starts, ends time.Time) {
	t.Helper()
	loc := london(t)
	return time.Date(2026, 10, 1, 0, 0, 0, 0, loc), time.Date(2027, 4, 1, 0, 0, 0, 0, loc)
}

// vatChangeAgreements is one Agile tariff split into three dated blocks that
// differ ONLY in VAT rate. The tariff code is identical throughout, because the
// tariff did not change — the tax did.
func vatChangeAgreements(t *testing.T) EnergyAgreements {
	t.Helper()
	starts, ends := zeroRateWindow(t)
	began := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const code = "E-1R-AGILE-24-10-01-A"

	return EnergyAgreements{Agreements: map[string][]Agreement{
		"electricity": {
			{From: &began, To: &starts, Name: "Agile", Type: TariffTypeVariable,
				ID: code, Unit: "kWh", VATRate: 0.05, DailyStandingCharge: 0.59},
			{From: &starts, To: &ends, Name: "Agile (VAT zero-rated)", Type: TariffTypeVariable,
				ID: code, Unit: "kWh", VATRate: 0, DailyStandingCharge: 0.59},
			{From: &ends, Name: "Agile", Type: TariffTypeVariable,
				ID: code, Unit: "kWh", VATRate: 0.05, DailyStandingCharge: 0.59},
		},
	}}
}

func TestZeroRateDocumentIsValid(t *testing.T) {
	if err := vatChangeAgreements(t).Validate(); err != nil {
		t.Fatalf("three blocks differing only in VAT must be a legal document: %v", err)
	}
}

// The rate in force at an instant, checked either side of both transitions and
// AT each boundary — half-open [from, to), so the boundary instant belongs to
// the later block.
func TestVATRateResolvesAtEachTransition(t *testing.T) {
	ag := vatChangeAgreements(t)
	loc := london(t)

	for _, tc := range []struct {
		when time.Time
		want float64
		why  string
	}{
		{time.Date(2026, 9, 30, 23, 30, 0, 0, loc), 0.05, "last half hour before the zero rate"},
		{time.Date(2026, 10, 1, 0, 0, 0, 0, loc), 0, "the boundary instant belongs to the zero-rated block"},
		{time.Date(2026, 12, 25, 12, 0, 0, 0, loc), 0, "mid zero-rate period"},
		{time.Date(2027, 3, 31, 23, 30, 0, 0, loc), 0, "last half hour of the zero rate"},
		{time.Date(2027, 4, 1, 0, 0, 0, 0, loc), 0.05, "back to 5% — the transition everyone forgets"},
	} {
		got, ok := ag.TariffFor(tc.when)
		if !ok {
			t.Fatalf("%s: no tariff covers %s", tc.why, tc.when.Format(time.RFC3339))
		}
		if got.VATRate != tc.want {
			t.Errorf("%s (%s): VAT %v, want %v",
				tc.why, tc.when.Format(time.RFC3339), got.VATRate, tc.want)
		}
	}
}

// Both boundaries are LOCAL midnight during BST, which is 23:00 UTC the previous
// day. Asserted explicitly because writing the document in UTC midnights is the
// obvious mistake, and it would zero-rate two half hours of 30 September.
func TestZeroRateBoundariesAreLocalMidnightNotUTC(t *testing.T) {
	starts, ends := zeroRateWindow(t)

	if got, want := starts.UTC().Format(time.RFC3339), "2026-09-30T23:00:00Z"; got != want {
		t.Errorf("zero rate starts at %s UTC, want %s", got, want)
	}
	if got, want := ends.UTC().Format(time.RFC3339), "2027-03-31T23:00:00Z"; got != want {
		t.Errorf("zero rate ends at %s UTC, want %s", got, want)
	}
}

// A window spanning a transition splits into segments at exactly the boundary,
// each carrying its own VAT rate — which is what lets the standing charge be
// grossed up correctly on either side of it.
func TestWindowSpanningTheTransitionSplitsAtTheBoundary(t *testing.T) {
	ag := vatChangeAgreements(t)
	loc := london(t)
	starts, _ := zeroRateWindow(t)

	from := time.Date(2026, 9, 29, 0, 0, 0, 0, loc)
	to := time.Date(2026, 10, 3, 0, 0, 0, 0, loc)

	segs, err := ag.PeriodsBetween(from, to)
	if err != nil {
		t.Fatalf("PeriodsBetween: %v", err)
	}
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2 (either side of the VAT change)", len(segs))
	}
	if !segs[0].Stop.Equal(starts) || !segs[1].Start.Equal(starts) {
		t.Errorf("segments meet at %s / %s, want the boundary %s",
			segs[0].Stop.Format(time.RFC3339), segs[1].Start.Format(time.RFC3339),
			starts.Format(time.RFC3339))
	}
	if segs[0].Tariff.VATRate != 0.05 || segs[1].Tariff.VATRate != 0 {
		t.Errorf("VAT rates %v then %v, want 0.05 then 0",
			segs[0].Tariff.VATRate, segs[1].Tariff.VATRate)
	}
	// The tariff did not change. Only the tax did.
	if segs[0].Tariff.TariffCode != segs[1].Tariff.TariffCode {
		t.Errorf("tariff code changed across a VAT-only split: %q then %q",
			segs[0].Tariff.TariffCode, segs[1].Tariff.TariffCode)
	}
}
