package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// energy_agreements: the dated-block tariff document.
//
// This supersedes the `periods` shape. The difference that matters is that a
// block carries an explicit `to` as well as a `from`, which makes two states
// expressible that the open-ended form could not represent at all:
//
//   - a GAP, where no agreement covers an instant. Legitimate — you were not a
//     customer then — and so valid in the document, but refused at pricing time.
//   - an OVERLAP, where two agreements cover one instant. Never legitimate: two
//     prices for one kWh is ambiguous, so the document is refused.
//
// Blocks also carry identity (name, type, id) rather than being anonymous rate
// rows, and a `variable` block deliberately carries NO unit rate — its price
// comes from the half-hourly archive, and a consumer is meant to ask the price
// API rather than find a misleading number here.
//
// Tariff ids use region "A"; nothing here depends on a deployment's region.
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

func ptrTime(t time.Time) *time.Time { return &t }

// realisticDoc is the shape as it would actually be authored: a fixed term that
// ended, followed by an open-ended variable one.
const realisticDoc = `{
  "agreements": {
    "electricity": [
      {
        "from": "2025-09-23T00:00:00+01:00",
        "to":   "2026-09-10T00:00:00+01:00",
        "name": "Octopus 12M Fixed",
        "type": "fixed",
        "id":   "E-1R-OE-FIX-12M-25-09-09-A",
        "unit": "kWh",
        "vat_rate": 0.05,
        "unit_rate": 0.208948,
        "daily_standing_charge": 0.529443
      },
      {
        "from": "2026-09-10T00:00:00+01:00",
        "name": "Agile Octopus",
        "type": "variable",
        "id":   "E-1R-AGILE-24-10-01-A",
        "unit": "kWh",
        "vat_rate": 0.05,
        "daily_standing_charge": 0.591606
      }
    ],
    "gas": [
      {
        "from": "2026-09-23T00:00:00+01:00",
        "name": "Octopus 18M Fixed",
        "type": "fixed",
        "id":   "G-1R-OE-FIX-18M-26-09-08-A",
        "unit": "kWh",
        "vat_rate": 0.05,
        "unit_rate": 0.0502,
        "daily_standing_charge": 0.317
      }
    ]
  }
}`

func parseDoc(t *testing.T, doc string) EnergyAgreements {
	t.Helper()
	var ea EnergyAgreements
	if err := json.Unmarshal([]byte(doc), &ea); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return ea
}

// agreementsFor builds a document from electricity blocks, for the table tests.
func agreementsFor(blocks ...Agreement) EnergyAgreements {
	return EnergyAgreements{Agreements: map[string][]Agreement{"electricity": blocks}}
}

// fixedBlock and variableBlock keep the table cases readable.
func fixedBlock(t *testing.T, from, to string, rate float64) Agreement {
	t.Helper()
	a := Agreement{
		From: ptrTime(mustTime(t, from)), Name: "Fixed", Type: TariffTypeFixed,
		ID: "E-1R-OE-FIX-12M-25-09-09-A", Unit: "kWh", VATRate: 0.05,
		UnitRate: rate, DailyStandingCharge: 0.5,
	}
	if to != "" {
		a.To = ptrTime(mustTime(t, to))
	}
	return a
}

func variableBlock(t *testing.T, from, to string) Agreement {
	t.Helper()
	a := Agreement{
		From: ptrTime(mustTime(t, from)), Name: "Agile", Type: TariffTypeVariable,
		ID: "E-1R-AGILE-24-10-01-A", Unit: "kWh", VATRate: 0.05,
		DailyStandingCharge: 0.591606,
	}
	if to != "" {
		a.To = ptrTime(mustTime(t, to))
	}
	return a
}

// ---------------------------------------------------------------------------
// Wire format
// ---------------------------------------------------------------------------

func TestEnergyAgreementsParsesRealisticDocument(t *testing.T) {
	ea := parseDoc(t, realisticDoc)
	if err := ea.Validate(); err != nil {
		t.Fatalf("a realistic document must validate: %v", err)
	}

	elec := ea.Agreements["electricity"]
	if len(elec) != 2 {
		t.Fatalf("got %d electricity agreements, want 2", len(elec))
	}

	// The fixed term: bounded, and carries a rate.
	if elec[0].Type != TariffTypeFixed {
		t.Errorf("first block type = %q", elec[0].Type)
	}
	if elec[0].To == nil {
		t.Error("a term that ended should carry a `to`")
	}
	if elec[0].UnitRate != 0.208948 {
		t.Errorf("fixed unit rate = %v", elec[0].UnitRate)
	}

	// The variable term: open-ended, and deliberately rateless.
	if elec[1].Type != TariffTypeVariable {
		t.Errorf("second block type = %q", elec[1].Type)
	}
	if elec[1].To != nil {
		t.Error("the current agreement should be open-ended")
	}
	if elec[1].UnitRate != 0 {
		t.Errorf("a variable block carries unit rate %v; it must carry none, "+
			"or a consumer will bill energy at a number that is not the price", elec[1].UnitRate)
	}
	if elec[1].Name == "" || elec[1].ID == "" {
		t.Error("identity (name, id) must survive parsing")
	}

	// Gas parses but is not billed; the document is not the place to decide that.
	if len(ea.Agreements["gas"]) != 1 {
		t.Error("gas should parse")
	}
}

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

func TestAgreementsTariffForSelectsByDate(t *testing.T) {
	ea := parseDoc(t, realisticDoc)
	switchover := "2026-09-09T23:00:00Z" // 2026-09-10T00:00+01:00

	for _, tc := range []struct {
		name       string
		when       string
		wantOK     bool
		wantRate   float64
		halfHourly bool
		wantName   string
	}{
		{
			name: "inside the fixed term", when: "2026-01-01T00:00:00Z",
			wantOK: true, wantRate: 0.208948, wantName: "Octopus 12M Fixed",
		},
		{
			name: "the instant before the switchover is still fixed", when: "2026-09-09T22:59:59Z",
			wantOK: true, wantRate: 0.208948, wantName: "Octopus 12M Fixed",
		},
		{
			// `from` is inclusive and `to` exclusive, so the boundary instant
			// belongs to the NEW agreement. Getting this backwards would bill one
			// half hour a year on the wrong tariff — small, and permanently
			// irreproducible.
			name: "exactly at the switchover is variable", when: switchover,
			wantOK: true, halfHourly: true, wantName: "Agile Octopus",
		},
		{
			name: "well inside the variable term", when: "2026-12-01T00:00:00Z",
			wantOK: true, halfHourly: true, wantName: "Agile Octopus",
		},
		{
			// Open-ended, so it still applies years out.
			name: "far future, the open-ended agreement still applies", when: "2030-01-01T00:00:00Z",
			wantOK: true, halfHourly: true, wantName: "Agile Octopus",
		},
		{
			name: "before the earliest agreement is unknown", when: "2024-01-01T00:00:00Z",
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ea.TariffFor(mustTime(t, tc.when))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.UnitRate != tc.wantRate {
				t.Errorf("unit rate = %v, want %v", got.UnitRate, tc.wantRate)
			}
			if got.IsHalfHourly() != tc.halfHourly {
				t.Errorf("IsHalfHourly = %v, want %v", got.IsHalfHourly(), tc.halfHourly)
			}
			if got.Name != tc.wantName {
				t.Errorf("name = %q, want %q", got.Name, tc.wantName)
			}
			// VAT and unit are carried on every block, so a resolved tariff is
			// always complete without reaching back to a parent.
			if got.VATRate != 0.05 || got.Unit != "kWh" {
				t.Errorf("vat/unit = %v/%q, want 0.05/kWh", got.VATRate, got.Unit)
			}
		})
	}
}

// A gap is the state the old open-ended shape could not express: no agreement
// covers the instant, because we were not a customer. It must report unknown
// rather than stretching a neighbouring agreement over it.
func TestAgreementsTariffForGapIsUnknown(t *testing.T) {
	ea := agreementsFor(
		fixedBlock(t, "2025-01-01T00:00:00Z", "2025-06-01T00:00:00Z", 0.20),
		// Three months with no agreement at all.
		fixedBlock(t, "2025-09-01T00:00:00Z", "", 0.21),
	)
	if err := ea.Validate(); err != nil {
		t.Fatalf("a document with a gap is legitimate: %v", err)
	}

	if _, ok := ea.TariffFor(mustTime(t, "2025-07-01T00:00:00Z")); ok {
		t.Error("an instant inside a gap must not resolve to a tariff")
	}
	// The boundaries either side still resolve.
	if _, ok := ea.TariffFor(mustTime(t, "2025-05-31T23:59:59Z")); !ok {
		t.Error("the instant before the gap should resolve")
	}
	if _, ok := ea.TariffFor(mustTime(t, "2025-09-01T00:00:00Z")); !ok {
		t.Error("the instant the next agreement begins should resolve")
	}
}

// `to` is exclusive, so an agreement does not cover its own end instant.
func TestAgreementsToIsExclusive(t *testing.T) {
	end := "2026-06-01T00:00:00Z"
	ea := agreementsFor(fixedBlock(t, "2026-01-01T00:00:00Z", end, 0.20))

	if _, ok := ea.TariffFor(mustTime(t, "2026-05-31T23:59:59Z")); !ok {
		t.Error("the instant before `to` must be covered")
	}
	if _, ok := ea.TariffFor(mustTime(t, end)); ok {
		t.Error("`to` is exclusive; the end instant must not be covered")
	}
}

// Authoring order is not a contract.
func TestAgreementsToleratesUnsortedBlocks(t *testing.T) {
	ea := agreementsFor(
		variableBlock(t, "2026-09-10T00:00:00Z", ""),
		fixedBlock(t, "2025-09-23T00:00:00Z", "2026-09-10T00:00:00Z", 0.208948),
	)
	if err := ea.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got, ok := ea.TariffFor(mustTime(t, "2026-01-01T00:00:00Z"))
	if !ok {
		t.Fatal("no tariff")
	}
	if got.UnitRate != 0.208948 {
		t.Errorf("unit rate = %v; selection must not depend on authoring order", got.UnitRate)
	}
}

// A variable block resolves to something a caller can bill from: no unit rate,
// a tariff code to look prices up under, and a standing charge.
func TestAgreementsVariableResolvesWithoutARate(t *testing.T) {
	ea := agreementsFor(variableBlock(t, "2026-09-10T00:00:00Z", ""))
	got, ok := ea.TariffFor(mustTime(t, "2026-10-01T00:00:00Z"))
	if !ok {
		t.Fatal("no tariff")
	}
	if !got.IsHalfHourly() {
		t.Error("a variable agreement must report as half-hourly")
	}
	if got.UnitRate != 0 {
		t.Errorf("unit rate = %v, want 0 — a caller that ignored IsHalfHourly must "+
			"bill zero and notice, not bill a plausible wrong number", got.UnitRate)
	}
	if got.TariffCode == "" {
		t.Error("a variable agreement must carry the code its prices are archived under")
	}
	if got.DailyStandingCharge == 0 {
		t.Error("the standing charge is flat per day on a variable tariff too")
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestEnergyAgreementsValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		build   func() EnergyAgreements
		wantErr string
	}{
		{
			name:  "a well-formed document",
			build: func() EnergyAgreements { return parseDoc(t, realisticDoc) },
		},
		{
			name: "an empty document is valid but prices nothing",
			build: func() EnergyAgreements {
				return EnergyAgreements{Agreements: map[string][]Agreement{}}
			},
		},
		{
			name: "a gap is legitimate",
			build: func() EnergyAgreements {
				return agreementsFor(
					fixedBlock(t, "2025-01-01T00:00:00Z", "2025-06-01T00:00:00Z", 0.20),
					fixedBlock(t, "2025-09-01T00:00:00Z", "", 0.21),
				)
			},
		},
		{
			// Two prices for one kWh. There is no defensible way to pick, so the
			// document is refused rather than resolved by an arbitrary rule like
			// "last wins" that nobody would remember.
			name: "overlapping agreements are refused",
			build: func() EnergyAgreements {
				return agreementsFor(
					fixedBlock(t, "2025-01-01T00:00:00Z", "2025-07-01T00:00:00Z", 0.20),
					fixedBlock(t, "2025-06-01T00:00:00Z", "2025-12-01T00:00:00Z", 0.21),
				)
			},
			wantErr: "overlap",
		},
		{
			name: "an open-ended block overlapping a later one is refused",
			build: func() EnergyAgreements {
				return agreementsFor(
					fixedBlock(t, "2025-01-01T00:00:00Z", "", 0.20),
					fixedBlock(t, "2026-01-01T00:00:00Z", "", 0.21),
				)
			},
			wantErr: "overlap",
		},
		{
			name: "a missing from is refused",
			build: func() EnergyAgreements {
				a := fixedBlock(t, "2025-01-01T00:00:00Z", "", 0.20)
				a.From = nil
				return agreementsFor(a)
			},
			wantErr: "from",
		},
		{
			name: "to before from is refused",
			build: func() EnergyAgreements {
				return agreementsFor(fixedBlock(t, "2026-01-01T00:00:00Z", "2025-01-01T00:00:00Z", 0.20))
			},
			wantErr: "to",
		},
		{
			name: "a zero-length block is refused",
			build: func() EnergyAgreements {
				return agreementsFor(fixedBlock(t, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z", 0.20))
			},
			wantErr: "to",
		},
		{
			name: "an unknown type is refused",
			build: func() EnergyAgreements {
				a := fixedBlock(t, "2025-01-01T00:00:00Z", "", 0.20)
				a.Type = "tracker"
				return agreementsFor(a)
			},
			wantErr: "type",
		},
		{
			name: "a missing type is refused",
			build: func() EnergyAgreements {
				a := fixedBlock(t, "2025-01-01T00:00:00Z", "", 0.20)
				a.Type = ""
				return agreementsFor(a)
			},
			wantErr: "type",
		},
		{
			name: "fixed without a unit rate prices nothing",
			build: func() EnergyAgreements {
				a := fixedBlock(t, "2025-01-01T00:00:00Z", "", 0)
				return agreementsFor(a)
			},
			wantErr: "unit_rate",
		},
		{
			// The whole point of the type: a variable block's price is not here.
			// A rate alongside it would be read by somebody, and it would be wrong.
			name: "variable WITH a unit rate is refused",
			build: func() EnergyAgreements {
				a := variableBlock(t, "2026-09-10T00:00:00Z", "")
				a.UnitRate = 0.25
				return agreementsFor(a)
			},
			wantErr: "unit_rate",
		},
		{
			name: "variable without an id cannot be priced at all",
			build: func() EnergyAgreements {
				a := variableBlock(t, "2026-09-10T00:00:00Z", "")
				a.ID = ""
				return agreementsFor(a)
			},
			wantErr: "id",
		},
		{
			name: "variable with an unparseable id is refused at load",
			build: func() EnergyAgreements {
				a := variableBlock(t, "2026-09-10T00:00:00Z", "")
				a.ID = "not-a-tariff-code"
				return agreementsFor(a)
			},
			wantErr: "id",
		},
		{
			// An unnamed agreement would surface as its code in any UI, which
			// reads as data rather than as a missing label.
			name: "a missing name is refused",
			build: func() EnergyAgreements {
				a := fixedBlock(t, "2025-01-01T00:00:00Z", "", 0.20)
				a.Name = ""
				return agreementsFor(a)
			},
			wantErr: "name",
		},
		{
			name: "a negative vat rate is refused",
			build: func() EnergyAgreements {
				a := fixedBlock(t, "2025-01-01T00:00:00Z", "", 0.20)
				a.VATRate = -0.05
				return agreementsFor(a)
			},
			wantErr: "vat_rate",
		},
		{
			name: "a negative standing charge is refused",
			build: func() EnergyAgreements {
				a := fixedBlock(t, "2025-01-01T00:00:00Z", "", 0.20)
				a.DailyStandingCharge = -1
				return agreementsFor(a)
			},
			wantErr: "daily_standing_charge",
		},
		{
			// A fixed block needs no id — the rate is right here — but if one is
			// given it must be real, because it will be used for audit.
			name: "a fixed block may omit its id",
			build: func() EnergyAgreements {
				a := fixedBlock(t, "2025-01-01T00:00:00Z", "", 0.20)
				a.ID = ""
				return agreementsFor(a)
			},
		},
		{
			name: "a fixed block with a malformed id is refused",
			build: func() EnergyAgreements {
				a := fixedBlock(t, "2025-01-01T00:00:00Z", "", 0.20)
				a.ID = "nonsense"
				return agreementsFor(a)
			},
			wantErr: "id",
		},
		{
			// Every fuel is validated, not only the one we bill: a document is
			// either coherent or it is not.
			name: "a broken gas block invalidates the document",
			build: func() EnergyAgreements {
				ea := parseDoc(t, realisticDoc)
				g := ea.Agreements["gas"][0]
				g.Type = "nonsense"
				ea.Agreements["gas"] = []Agreement{g}
				return ea
			},
			wantErr: "type",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.build().Validate()
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
// Window splitting
// ---------------------------------------------------------------------------

func TestAgreementsPeriodsBetween(t *testing.T) {
	ea := parseDoc(t, realisticDoc)
	switchover := mustTime(t, "2026-09-09T23:00:00Z")

	t.Run("a window spanning the switchover splits into two segments", func(t *testing.T) {
		from, to := mustTime(t, "2026-09-01T00:00:00Z"), mustTime(t, "2026-10-01T00:00:00Z")
		segs, err := ea.PeriodsBetween(from, to)
		if err != nil {
			t.Fatalf("PeriodsBetween: %v", err)
		}
		if len(segs) != 2 {
			t.Fatalf("got %d segments, want 2: %+v", len(segs), segs)
		}
		if !segs[0].Stop.Equal(switchover) || !segs[1].Start.Equal(switchover) {
			t.Errorf("segments should meet at the switchover %s, got %s / %s",
				switchover, segs[0].Stop, segs[1].Start)
		}
		if segs[0].Tariff.IsHalfHourly() {
			t.Error("the first segment is the fixed term")
		}
		if !segs[1].Tariff.IsHalfHourly() {
			t.Error("the second segment is the variable term")
		}
		// Exact tiling, so the apportioned standing charge adds up.
		covered := segs[0].Stop.Sub(segs[0].Start) + segs[1].Stop.Sub(segs[1].Start)
		if covered != to.Sub(from) {
			t.Errorf("segments cover %v, want %v", covered, to.Sub(from))
		}
		// The standing charge differs across the switchover, which is why it is
		// resolved per segment.
		if segs[0].Tariff.DailyStandingCharge == segs[1].Tariff.DailyStandingCharge {
			t.Error("the fixture should change standing charge at the switchover")
		}
	})

	t.Run("a window inside one agreement is one segment covering it", func(t *testing.T) {
		from, to := mustTime(t, "2026-01-01T00:00:00Z"), mustTime(t, "2026-02-01T00:00:00Z")
		segs, err := ea.PeriodsBetween(from, to)
		if err != nil {
			t.Fatalf("PeriodsBetween: %v", err)
		}
		if len(segs) != 1 {
			t.Fatalf("got %d segments, want 1", len(segs))
		}
		if !segs[0].Start.Equal(from) || !segs[0].Stop.Equal(to) {
			t.Errorf("segment = [%s, %s), want the window", segs[0].Start, segs[0].Stop)
		}
	})
}

// A window overlapping a gap cannot be billed in full. Returning only the
// covered parts would silently under-bill, so it is an error that names the gap.
func TestAgreementsPeriodsBetweenRefusesAGap(t *testing.T) {
	ea := agreementsFor(
		fixedBlock(t, "2025-01-01T00:00:00Z", "2025-06-01T00:00:00Z", 0.20),
		fixedBlock(t, "2025-09-01T00:00:00Z", "", 0.21),
	)

	for _, tc := range []struct{ name, from, to string }{
		{name: "entirely inside the gap", from: "2025-07-01T00:00:00Z", to: "2025-08-01T00:00:00Z"},
		{name: "starting before the gap and ending inside it", from: "2025-05-01T00:00:00Z", to: "2025-07-01T00:00:00Z"},
		{name: "spanning the whole gap", from: "2025-05-01T00:00:00Z", to: "2025-10-01T00:00:00Z"},
		{name: "starting inside the gap", from: "2025-08-01T00:00:00Z", to: "2025-10-01T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ea.PeriodsBetween(mustTime(t, tc.from), mustTime(t, tc.to))
			if err == nil {
				t.Fatal("a window touching a gap must not price silently")
			}
			if !strings.Contains(err.Error(), "no agreement") {
				t.Errorf("error %q should explain that part of the window has no agreement", err)
			}
		})
	}
}

func TestAgreementsPeriodsBetweenBeforeAllAgreements(t *testing.T) {
	ea := parseDoc(t, realisticDoc)
	_, err := ea.PeriodsBetween(mustTime(t, "2024-01-01T00:00:00Z"), mustTime(t, "2026-01-01T00:00:00Z"))
	if err == nil {
		t.Fatal("a window predating every agreement must not price")
	}
}

func TestAgreementsPeriodsBetweenRejectsInvertedWindow(t *testing.T) {
	ea := parseDoc(t, realisticDoc)
	if _, err := ea.PeriodsBetween(
		mustTime(t, "2026-06-01T00:00:00Z"), mustTime(t, "2026-01-01T00:00:00Z")); err == nil {
		t.Fatal("want an error when stop precedes start")
	}
}

// ---------------------------------------------------------------------------
// The shared interface
// ---------------------------------------------------------------------------

// Both documents answer the same two questions, so handlers do not care which
// namespace is in use. This is what keeps the migration from reaching into the
// HTTP layer.
func TestBothDocumentsSatisfyTariffSource(t *testing.T) {
	var _ TariffSource = EnergyAgreements{}
	var _ TariffSource = EnergyTariffs{}

	legacy := EnergyTariffs{Tariffs: map[string]Tariff{
		"electricity": {UnitRate: 0.2089, DailyStandingCharge: 0.5294, Unit: "kWh", VATRate: 0.05},
	}}
	modern := parseDoc(t, realisticDoc)

	for _, tc := range []struct {
		name   string
		source TariffSource
		when   string
		wantOK bool
	}{
		{name: "legacy prices any instant", source: legacy, when: "2026-01-01T00:00:00Z", wantOK: true},
		{name: "agreements price a covered instant", source: modern, when: "2026-01-01T00:00:00Z", wantOK: true},
		{name: "agreements refuse an uncovered instant", source: modern, when: "2020-01-01T00:00:00Z", wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := tc.source.TariffFor(mustTime(t, tc.when))
			if ok != tc.wantOK {
				t.Errorf("TariffFor ok = %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

// The legacy single-rate document is presentable as one open-ended agreement, so
// /tariffs can serve one shape regardless of which namespace backs it.
func TestLegacyDocumentPresentsAsAnAgreement(t *testing.T) {
	legacy := EnergyTariffs{Tariffs: map[string]Tariff{
		"electricity": {UnitRate: 0.2089, DailyStandingCharge: 0.5294, Unit: "kWh", VATRate: 0.05},
	}}

	got := legacy.AsAgreements()
	elec := got.Agreements["electricity"]
	if len(elec) != 1 {
		t.Fatalf("got %d agreements, want 1", len(elec))
	}
	if elec[0].Type != TariffTypeFixed {
		t.Errorf("type = %q, want fixed", elec[0].Type)
	}
	if elec[0].To != nil {
		t.Error("a legacy rate has no end date, so the synthesised block is open-ended")
	}
	if elec[0].UnitRate != 0.2089 {
		t.Errorf("unit rate = %v", elec[0].UnitRate)
	}
	// No `from` at all: the legacy document asserts nothing about when the rate
	// started, and inventing a date would be fabrication. It also serialises as an
	// absent field rather than as a year-1 timestamp, which would read as data.
	if elec[0].From != nil {
		t.Errorf("from = %v, want absent — the legacy shape carries no start date", elec[0].From)
	}
}
