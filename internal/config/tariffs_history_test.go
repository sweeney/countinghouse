package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Effective-date history.
//
// This is a CORRECTNESS FIX, not an Agile feature. Until now TariffFor ignored
// its time argument and always returned the current rate, so every historical
// window was priced at today's price. Real tariffs change rate mid-term — a
// fixed 12-month product can and does re-price partway through — so a bill for
// last winter was simply wrong, silently, by the difference between the two
// rates.
//
// Tariff codes in these tests use region "A" deliberately: nothing here depends
// on which region the service is deployed for, and a test is the wrong place to
// encode a deployment's location.
// ---------------------------------------------------------------------------

// mustTime parses an RFC3339 instant or fails the test.
func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return ts
}

// twoRateHistory is a fuel that re-priced once mid-term — the shape that was
// being mispriced. Rates are £/kWh ex-VAT.
func twoRateHistory(t *testing.T) EnergyTariffs {
	t.Helper()
	return EnergyTariffs{Tariffs: map[string]Tariff{
		"electricity": {
			Unit:    "kWh",
			VATRate: 0.05,
			Periods: []Tariff{
				{EffectiveFrom: ptrTime(mustTime(t, "2025-09-08T23:00:00Z")),
					UnitRate: 0.242348, DailyStandingCharge: 0.529443},
				{EffectiveFrom: ptrTime(mustTime(t, "2026-03-31T23:00:00Z")),
					UnitRate: 0.208948, DailyStandingCharge: 0.529443},
			},
		},
	}}
}

func ptrTime(t time.Time) *time.Time { return &t }

// The bug, stated as a test: an instant before the re-pricing must get the OLD
// rate. Previously both of these returned the current rate, overstating or
// understating every pre-change bill by the difference.
func TestTariffFor_SelectsPeriodByDate(t *testing.T) {
	et := twoRateHistory(t)

	for _, tc := range []struct {
		name     string
		when     string
		wantRate float64
	}{
		{name: "well inside the first period", when: "2025-12-01T12:00:00Z", wantRate: 0.242348},
		{name: "the instant before the change", when: "2026-03-31T22:59:59Z", wantRate: 0.242348},
		{name: "exactly at the change", when: "2026-03-31T23:00:00Z", wantRate: 0.208948},
		{name: "well inside the second period", when: "2026-06-01T12:00:00Z", wantRate: 0.208948},
		{name: "far in the future, the newest period still applies", when: "2030-01-01T00:00:00Z", wantRate: 0.208948},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := et.TariffFor(mustTime(t, tc.when))
			if !ok {
				t.Fatalf("TariffFor(%s) reported no tariff", tc.when)
			}
			if got.UnitRate != tc.wantRate {
				t.Errorf("TariffFor(%s) unit rate = %v, want %v", tc.when, got.UnitRate, tc.wantRate)
			}
		})
	}
}

// An instant before the earliest period we know about has NO known price. It
// must report that rather than falling back to the nearest rate: pricing 2019
// at a 2026 rate produces a confident, wrong number, and a wrong number that
// looks right is the failure this repo keeps refusing to ship.
func TestTariffFor_BeforeEarliestPeriodIsUnknown(t *testing.T) {
	et := twoRateHistory(t)

	if got, ok := et.TariffFor(mustTime(t, "2024-01-01T00:00:00Z")); ok {
		t.Errorf("TariffFor before all history returned %+v; want not-ok so the caller must surface it", got)
	}
	// The boundary itself is covered — effective_from is inclusive.
	if _, ok := et.TariffFor(mustTime(t, "2025-09-08T23:00:00Z")); !ok {
		t.Error("the earliest effective_from should itself be covered")
	}
	// One nanosecond earlier is not.
	if _, ok := et.TariffFor(mustTime(t, "2025-09-08T22:59:59Z")); ok {
		t.Error("an instant before the earliest effective_from must not be covered")
	}
}

// Config is hand-authored, so the periods may arrive in any order. Selection
// must not depend on the order somebody happened to type them in.
func TestTariffFor_ToleratesUnsortedPeriods(t *testing.T) {
	et := EnergyTariffs{Tariffs: map[string]Tariff{
		"electricity": {
			Unit: "kWh", VATRate: 0.05,
			Periods: []Tariff{
				// Newest first, i.e. the opposite of the natural reading order.
				{EffectiveFrom: ptrTime(mustTime(t, "2026-03-31T23:00:00Z")), UnitRate: 0.208948, DailyStandingCharge: 0.5},
				{EffectiveFrom: ptrTime(mustTime(t, "2025-09-08T23:00:00Z")), UnitRate: 0.242348, DailyStandingCharge: 0.5},
			},
		},
	}}

	got, ok := et.TariffFor(mustTime(t, "2025-12-01T00:00:00Z"))
	if !ok {
		t.Fatal("no tariff found")
	}
	if got.UnitRate != 0.242348 {
		t.Errorf("unit rate = %v, want 0.242348 — selection must not depend on authoring order", got.UnitRate)
	}
}

// A period inherits the fuel's unit and VAT rate, so the common case stays terse.
// But VAT can be overridden per period, because VAT rates do change and a
// historical bill must use the rate that applied on the day.
func TestTariffFor_PeriodInheritsUnitAndVAT(t *testing.T) {
	et := EnergyTariffs{Tariffs: map[string]Tariff{
		"electricity": {
			Unit: "kWh", VATRate: 0.05,
			Periods: []Tariff{
				{EffectiveFrom: ptrTime(mustTime(t, "2025-01-01T00:00:00Z")), UnitRate: 0.20, DailyStandingCharge: 0.5},
				// A period with its own VAT rate overrides the fuel's.
				{EffectiveFrom: ptrTime(mustTime(t, "2026-01-01T00:00:00Z")), UnitRate: 0.21, DailyStandingCharge: 0.5, VATRate: 0.20},
			},
		},
	}}

	inherited, _ := et.TariffFor(mustTime(t, "2025-06-01T00:00:00Z"))
	if inherited.VATRate != 0.05 {
		t.Errorf("inherited VAT = %v, want 0.05", inherited.VATRate)
	}
	if inherited.Unit != "kWh" {
		t.Errorf("inherited unit = %q, want kWh", inherited.Unit)
	}
	if got, want := inherited.Multiplier(), 1.05; got != want {
		t.Errorf("multiplier = %v, want %v", got, want)
	}

	overridden, _ := et.TariffFor(mustTime(t, "2026-06-01T00:00:00Z"))
	if overridden.VATRate != 0.20 {
		t.Errorf("overridden VAT = %v, want 0.20", overridden.VATRate)
	}
	if overridden.Unit != "kWh" {
		t.Errorf("unit should still be inherited, got %q", overridden.Unit)
	}
}

// A half-hourly period carries a tariff code instead of a unit rate: the price
// of a kWh varies within the period, so it comes from the price archive. The
// standing charge is still flat per day and still lives in config.
func TestTariffFor_HalfHourlyPeriod(t *testing.T) {
	et := EnergyTariffs{Tariffs: map[string]Tariff{
		"electricity": {
			Unit: "kWh", VATRate: 0.05,
			Periods: []Tariff{
				{EffectiveFrom: ptrTime(mustTime(t, "2025-09-08T23:00:00Z")), UnitRate: 0.208948, DailyStandingCharge: 0.529443},
				{EffectiveFrom: ptrTime(mustTime(t, "2026-09-09T23:00:00Z")), TariffCode: "E-1R-AGILE-24-10-01-A", DailyStandingCharge: 0.591606},
			},
		},
	}}

	flat, ok := et.TariffFor(mustTime(t, "2026-01-01T00:00:00Z"))
	if !ok {
		t.Fatal("no tariff")
	}
	if flat.IsHalfHourly() {
		t.Error("a period with a unit rate must not report as half-hourly")
	}

	agile, ok := et.TariffFor(mustTime(t, "2026-10-01T00:00:00Z"))
	if !ok {
		t.Fatal("no tariff")
	}
	if !agile.IsHalfHourly() {
		t.Error("a period with a tariff code must report as half-hourly")
	}
	if agile.TariffCode != "E-1R-AGILE-24-10-01-A" {
		t.Errorf("tariff code = %q", agile.TariffCode)
	}
	// The standing charge is per-day on Agile too, and differs between products.
	if agile.DailyStandingCharge != 0.591606 {
		t.Errorf("standing charge = %v, want 0.591606", agile.DailyStandingCharge)
	}
	// UnitRate must be zero, not stale: a non-zero value here would be silently
	// used by any caller that forgot to check IsHalfHourly.
	if agile.UnitRate != 0 {
		t.Errorf("half-hourly period unit rate = %v, want 0", agile.UnitRate)
	}
}

// ---------------------------------------------------------------------------
// Validation. A tariff document is money; an ambiguous one must not boot.
// ---------------------------------------------------------------------------

func TestEnergyTariffsValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tariffs EnergyTariffs
		wantErr string
	}{
		{
			name: "the legacy single-rate shape stays valid",
			tariffs: EnergyTariffs{Tariffs: map[string]Tariff{
				"electricity": {UnitRate: 0.2089, DailyStandingCharge: 0.5294, Unit: "kWh", VATRate: 0.05},
			}},
		},
		{
			name:    "a well-formed history is valid",
			tariffs: twoRateHistory(t),
		},
		{
			name: "a period with no effective_from is ambiguous",
			tariffs: EnergyTariffs{Tariffs: map[string]Tariff{
				"electricity": {Unit: "kWh", VATRate: 0.05, Periods: []Tariff{
					{UnitRate: 0.20, DailyStandingCharge: 0.5},
				}},
			}},
			wantErr: "effective_from",
		},
		{
			name: "two periods starting at the same instant is ambiguous",
			tariffs: EnergyTariffs{Tariffs: map[string]Tariff{
				"electricity": {Unit: "kWh", VATRate: 0.05, Periods: []Tariff{
					{EffectiveFrom: ptrTime(mustTime(t, "2026-01-01T00:00:00Z")), UnitRate: 0.20, DailyStandingCharge: 0.5},
					{EffectiveFrom: ptrTime(mustTime(t, "2026-01-01T00:00:00Z")), UnitRate: 0.21, DailyStandingCharge: 0.5},
				}},
			}},
			wantErr: "duplicate",
		},
		{
			name: "a period with both a unit rate and a tariff code is ambiguous",
			tariffs: EnergyTariffs{Tariffs: map[string]Tariff{
				"electricity": {Unit: "kWh", VATRate: 0.05, Periods: []Tariff{
					{EffectiveFrom: ptrTime(mustTime(t, "2026-01-01T00:00:00Z")),
						UnitRate: 0.20, TariffCode: "E-1R-AGILE-24-10-01-A", DailyStandingCharge: 0.5},
				}},
			}},
			wantErr: "both",
		},
		{
			name: "a period with neither prices nothing",
			tariffs: EnergyTariffs{Tariffs: map[string]Tariff{
				"electricity": {Unit: "kWh", VATRate: 0.05, Periods: []Tariff{
					{EffectiveFrom: ptrTime(mustTime(t, "2026-01-01T00:00:00Z")), DailyStandingCharge: 0.5},
				}},
			}},
			wantErr: "neither",
		},
		{
			name: "an unparseable tariff code is refused at load, not at fetch time",
			tariffs: EnergyTariffs{Tariffs: map[string]Tariff{
				"electricity": {Unit: "kWh", VATRate: 0.05, Periods: []Tariff{
					{EffectiveFrom: ptrTime(mustTime(t, "2026-01-01T00:00:00Z")),
						TariffCode: "not-a-tariff", DailyStandingCharge: 0.5},
				}},
			}},
			wantErr: "tariff_code",
		},
		{
			name: "a negative VAT rate is refused",
			tariffs: EnergyTariffs{Tariffs: map[string]Tariff{
				"electricity": {UnitRate: 0.2, DailyStandingCharge: 0.5, Unit: "kWh", VATRate: -0.05},
			}},
			wantErr: "vat_rate",
		},
		{
			// Gas is not billed but must not make the document invalid.
			name: "other fuels are validated too but do not need to be billable",
			tariffs: EnergyTariffs{Tariffs: map[string]Tariff{
				"electricity": {UnitRate: 0.2, DailyStandingCharge: 0.5, Unit: "kWh", VATRate: 0.05},
				"gas":         {UnitRate: 0.05, DailyStandingCharge: 0.3, Unit: "kWh", VATRate: 0.05},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.tariffs.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Window splitting — what the cost layer needs to bill across a rate change.
// ---------------------------------------------------------------------------

// A window that spans a rate change must be billable as two segments, each with
// its own rate and its own VAT multiplier. This is the machinery the first Agile
// bill needs, because it spans the switchover from the previous tariff.
func TestPeriodsBetween(t *testing.T) {
	et := twoRateHistory(t)
	change := mustTime(t, "2026-03-31T23:00:00Z")

	for _, tc := range []struct {
		name       string
		from, to   string
		wantLen    int
		wantRates  []float64
		wantBounds [][2]string // expected [start, end) of each segment
	}{
		{
			name: "entirely within one period yields one segment covering the window",
			from: "2026-01-01T00:00:00Z", to: "2026-02-01T00:00:00Z",
			wantLen: 1, wantRates: []float64{0.242348},
			wantBounds: [][2]string{{"2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z"}},
		},
		{
			name: "spanning the change splits at the boundary",
			from: "2026-03-01T00:00:00Z", to: "2026-05-01T00:00:00Z",
			wantLen: 2, wantRates: []float64{0.242348, 0.208948},
			wantBounds: [][2]string{
				{"2026-03-01T00:00:00Z", "2026-03-31T23:00:00Z"},
				{"2026-03-31T23:00:00Z", "2026-05-01T00:00:00Z"},
			},
		},
		{
			name: "a window starting exactly at the boundary is one segment",
			from: "2026-03-31T23:00:00Z", to: "2026-05-01T00:00:00Z",
			wantLen: 1, wantRates: []float64{0.208948},
			wantBounds: [][2]string{{"2026-03-31T23:00:00Z", "2026-05-01T00:00:00Z"}},
		},
		{
			name: "a window ending exactly at the boundary is one segment of the OLD rate",
			from: "2026-03-01T00:00:00Z", to: "2026-03-31T23:00:00Z",
			wantLen: 1, wantRates: []float64{0.242348},
			wantBounds: [][2]string{{"2026-03-01T00:00:00Z", "2026-03-31T23:00:00Z"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			segs, err := et.PeriodsBetween(mustTime(t, tc.from), mustTime(t, tc.to))
			if err != nil {
				t.Fatalf("PeriodsBetween: %v", err)
			}
			if len(segs) != tc.wantLen {
				t.Fatalf("got %d segments, want %d: %+v", len(segs), tc.wantLen, segs)
			}
			var covered time.Duration
			for i, s := range segs {
				if s.Tariff.UnitRate != tc.wantRates[i] {
					t.Errorf("segment %d rate = %v, want %v", i, s.Tariff.UnitRate, tc.wantRates[i])
				}
				wantStart, wantEnd := mustTime(t, tc.wantBounds[i][0]), mustTime(t, tc.wantBounds[i][1])
				if !s.Start.Equal(wantStart) || !s.Stop.Equal(wantEnd) {
					t.Errorf("segment %d = [%s, %s), want [%s, %s)", i, s.Start, s.Stop, wantStart, wantEnd)
				}
				covered += s.Stop.Sub(s.Start)
			}
			// The segments must tile the window exactly — no gap, no overlap.
			// Otherwise the standing charge silently under- or over-bills.
			if want := mustTime(t, tc.to).Sub(mustTime(t, tc.from)); covered != want {
				t.Errorf("segments cover %v, want the whole window %v", covered, want)
			}
			_ = change
		})
	}
}

// A window reaching back before the earliest known tariff cannot be priced in
// full. It must say so rather than quietly billing the uncovered head at the
// oldest rate it happens to have.
func TestPeriodsBetween_UncoveredHeadIsAnError(t *testing.T) {
	et := twoRateHistory(t)
	_, err := et.PeriodsBetween(mustTime(t, "2024-01-01T00:00:00Z"), mustTime(t, "2026-01-01T00:00:00Z"))
	if err == nil {
		t.Fatal("a window predating all known tariffs should not price silently")
	}
	if !strings.Contains(err.Error(), "no tariff") {
		t.Errorf("error %q should explain that part of the window has no known tariff", err)
	}
}

// With no history configured at all, the single rate covers any window — the
// backwards-compatible path every existing deployment is on.
func TestPeriodsBetween_NoHistoryCoversEverything(t *testing.T) {
	et := EnergyTariffs{Tariffs: map[string]Tariff{
		"electricity": {UnitRate: 0.2089, DailyStandingCharge: 0.5294, Unit: "kWh", VATRate: 0.05},
	}}
	segs, err := et.PeriodsBetween(mustTime(t, "2019-01-01T00:00:00Z"), mustTime(t, "2030-01-01T00:00:00Z"))
	if err != nil {
		t.Fatalf("PeriodsBetween: %v", err)
	}
	if len(segs) != 1 {
		t.Fatalf("got %d segments, want 1", len(segs))
	}
	if segs[0].Tariff.UnitRate != 0.2089 {
		t.Errorf("rate = %v, want 0.2089", segs[0].Tariff.UnitRate)
	}
}

func TestPeriodsBetween_RejectsInvertedWindow(t *testing.T) {
	et := twoRateHistory(t)
	_, err := et.PeriodsBetween(mustTime(t, "2026-05-01T00:00:00Z"), mustTime(t, "2026-01-01T00:00:00Z"))
	if err == nil {
		t.Fatal("want an error when stop precedes start")
	}
}

// ---------------------------------------------------------------------------
// Wire format
// ---------------------------------------------------------------------------

// The document is authored by hand in the remote config service, so the JSON
// shape is part of the contract. This pins it, including that the legacy
// single-rate fields still parse.
func TestEnergyTariffsJSONRoundTrip(t *testing.T) {
	doc := []byte(`{
	  "tariffs": {
	    "electricity": {
	      "unit": "kWh",
	      "vat_rate": 0.05,
	      "periods": [
	        {"effective_from": "2025-09-08T23:00:00Z", "unit_rate": 0.242348, "daily_standing_charge": 0.529443},
	        {"effective_from": "2026-03-31T23:00:00Z", "unit_rate": 0.208948, "daily_standing_charge": 0.529443},
	        {"effective_from": "2026-09-09T23:00:00Z", "tariff_code": "E-1R-AGILE-24-10-01-A", "daily_standing_charge": 0.591606}
	      ]
	    },
	    "gas": {"unit_rate": 0.0502, "daily_standing_charge": 0.317, "unit": "kWh", "vat_rate": 0.05}
	  }
	}`)

	var et EnergyTariffs
	if err := json.Unmarshal(doc, &et); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := et.Validate(); err != nil {
		t.Fatalf("a realistic document should validate: %v", err)
	}

	elec, ok := et.Tariffs["electricity"]
	if !ok {
		t.Fatal("no electricity tariff")
	}
	if len(elec.Periods) != 3 {
		t.Fatalf("got %d periods, want 3", len(elec.Periods))
	}

	// Spot-check selection across all three periods.
	for _, tc := range []struct {
		when       string
		rate       float64
		halfHourly bool
		standing   float64
	}{
		{when: "2025-10-01T00:00:00Z", rate: 0.242348, standing: 0.529443},
		{when: "2026-05-01T00:00:00Z", rate: 0.208948, standing: 0.529443},
		{when: "2026-10-01T00:00:00Z", rate: 0, halfHourly: true, standing: 0.591606},
	} {
		got, ok := et.TariffFor(mustTime(t, tc.when))
		if !ok {
			t.Fatalf("no tariff at %s", tc.when)
		}
		if got.UnitRate != tc.rate {
			t.Errorf("%s unit rate = %v, want %v", tc.when, got.UnitRate, tc.rate)
		}
		if got.IsHalfHourly() != tc.halfHourly {
			t.Errorf("%s half-hourly = %v, want %v", tc.when, got.IsHalfHourly(), tc.halfHourly)
		}
		if got.DailyStandingCharge != tc.standing {
			t.Errorf("%s standing = %v, want %v", tc.when, got.DailyStandingCharge, tc.standing)
		}
		if got.VATRate != 0.05 {
			t.Errorf("%s VAT = %v, want the inherited 0.05", tc.when, got.VATRate)
		}
	}

	// Gas keeps working on the legacy shape.
	gas := et.Tariffs["gas"]
	if gas.UnitRate != 0.0502 || len(gas.Periods) != 0 {
		t.Errorf("gas = %+v, want the legacy single-rate shape preserved", gas)
	}
}

// Three periods, one window. Worth its own case: the two-period test passes
// even with off-by-one cursor logic, because the first and last segment are
// both anchored to the window's own bounds. A middle segment is anchored at
// both ends by PERIOD boundaries instead, which is where tiling actually breaks.
func TestPeriodsBetween_SpansThreePeriods(t *testing.T) {
	et := EnergyTariffs{Tariffs: map[string]Tariff{
		"electricity": {
			Unit: "kWh", VATRate: 0.05,
			Periods: []Tariff{
				{EffectiveFrom: ptrTime(mustTime(t, "2026-01-01T00:00:00Z")), UnitRate: 0.20, DailyStandingCharge: 0.5},
				{EffectiveFrom: ptrTime(mustTime(t, "2026-02-01T00:00:00Z")), UnitRate: 0.21, DailyStandingCharge: 0.5},
				{EffectiveFrom: ptrTime(mustTime(t, "2026-03-01T00:00:00Z")), TariffCode: "E-1R-AGILE-24-10-01-A", DailyStandingCharge: 0.6},
			},
		},
	}}

	from, to := mustTime(t, "2026-01-15T00:00:00Z"), mustTime(t, "2026-03-15T00:00:00Z")
	segs, err := et.PeriodsBetween(from, to)
	if err != nil {
		t.Fatalf("PeriodsBetween: %v", err)
	}
	if len(segs) != 3 {
		t.Fatalf("got %d segments, want 3: %+v", len(segs), segs)
	}

	wantBounds := [][2]string{
		{"2026-01-15T00:00:00Z", "2026-02-01T00:00:00Z"}, // clipped head
		{"2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z"}, // a whole period, both ends on boundaries
		{"2026-03-01T00:00:00Z", "2026-03-15T00:00:00Z"}, // clipped tail
	}
	for i, s := range segs {
		ws, we := mustTime(t, wantBounds[i][0]), mustTime(t, wantBounds[i][1])
		if !s.Start.Equal(ws) || !s.Stop.Equal(we) {
			t.Errorf("segment %d = [%s, %s), want [%s, %s)", i, s.Start, s.Stop, ws, we)
		}
	}
	// Contiguous and exactly covering: each segment starts where the last ended.
	for i := 1; i < len(segs); i++ {
		if !segs[i].Start.Equal(segs[i-1].Stop) {
			t.Errorf("gap or overlap between segment %d and %d: %s vs %s",
				i-1, i, segs[i-1].Stop, segs[i].Start)
		}
	}
	if !segs[0].Start.Equal(from) || !segs[len(segs)-1].Stop.Equal(to) {
		t.Errorf("segments span [%s, %s), want the window [%s, %s)",
			segs[0].Start, segs[len(segs)-1].Stop, from, to)
	}
	// The last segment is the half-hourly one: its rate comes from the archive.
	if !segs[2].Tariff.IsHalfHourly() {
		t.Error("the third segment should report as half-hourly")
	}
	// Standing charge differs across the switchover, which is why it is
	// per-segment rather than per-window.
	if segs[1].Tariff.DailyStandingCharge == segs[2].Tariff.DailyStandingCharge {
		t.Error("the fixture should change standing charge at the switchover")
	}
}
