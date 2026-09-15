package config

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The price archive's local config.
//
// The archive is the one thing countinghouse writes, so where it lives is local
// bootstrap config rather than remote — a service cannot fetch the location of
// the thing it needs before it can answer anything.
//
// The interesting case is the INTERACTION with the agreements document: an
// archive is only needed when some agreement is half-hourly. A flat-only
// deployment needs none, and should not be made to configure one. But a
// half-hourly agreement with no archive configured can price nothing, and that
// is the silent failure this guard exists to prevent.
// ---------------------------------------------------------------------------

func TestPricesArchiveRequiredOnlyForHalfHourlyAgreements(t *testing.T) {
	flatOnly := agreementsFor(fixedBlock(t, "2025-01-01T00:00:00Z", "", 0.20))
	withVariable := agreementsFor(
		fixedBlock(t, "2025-01-01T00:00:00Z", "2026-09-10T00:00:00Z", 0.20),
		variableBlock(t, "2026-09-10T00:00:00Z", ""),
	)

	for _, tc := range []struct {
		name       string
		archive    string
		agreements EnergyAgreements
		wantErr    string
	}{
		{
			name:    "flat-only agreements need no archive",
			archive: "", agreements: flatOnly,
		},
		{
			name:    "flat-only agreements may still have one configured",
			archive: "/var/lib/countinghouse/prices.db", agreements: flatOnly,
		},
		{
			name:    "half-hourly agreements with an archive are fine",
			archive: "/var/lib/countinghouse/prices.db", agreements: withVariable,
		},
		{
			// The guard. Without it the service starts, looks healthy, and then
			// refuses to price every window after the switchover — with the cause
			// nowhere near the symptom.
			name:    "half-hourly agreements with NO archive is refused",
			archive: "", agreements: withVariable,
			wantErr: "db_path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckArchiveRequired(tc.archive, tc.agreements)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("CheckArchiveRequired = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q should mention %q", err, tc.wantErr)
			}
			// The message must name the offending agreement, or an operator has to
			// go hunting for which one needs the archive.
			if !strings.Contains(err.Error(), "Agile") {
				t.Errorf("error %q should name the half-hourly agreement", err)
			}
		})
	}
}

// The collector needs one instance per distinct half-hourly tariff, because a
// PAST half-hourly agreement's prices are still needed to bill its window. So
// this returns every distinct code, not just the current one.
func TestVariableTariffCodes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		agreements EnergyAgreements
		want       []string
	}{
		{
			name:       "no half-hourly agreements",
			agreements: agreementsFor(fixedBlock(t, "2025-01-01T00:00:00Z", "", 0.20)),
			want:       nil,
		},
		{
			name: "one half-hourly agreement",
			agreements: agreementsFor(
				fixedBlock(t, "2025-01-01T00:00:00Z", "2026-09-10T00:00:00Z", 0.20),
				variableBlock(t, "2026-09-10T00:00:00Z", ""),
			),
			want: []string{"E-1R-AGILE-24-10-01-A"},
		},
		{
			// Two successive half-hourly agreements on the same product: one code.
			name: "the same code twice is deduplicated",
			agreements: agreementsFor(
				variableBlock(t, "2025-01-01T00:00:00Z", "2026-01-01T00:00:00Z"),
				variableBlock(t, "2026-01-01T00:00:00Z", ""),
			),
			want: []string{"E-1R-AGILE-24-10-01-A"},
		},
		{
			// A PAST half-hourly agreement still needs its prices archived, or its
			// window becomes unbillable. So both codes come back, not just the
			// current one.
			name: "a superseded half-hourly tariff is still returned",
			agreements: func() EnergyAgreements {
				old := variableBlock(t, "2025-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
				old.ID = "E-1R-AGILE-18-02-21-A"
				return agreementsFor(old, variableBlock(t, "2026-01-01T00:00:00Z", ""))
			}(),
			want: []string{"E-1R-AGILE-18-02-21-A", "E-1R-AGILE-24-10-01-A"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.agreements.VariableTariffCodes()
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q (sorted for determinism)", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Gas is not billed, so a half-hourly GAS agreement must not conscript the
// electricity collector. Worth pinning: the document validates all fuels, and it
// would be easy to sweep them all up here by accident.
func TestVariableTariffCodesIgnoresOtherFuels(t *testing.T) {
	ea := EnergyAgreements{Agreements: map[string][]Agreement{
		"electricity": {fixedBlock(t, "2025-01-01T00:00:00Z", "", 0.20)},
		"gas":         {variableBlock(t, "2025-01-01T00:00:00Z", "")},
	}}
	if got := ea.VariableTariffCodes(); len(got) != 0 {
		t.Errorf("got %v, want none — countinghouse bills electricity only", got)
	}
}
