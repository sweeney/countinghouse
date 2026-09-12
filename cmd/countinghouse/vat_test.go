package main

import (
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
)

// ---------------------------------------------------------------------------
// Resolving the expected VAT rate for the collector.
//
// This replaced a single number taken from the agreement in force at boot. That was
// wrong three ways, and all three ended the same way: the validation gate rejected
// 100% of slots for vat_mismatch, nothing was stored, and because the collector is
// fail-open nothing downstream looked broken — /healthz showed fetches succeeding
// while the archive quietly stopped filling.
//
//  1. No else branch: an agreement gap, or boot before the first agreement's `from`,
//     left the rate at 0 and validated every slot against an implied VAT of zero.
//  2. Frozen for the process lifetime: a VAT change adopted by SIGHUP, or the
//     agreement rolling over to the next block at midnight, was never picked up.
//  3. One rate for every collector: superseded half-hourly codes — which
//     VariableTariffCodes correctly returns — all got the CURRENT agreement's VAT.
// ---------------------------------------------------------------------------

// vatSrc is a ConfigProvider-shaped stub exposing whatever agreements it is given,
// re-read on every call so a test can change them underneath the resolver.
type vatSrc struct{ ag *config.EnergyAgreements }

func (v vatSrc) Tariffs() config.TariffSource { return *v.ag }

// at returns a UTC instant on 2026-09-13.
func at(h int) time.Time { return time.Date(2026, 9, 13, h, 0, 0, 0, time.UTC) }

// agreement builds one dated electricity agreement.
func agreement(from, to *time.Time, vat float64) config.Agreement {
	return config.Agreement{
		From: from, To: to, Name: "A", Type: config.TariffTypeVariable,
		ID: "E-1R-AGILE-24-10-01-A", Unit: "kWh", VATRate: vat,
		DailyStandingCharge: 0.5,
	}
}

func TestVATRateAtResolvesFromTheCoveringAgreement(t *testing.T) {
	early := at(0)
	boundary := at(12)
	ag := config.EnergyAgreements{Agreements: map[string][]config.Agreement{
		"electricity": {
			agreement(&early, &boundary, 0.05),
			agreement(&boundary, nil, 0.20),
		},
	}}
	resolve := vatRateAt(vatSrc{&ag})

	for _, tc := range []struct {
		name string
		at   time.Time
		want float64
		ok   bool
	}{
		{"inside the first agreement", at(6), 0.05, true},
		{"the boundary instant belongs to the LATER agreement", at(12), 0.20, true},
		{"inside the second agreement", at(18), 0.20, true},
		// Before any agreement: no opinion. Returning 0 here is the failure mode that
		// rejected entire backfills, because 0 is a legal rate and reads as an answer.
		{"before the first agreement", at(0).Add(-time.Hour), 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolve(tc.at)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Errorf("rate = %v, want %v", got, tc.want)
			}
		})
	}
}

// The resolver reads the LIVE agreements, so a document swapped in by SIGHUP changes
// the answer without a restart. Capturing the rate at construction is the bug.
func TestVATRateAtFollowsAConfigChange(t *testing.T) {
	from := at(0)
	ag := config.EnergyAgreements{Agreements: map[string][]config.Agreement{
		"electricity": {agreement(&from, nil, 0.05)},
	}}
	resolve := vatRateAt(vatSrc{&ag})

	if got, ok := resolve(at(6)); !ok || got != 0.05 {
		t.Fatalf("before the change: %v %v, want 0.05 true", got, ok)
	}

	// The remote document is refreshed — VAT is now 20%.
	ag = config.EnergyAgreements{Agreements: map[string][]config.Agreement{
		"electricity": {agreement(&from, nil, 0.20)},
	}}

	got, ok := resolve(at(6))
	if !ok {
		t.Fatal("the resolver stopped answering after a config refresh")
	}
	if got != 0.20 {
		t.Errorf("rate = %v, want 0.20 — the resolver must read the live snapshot, not "+
			"a copy taken when it was built", got)
	}
}

// A gap between agreements — a stretch where we were not a customer, which the
// agreements document is allowed to describe — resolves to no opinion rather than to
// the neighbouring rate.
func TestVATRateAtInAGap(t *testing.T) {
	a1from, a1to := at(0), at(6)
	a2from := at(18)
	ag := config.EnergyAgreements{Agreements: map[string][]config.Agreement{
		"electricity": {
			agreement(&a1from, &a1to, 0.05),
			agreement(&a2from, nil, 0.20),
		},
	}}
	resolve := vatRateAt(vatSrc{&ag})

	if _, ok := resolve(at(12)); ok {
		t.Error("a rate was reported inside an agreement gap; reaching for the nearest " +
			"agreement is exactly the guess the config layer refuses to make")
	}
	// Either side still resolves.
	if _, ok := resolve(at(3)); !ok {
		t.Error("the first agreement stopped resolving")
	}
	if _, ok := resolve(at(20)); !ok {
		t.Error("the second agreement stopped resolving")
	}
}

// No agreements at all: no opinion, and no panic. This is the state between boot and
// the first successful config fetch.
func TestVATRateAtWithNoAgreements(t *testing.T) {
	ag := config.EnergyAgreements{}
	if _, ok := vatRateAt(vatSrc{&ag})(at(6)); ok {
		t.Error("a rate was reported with no agreements configured")
	}
}

// A superseded half-hourly tariff whose VAT differed from today's must validate against
// ITS OWN rate. This is failure mode 3: VariableTariffCodes correctly returns superseded
// codes so their history keeps collecting, and giving them today's VAT rejected the
// entire backfill.
func TestVATRateAtForASupersededTariff(t *testing.T) {
	oldFrom, oldTo := at(0), at(12)
	newFrom := at(12)
	ag := config.EnergyAgreements{Agreements: map[string][]config.Agreement{
		"electricity": {
			func() config.Agreement {
				a := agreement(&oldFrom, &oldTo, 0.05)
				a.ID = "E-1R-AGILE-OLD-A"
				return a
			}(),
			agreement(&newFrom, nil, 0.20),
		},
	}}
	resolve := vatRateAt(vatSrc{&ag})

	// A slot from the superseded tariff's era gets the rate that applied THEN.
	got, ok := resolve(at(6))
	if !ok || got != 0.05 {
		t.Errorf("rate at the superseded tariff's time = %v %v, want 0.05 true — a "+
			"backfill of that tariff must be checked against the VAT of its own era", got, ok)
	}
}
