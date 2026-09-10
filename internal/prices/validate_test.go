package prices

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sweeney/countinghouse/internal/octopus"
)

// ---------------------------------------------------------------------------
// Test harness
//
// Every fixture here is a REAL recorded API response, read from
// ../octopus/testdata and served by an httptest server. Nothing reaches
// api.octopus.energy: a validation suite that depended on a supplier's uptime
// would be useless exactly when we needed it, and we could not provoke a
// partial day on demand.
//
// Going through the real octopus client rather than hand-building slots is
// deliberate. It is the only way the tests exercise the same wire parsing, the
// same UTC coercion and the same oldest-first ordering the collector will hand
// to Validate — and Gate C's contiguity answer depends on all three.
//
// Tariff codes use region "A" throughout. Nothing here depends on which region
// the service is deployed for, and a test is the wrong place to record one.
// ---------------------------------------------------------------------------

const testTariff = "E-1R-AGILE-24-10-01-A"

// london is the zone the local-day arithmetic is about. Loaded once per test so
// a missing tzdata fails loudly rather than silently testing UTC, where 46- and
// 50-slot days do not exist and Gate C would look correct while proving nothing.
func london(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("load Europe/London: %v", err)
	}
	return loc
}

func instant(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return ts
}

// fetchedAt is a fixed transaction time. Validation must not care what it is,
// and no test here may depend on the wall clock.
func fetchedAt(t *testing.T) time.Time { return instant(t, "2026-09-10T16:08:00Z") }

// fixtureSlots serves one recorded response over httptest, parses it with the
// real client, and converts it to slots with a fixed retrieved-at.
func fixtureSlots(t *testing.T, name string) []Slot {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("..", "octopus", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body) //nolint:errcheck
	}))
	t.Cleanup(ts.Close)

	code, err := octopus.ParseTariffCode(testTariff)
	if err != nil {
		t.Fatalf("ParseTariffCode(%q): %v", testTariff, err)
	}
	c, err := octopus.New(octopus.Options{BaseURL: ts.URL})
	if err != nil {
		t.Fatalf("octopus.New: %v", err)
	}
	rates, err := c.UnitRates(context.Background(), code, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("UnitRates from fixture %s: %v", name, err)
	}
	if len(rates) == 0 {
		t.Fatalf("fixture %s produced no rates", name)
	}
	return FromRates(testTariff, rates, fetchedAt(t))
}

// halfHourly is the options set the Agile collector uses: 5% VAT, the exact
// tariff we asked for, and every threshold left at its documented default.
func halfHourly() ValidateOptions {
	return ValidateOptions{TariffCode: testTariff, VATRate: 0.05}
}

// hhSlot builds one well-formed half-hourly slot at from, priced exc with VAT
// applied at vat — so a test has to go out of its way to break the relationship
// rather than breaking it by accident.
func hhSlot(t *testing.T, from string, exc, vat float64) Slot {
	t.Helper()
	start := instant(t, from)
	end := start.Add(30 * time.Minute)
	return Slot{
		TariffCode:  testTariff,
		ValidFrom:   start,
		ValidTo:     &end,
		ExcVATPence: exc,
		IncVATPence: exc * (1 + vat),
		RetrievedAt: fetchedAt(t),
	}
}

// reasons lists the reject reasons in result order, for terse assertions.
func reasons(r ValidationResult) []RejectReason {
	out := make([]RejectReason, 0, len(r.Rejected))
	for _, rej := range r.Rejected {
		out = append(out, rej.Reason)
	}
	return out
}

// warningKinds lists the warning kinds in result order.
func warningKinds(r ValidationResult) []WarningKind {
	out := make([]WarningKind, 0, len(r.Warnings))
	for _, w := range r.Warnings {
		out = append(out, w.Kind)
	}
	return out
}

// ---------------------------------------------------------------------------
// Gate A — structural. Per slot, REJECT.
// ---------------------------------------------------------------------------

// One table per structural check, each case stating what a production slot
// looks like when the check fires. Every rejected case must also prove the slot
// came back WITH its reason: a dropped slot becomes unpriced energy weeks
// later, at which point nobody can tell whether the price was missing upstream
// or the collector ate it.
func TestValidate_GateA_Structural(t *testing.T) {
	base := hhSlot(t, "2026-09-11T00:00:00Z", 20, 0.05)

	mutate := func(f func(s *Slot)) Slot {
		s := base
		// Copy the pointer's target too, so a case that edits ValidTo cannot
		// corrupt the next case's fixture.
		if base.ValidTo != nil {
			to := *base.ValidTo
			s.ValidTo = &to
		}
		f(&s)
		return s
	}
	open := mutate(func(s *Slot) { s.ValidTo = nil })

	for _, tc := range []struct {
		name string
		in   Slot
		want RejectReason // "" means the slot must be accepted
	}{
		{name: "a well-formed half-hourly slot is accepted", in: base},
		{
			name: "an open-ended slot is legal — standing charges have no end",
			in:   open,
		},
		{
			name: "exactly zero pence is a real price, not a missing one",
			in:   hhSlot(t, "2026-09-11T00:00:00Z", 0, 0.05),
		},
		{
			name: "a negative price is real — VAT makes it MORE negative",
			in:   hhSlot(t, "2026-09-11T00:00:00Z", -3.680, 0.05),
		},
		{
			name: "a zero ValidFrom is rejected",
			in:   mutate(func(s *Slot) { s.ValidFrom = time.Time{}; s.ValidTo = nil }),
			want: ReasonValidFromZero,
		},
		{
			name: "a non-UTC ValidFrom is rejected even when it names the same instant",
			in:   mutate(func(s *Slot) { s.ValidFrom = s.ValidFrom.In(london(t)) }),
			want: ReasonValidFromNotUTC,
		},
		{
			name: "a quarter-past ValidFrom is rejected",
			in: mutate(func(s *Slot) {
				s.ValidFrom = instant(t, "2026-09-11T00:15:00Z")
				*s.ValidTo = s.ValidFrom.Add(30 * time.Minute)
			}),
			want: ReasonValidFromUnaligned,
		},
		{
			name: "a stray second in ValidFrom is rejected",
			in: mutate(func(s *Slot) {
				s.ValidFrom = s.ValidFrom.Add(time.Second)
				*s.ValidTo = s.ValidFrom.Add(30 * time.Minute)
			}),
			want: ReasonValidFromUnaligned,
		},
		{
			name: "a stray nanosecond in ValidFrom is rejected",
			in: mutate(func(s *Slot) {
				s.ValidFrom = s.ValidFrom.Add(time.Nanosecond)
				*s.ValidTo = s.ValidFrom.Add(30 * time.Minute)
			}),
			want: ReasonValidFromUnaligned,
		},
		{
			name: "ValidTo before ValidFrom is rejected",
			in:   mutate(func(s *Slot) { *s.ValidTo = s.ValidFrom.Add(-30 * time.Minute) }),
			want: ReasonValidToNotAfter,
		},
		{
			name: "a zero-length slot is rejected",
			in:   mutate(func(s *Slot) { *s.ValidTo = s.ValidFrom }),
			want: ReasonValidToNotAfter,
		},
		{
			name: "an hour-long slot is rejected — a silently changed slot length",
			in:   mutate(func(s *Slot) { *s.ValidTo = s.ValidFrom.Add(time.Hour) }),
			want: ReasonSlotDuration,
		},
		{
			name: "a NaN exc-VAT price is rejected before it can poison a bill",
			in:   mutate(func(s *Slot) { s.ExcVATPence = math.NaN() }),
			want: ReasonPriceNotFinite,
		},
		{
			name: "a NaN inc-VAT price is rejected",
			in:   mutate(func(s *Slot) { s.IncVATPence = math.NaN() }),
			want: ReasonPriceNotFinite,
		},
		{
			name: "+Inf is rejected",
			in:   mutate(func(s *Slot) { s.IncVATPence = math.Inf(1) }),
			want: ReasonPriceNotFinite,
		},
		{
			name: "-Inf is rejected",
			in:   mutate(func(s *Slot) { s.ExcVATPence = math.Inf(-1) }),
			want: ReasonPriceNotFinite,
		},
		{
			name: "inc equal to exc is rejected — VAT has gone missing",
			in:   mutate(func(s *Slot) { s.IncVATPence = s.ExcVATPence }),
			want: ReasonVATMismatch,
		},
		{
			name: "VAT at 20% is rejected while we expect 5%",
			in:   hhSlot(t, "2026-09-11T00:00:00Z", 20, 0.20),
			want: ReasonVATMismatch,
		},
		{
			name: "a pence/pounds unit error in inc is caught by the VAT check",
			in:   mutate(func(s *Slot) { s.IncVATPence = s.ExcVATPence * 1.05 / 100 }),
			want: ReasonVATMismatch,
		},
		{
			name: "VAT applied the wrong way on a negative price is rejected",
			in: func() Slot {
				s := hhSlot(t, "2026-09-11T00:00:00Z", -3.680, 0.05)
				s.IncVATPence = -3.680 / 1.05 // less negative, i.e. VAT divided out
				return s
			}(),
			want: ReasonVATMismatch,
		},
		{
			name: "an unparseable tariff code is rejected",
			in:   mutate(func(s *Slot) { s.TariffCode = "AGILE" }),
			want: ReasonTariffCode,
		},
		{
			name: "a lowercase tariff code is rejected, not upcased",
			in:   mutate(func(s *Slot) { s.TariffCode = "e-1r-agile-24-10-01-a" }),
			want: ReasonTariffCode,
		},
		{
			name: "an empty tariff code is rejected",
			in:   mutate(func(s *Slot) { s.TariffCode = "" }),
			want: ReasonTariffCode,
		},
		{
			name: "a different region's prices are rejected, not archived under ours",
			in:   mutate(func(s *Slot) { s.TariffCode = "E-1R-AGILE-24-10-01-C" }),
			want: ReasonTariffMismatch,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Validate([]Slot{tc.in}, halfHourly())

			if tc.want == "" {
				if len(got.Accepted) != 1 || len(got.Rejected) != 0 {
					t.Fatalf("accepted %d, rejected %v; want the slot accepted", len(got.Accepted), reasons(got))
				}
				return
			}
			if len(got.Rejected) != 1 {
				t.Fatalf("rejected %v; want exactly one rejection with reason %q", reasons(got), tc.want)
			}
			if got.Rejected[0].Reason != tc.want {
				t.Errorf("reason = %q, want %q (detail: %s)", got.Rejected[0].Reason, tc.want, got.Rejected[0].Detail)
			}
			if len(got.Accepted) != 0 {
				t.Errorf("a rejected slot must not also be accepted; accepted %d", len(got.Accepted))
			}
			// The quarantine file is written from this, so the slot itself and a
			// human-readable detail both have to survive the rejection.
			if got.Rejected[0].Slot.ValidFrom != tc.in.ValidFrom {
				t.Errorf("rejection carries ValidFrom %s, want the offending slot's %s",
					got.Rejected[0].Slot.ValidFrom, tc.in.ValidFrom)
			}
			if got.Rejected[0].Detail == "" {
				t.Error("rejection has no detail; a quarantined slot with no explanation is un-triageable")
			}
		})
	}
}

// The one rule above all: nothing is silently dropped. Every input must come
// back exactly once, either accepted or rejected.
func TestValidate_AccountsForEverySlot(t *testing.T) {
	in := []Slot{
		hhSlot(t, "2026-09-11T00:00:00Z", 20, 0.05),
		hhSlot(t, "2026-09-11T00:30:00Z", 20, 0.20), // VAT wrong
		hhSlot(t, "2026-09-11T01:00:00Z", 21, 0.05),
		func() Slot { s := hhSlot(t, "2026-09-11T01:30:00Z", 21, 0.05); s.ExcVATPence = math.NaN(); return s }(),
	}

	got := Validate(in, halfHourly())

	if n := len(got.Accepted) + len(got.Rejected); n != len(in) {
		t.Fatalf("accepted %d + rejected %d = %d, want all %d slots accounted for",
			len(got.Accepted), len(got.Rejected), n, len(in))
	}
	if len(got.Accepted) != 2 {
		t.Errorf("accepted %d, want 2", len(got.Accepted))
	}
	// Accepted order is the input order, so the caller can hand the slice
	// straight to Store.Put and match it against what it fetched.
	if len(got.Accepted) == 2 {
		if !got.Accepted[0].ValidFrom.Equal(in[0].ValidFrom) || !got.Accepted[1].ValidFrom.Equal(in[2].ValidFrom) {
			t.Errorf("accepted order = %s, %s; want input order", got.Accepted[0].ValidFrom, got.Accepted[1].ValidFrom)
		}
	}
}

// Nothing in the VAT check may bake in today's 5%. A rate change is a config
// change, and the check has to follow it rather than rejecting the whole feed.
func TestValidate_VATRateIsAParameter(t *testing.T) {
	at20 := hhSlot(t, "2026-09-11T00:00:00Z", 20, 0.20)

	opts := halfHourly()
	opts.VATRate = 0.20
	if got := Validate([]Slot{at20}, opts); len(got.Accepted) != 1 {
		t.Errorf("with VATRate 0.20 a x1.20 slot was rejected %v; the rate must be a parameter", reasons(got))
	}

	at5 := hhSlot(t, "2026-09-11T00:00:00Z", 20, 0.05)
	if got := Validate([]Slot{at5}, opts); len(got.Rejected) != 1 {
		t.Errorf("with VATRate 0.20 a x1.05 slot was accepted; the parameter is being ignored")
	}
}

// The epsilon has to straddle a wide gap: float64 noise on penny-scale money is
// ~1e-14, while the smallest VAT change anyone would make moves inc by
// thousandths of a penny even on a 1p slot.
func TestValidate_VATEpsilon(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deviation  float64
		wantReject bool
	}{
		{name: "float64 representation noise is tolerated", deviation: 1e-12},
		{name: "a deviation just inside the epsilon is tolerated", deviation: DefaultVATEpsilonPence / 2},
		{name: "a thousandth of a penny is a real discrepancy", deviation: 0.001, wantReject: true},
		{name: "a whole penny is a real discrepancy", deviation: 1, wantReject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := hhSlot(t, "2026-09-11T00:00:00Z", 20, 0.05)
			s.IncVATPence += tc.deviation

			got := Validate([]Slot{s}, halfHourly())
			if tc.wantReject && len(got.Rejected) != 1 {
				t.Errorf("deviation %g was accepted, want rejected", tc.deviation)
			}
			if !tc.wantReject && len(got.Rejected) != 0 {
				t.Errorf("deviation %g was rejected %v, want accepted", tc.deviation, reasons(got))
			}
		})
	}
}

// Flat tariffs and standing charges are in the same archive, and their
// intervals run for months. The length check therefore has to be switchable off
// rather than assuming every row is half-hourly.
func TestValidate_SlotDurationIsConfigurable(t *testing.T) {
	quarterly := hhSlot(t, "2026-03-31T23:00:00Z", 23.234, 0.05)
	end := instant(t, "2026-06-30T23:00:00Z")
	quarterly.ValidTo = &end

	if got := Validate([]Slot{quarterly}, halfHourly()); len(got.Rejected) != 1 ||
		got.Rejected[0].Reason != ReasonSlotDuration {
		t.Errorf("a three-month interval passed the half-hourly length check: rejected %v", reasons(got))
	}

	opts := halfHourly()
	opts.SlotDuration = AnyDuration
	if got := Validate([]Slot{quarterly}, opts); len(got.Accepted) != 1 {
		t.Errorf("with AnyDuration a flat-tariff interval was rejected %v", reasons(got))
	}
}

// ---------------------------------------------------------------------------
// Gate B — plausibility. Per slot, FLAG but ACCEPT.
// ---------------------------------------------------------------------------

// Plunge pricing is the point of the tariff, so Gate B never rejects. A
// collector that refused surprising prices would discard exactly the slots
// worth knowing about.
func TestValidate_GateB_FlagsButNeverRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		exc  float64
		want []WarningKind
	}{
		{name: "an ordinary price is unremarkable", exc: 25},
		{name: "the observed maximum is not flagged", exc: 76.67},
		{name: "the observed minimum is not flagged", exc: -11.11},
		{name: "exactly zero is not flagged", exc: 0},
		{name: "above the documented cap is flagged", exc: 120, want: []WarningKind{WarnAboveCap}},
		{name: "below the sanity floor is flagged", exc: -60, want: []WarningKind{WarnBelowFloor}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Validate([]Slot{hhSlot(t, "2026-09-11T00:00:00Z", tc.exc, 0.05)}, halfHourly())

			if len(got.Accepted) != 1 || len(got.Rejected) != 0 {
				t.Fatalf("accepted %d, rejected %v; Gate B must never reject", len(got.Accepted), reasons(got))
			}
			if kinds := warningKinds(got); !sameKinds(kinds, tc.want) {
				t.Errorf("warnings = %v, want %v", kinds, tc.want)
			}
			for _, w := range got.Warnings {
				if w.Detail == "" {
					t.Error("warning has no detail; an unexplained flag is noise")
				}
			}
		})
	}
}

// A jump warning is about a break in shape, not about the level. It must only
// fire between slots that are genuinely adjacent, and only at a threshold no
// real day has ever reached — the worst recorded full-day spread is 62.6 p/kWh
// and the worst recorded adjacent step is 18.1.
func TestValidate_GateB_JumpWarning(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []Slot
		want int
	}{
		{
			name: "a normal diurnal step is not a jump",
			in: []Slot{
				hhSlot(t, "2026-09-11T00:00:00Z", 20, 0.05),
				hhSlot(t, "2026-09-11T00:30:00Z", 38, 0.05),
			},
		},
		{
			name: "a step larger than any observed day is flagged once",
			in: []Slot{
				hhSlot(t, "2026-09-11T00:00:00Z", 10, 0.05),
				hhSlot(t, "2026-09-11T00:30:00Z", 95, 0.05),
			},
			want: 1,
		},
		{
			name: "slots either side of a gap are not adjacent, so no jump is reported",
			in: []Slot{
				hhSlot(t, "2026-09-11T00:00:00Z", 10, 0.05),
				hhSlot(t, "2026-09-11T02:00:00Z", 95, 0.05),
			},
		},
		{
			name: "the two payment methods of one interval are not a jump",
			in: func() []Slot {
				dd := hhSlot(t, "2026-09-11T00:00:00Z", 10, 0.05)
				dd.PaymentMethod = "DIRECT_DEBIT"
				nd := hhSlot(t, "2026-09-11T00:00:00Z", 95, 0.05)
				nd.PaymentMethod = "NON_DIRECT_DEBIT"
				return []Slot{dd, nd}
			}(),
		},
		{
			name: "unsorted input is still measured between true neighbours",
			in: []Slot{
				hhSlot(t, "2026-09-11T00:30:00Z", 95, 0.05),
				hhSlot(t, "2026-09-11T00:00:00Z", 10, 0.05),
			},
			want: 1,
		},
		{
			name: "a rejected slot is not a neighbour — its price was never trusted",
			in: []Slot{
				hhSlot(t, "2026-09-11T00:00:00Z", 10, 0.05),
				hhSlot(t, "2026-09-11T00:30:00Z", 95, 0.20), // rejected by Gate A
				hhSlot(t, "2026-09-11T01:00:00Z", 11, 0.05),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Validate(tc.in, halfHourly())

			jumps := 0
			for _, w := range got.Warnings {
				if w.Kind == WarnJump {
					jumps++
				}
			}
			if jumps != tc.want {
				t.Errorf("jump warnings = %d, want %d (all warnings: %v)", jumps, tc.want, warningKinds(got))
			}
		})
	}
}

// Every recorded day must pass both gates clean. If a real response trips a
// warning then the threshold is wrong, not the data — and an alert that fires
// on normal days is an alert nobody reads.
func TestValidate_RealFixturesPassCleanly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fixture  string
		duration time.Duration
		want     int
	}{
		{name: "a short ordinary response", fixture: "unit_rates_simple.json", want: 4},
		{name: "a day with 36 negative slots", fixture: "unit_rates_negative.json", want: 48},
		{name: "the 46-slot spring DST day", fixture: "unit_rates_dst_spring_46.json", want: 46},
		{name: "the 50-slot autumn DST day", fixture: "unit_rates_dst_autumn_50.json", want: 50},
		{name: "the partial day missing its tail", fixture: "unit_rates_partial_day_46_of_48.json", want: 46},
		{
			// Flat-tariff rates: quarterly intervals, two payment methods per
			// interval, the newest open-ended.
			name: "payment-method pairs on a flat tariff", fixture: "unit_rates_payment_methods.json",
			duration: AnyDuration, want: 6,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := halfHourly()
			opts.SlotDuration = tc.duration

			got := Validate(fixtureSlots(t, tc.fixture), opts)

			if len(got.Rejected) != 0 {
				t.Errorf("real response rejected %d slot(s): %v (first detail: %s)",
					len(got.Rejected), reasons(got), got.Rejected[0].Detail)
			}
			if len(got.Warnings) != 0 {
				t.Errorf("real response raised %d warning(s): %v (first detail: %s)",
					len(got.Warnings), warningKinds(got), got.Warnings[0].Detail)
			}
			if len(got.Accepted) != tc.want {
				t.Errorf("accepted %d, want %d", len(got.Accepted), tc.want)
			}
		})
	}
}

func TestValidate_EmptyInput(t *testing.T) {
	got := Validate(nil, halfHourly())
	if len(got.Accepted) != 0 || len(got.Rejected) != 0 || len(got.Warnings) != 0 {
		t.Errorf("Validate(nil) = %+v, want an empty result and no panic", got)
	}
}

// ---------------------------------------------------------------------------
// Gate C — set level. Per local day, gates the COMPLETENESS SIGNAL.
// ---------------------------------------------------------------------------

// The expectation is 46, 48 or 50. Anything that hardcodes 48 is wrong twice a
// year, on the two days of the year hardest to debug.
func TestExpectedSlots_LocalCalendar(t *testing.T) {
	loc := london(t)

	for _, tc := range []struct {
		name string
		day  string
		want int
	}{
		{name: "an ordinary BST day", day: "2026-09-11T00:00:00Z", want: 48},
		{name: "an ordinary GMT day", day: "2026-01-15T00:00:00Z", want: 48},
		{name: "the spring forward day is 23 hours", day: "2026-03-29T00:00:00Z", want: 46},
		{name: "the autumn back day is 25 hours", day: "2025-10-26T00:00:00Z", want: 50},
		{name: "the day after the spring change is ordinary again", day: "2026-03-30T00:00:00Z", want: 48},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpectedSlots(instant(t, tc.day), loc); got != tc.want {
				t.Errorf("ExpectedSlots(%s) = %d, want %d", tc.day, got, tc.want)
			}
		})
	}
}

// Any instant during the local day names the same day, because the collector
// holds UTC instants and the day boundary is not midnight UTC.
func TestLocalDayWindow_IsAnchoredInTheZone(t *testing.T) {
	loc := london(t)

	// 2026-09-11 BST runs 23:00Z the day before to 23:00Z the same day.
	wantStart := instant(t, "2026-09-10T23:00:00Z")
	wantEnd := instant(t, "2026-09-11T23:00:00Z")

	for _, when := range []string{
		"2026-09-10T23:00:00Z", // the first instant of the local day
		"2026-09-11T12:00:00Z",
		"2026-09-11T22:59:59Z", // the last second of it
	} {
		start, end := LocalDayWindow(instant(t, when), loc)
		if !start.Equal(wantStart) || !end.Equal(wantEnd) {
			t.Errorf("LocalDayWindow(%s) = [%s, %s), want [%s, %s)", when, start, end, wantStart, wantEnd)
		}
	}

	// 22:00Z on the 10th is still 23:00 BST on the 10th, a DIFFERENT local day.
	start, _ := LocalDayWindow(instant(t, "2026-09-10T22:00:00Z"), loc)
	if start.Equal(wantStart) {
		t.Error("23:00 local on the previous day was folded into the wrong local day")
	}
}

// ✅ The observed case, and the reason Gate C exists at all. At 16:08Z on
// 2026-09-10 the API's horizon claimed all of 2026-09-11, but the day held 46
// of its 48 slots: the 23:00 and 23:30 BST slots had not been published. A
// collector that read "horizon advanced" as "day complete" would have archived
// a day with a hole and stopped looking.
func TestCheckDay_PartialDayAtTheTail(t *testing.T) {
	loc := london(t)
	slots := fixtureSlots(t, "unit_rates_partial_day_46_of_48.json")

	got := CheckDay(slots, instant(t, "2026-09-11T12:00:00Z"), loc)

	if got.Complete {
		t.Fatal("a day missing two slots was reported complete; the collector would stop polling")
	}
	if got.Expected != 48 || got.Present != 46 {
		t.Errorf("present %d of expected %d, want 46 of 48", got.Present, got.Expected)
	}
	want := []time.Time{
		instant(t, "2026-09-11T22:00:00Z"), // 23:00 BST
		instant(t, "2026-09-11T22:30:00Z"), // 23:30 BST
	}
	if len(got.Missing) != len(want) {
		t.Fatalf("missing %v, want %v", got.Missing, want)
	}
	for i, w := range want {
		if !got.Missing[i].Equal(w) {
			t.Errorf("missing[%d] = %s, want %s", i, got.Missing[i], w)
		}
	}
	if len(got.Overlaps) != 0 {
		t.Errorf("overlaps = %v, want none", got.Overlaps)
	}
	// Tail-only is the operationally useful distinction: keep polling, the
	// publication is still landing. An interior hole means something else is
	// wrong and polling will not fix it.
	if !got.MissingTailOnly() {
		t.Error("the gap is at the tail; MissingTailOnly must say so or the collector cannot tell 'wait' from 'alert'")
	}
}

// The two DST days are the whole reason the count is computed rather than
// assumed. Both of these are complete days, and a checker that expected 48
// would declare one short and the other overfull.
func TestCheckDay_DSTDaysAreComplete(t *testing.T) {
	loc := london(t)

	for _, tc := range []struct {
		name    string
		fixture string
		day     string
		want    int
	}{
		{name: "spring forward, 23 local hours", fixture: "unit_rates_dst_spring_46.json", day: "2026-03-29T12:00:00Z", want: 46},
		{name: "autumn back, 25 local hours", fixture: "unit_rates_dst_autumn_50.json", day: "2025-10-26T12:00:00Z", want: 50},
		{name: "an ordinary 48-slot day", fixture: "unit_rates_negative.json", day: "2026-04-11T12:00:00Z", want: 48},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CheckDay(fixtureSlots(t, tc.fixture), instant(t, tc.day), loc)

			if !got.Complete {
				t.Errorf("day reported incomplete: present %d of %d, missing %v, overlaps %v",
					got.Present, got.Expected, got.Missing, got.Overlaps)
			}
			if got.Expected != tc.want || got.Present != tc.want {
				t.Errorf("present %d of expected %d, want %d of %d", got.Present, got.Expected, tc.want, tc.want)
			}
			// Nothing is missing, so there is no gap to describe. Complete is
			// the question to ask about a whole day.
			if got.MissingTailOnly() {
				t.Error("a complete day reported a tail gap; it has no gap at all")
			}
		})
	}
}

// A mixed batch — several tariffs fetched together — must compare neighbours
// within a tariff only. Two tariffs' prices at the same instant are not a step
// in either curve, and reporting one would make every mixed sweep noisy.
func TestValidate_GateB_JumpIsPerTariff(t *testing.T) {
	cheap := hhSlot(t, "2026-09-11T00:00:00Z", 10, 0.05)
	dear := hhSlot(t, "2026-09-11T00:30:00Z", 95, 0.05)
	dear.TariffCode = "E-1R-AGILE-24-10-01-C"

	// No expected code: a mixed batch is the one case where "don't care" is right.
	opts := halfHourly()
	opts.TariffCode = ""

	got := Validate([]Slot{cheap, dear}, opts)

	if len(got.Rejected) != 0 {
		t.Fatalf("rejected %v; both codes parse and no code was requested", reasons(got))
	}
	if len(got.Warnings) != 0 {
		t.Errorf("warnings = %v, want none — the two slots belong to different curves", warningKinds(got))
	}
}

// The local-day arithmetic is meaningless without a zone, and a nil one falls
// back to UTC — where no day is ever 46 or 50 slots long. That is documented as
// a programming error rather than a mode, and pinned here so nobody "fixes" it
// into something that looks right on a DST day.
func TestExpectedSlots_NilLocationFallsBackToUTC(t *testing.T) {
	spring := instant(t, "2026-03-29T12:00:00Z")

	if got := ExpectedSlots(spring, nil); got != 48 {
		t.Errorf("ExpectedSlots(nil zone) = %d, want 48 — the UTC fallback", got)
	}
	if got := ExpectedSlots(spring, london(t)); got != 46 {
		t.Errorf("ExpectedSlots(Europe/London) = %d, want 46; the zone is what makes the count real", got)
	}
	if d := CheckDay(nil, spring, nil); d.Day.Location() != time.UTC {
		t.Errorf("CheckDay with a nil zone reported Day in %s, want UTC", d.Day.Location())
	}
}

// Gaps and overlaps are separate findings because they mean different things: a
// gap is unpriced energy, an overlap is two prices for one kWh.
func TestCheckDay_GapsAndOverlaps(t *testing.T) {
	loc := london(t)
	full := fixtureSlots(t, "unit_rates_negative.json") // a complete 48-slot day

	t.Run("an interior hole is missing but not tail-only", func(t *testing.T) {
		holed := make([]Slot, 0, len(full)-1)
		holed = append(holed, full[:10]...)
		holed = append(holed, full[11:]...)

		got := CheckDay(holed, instant(t, "2026-04-11T12:00:00Z"), loc)
		if got.Complete {
			t.Error("a day with an interior hole was reported complete")
		}
		if len(got.Missing) != 1 || !got.Missing[0].Equal(full[10].ValidFrom) {
			t.Errorf("missing = %v, want just %s", got.Missing, full[10].ValidFrom)
		}
		if got.MissingTailOnly() {
			t.Error("an interior hole must not read as a tail gap; polling will never fill it")
		}
	})

	t.Run("a duplicated interval is an overlap", func(t *testing.T) {
		dup := append(append([]Slot{}, full...), full[5])

		got := CheckDay(dup, instant(t, "2026-04-11T12:00:00Z"), loc)
		if got.Complete {
			t.Error("a day with two prices for one interval was reported complete")
		}
		if len(got.Overlaps) != 1 || !got.Overlaps[0].Equal(full[5].ValidFrom) {
			t.Errorf("overlaps = %v, want just %s", got.Overlaps, full[5].ValidFrom)
		}
		if len(got.Missing) != 0 {
			t.Errorf("missing = %v, want none — every interval is present", got.Missing)
		}
	})

	t.Run("an hour-long slot overruns its neighbour", func(t *testing.T) {
		long := append([]Slot{}, full...)
		end := long[5].ValidFrom.Add(time.Hour)
		long[5].ValidTo = &end

		got := CheckDay(long, instant(t, "2026-04-11T12:00:00Z"), loc)
		if got.Complete {
			t.Error("a day whose slots overlap was reported complete")
		}
		if len(got.Overlaps) != 1 {
			t.Errorf("overlaps = %v, want one", got.Overlaps)
		}
	})
}

// A fetch spans whatever window was asked for, so the day checker is routinely
// handed neighbouring days' slots. They must not count towards this day.
func TestCheckDay_IgnoresSlotsOutsideTheDay(t *testing.T) {
	loc := london(t)
	// The partial fixture covers local 2026-09-11 only, so every one of its
	// slots is outside local 2026-09-12.
	slots := fixtureSlots(t, "unit_rates_partial_day_46_of_48.json")

	got := CheckDay(slots, instant(t, "2026-09-12T12:00:00Z"), loc)

	if got.Present != 0 {
		t.Errorf("present = %d, want 0 — the previous day's slots leaked in", got.Present)
	}
	if got.Expected != 48 || len(got.Missing) != 48 {
		t.Errorf("expected %d with %d missing, want 48 and 48", got.Expected, len(got.Missing))
	}
	if got.Complete {
		t.Error("an empty day must not be complete; an unpublished day is not a finished one")
	}
	// An unpublished day is all tail. Saying so is what keeps the watch polling.
	if !got.MissingTailOnly() {
		t.Error("a wholly unpublished day should read as a tail gap")
	}
}

// A day's rows are stored whatever Gate C says, so the checker is a reporter
// and never filters. Proven by the window it reports being self-consistent.
func TestCheckDay_ReportsItsOwnWindow(t *testing.T) {
	loc := london(t)

	got := CheckDay(nil, instant(t, "2026-03-29T12:00:00Z"), loc)

	wantStart := instant(t, "2026-03-29T00:00:00Z") // GMT midnight
	wantEnd := instant(t, "2026-03-29T23:00:00Z")   // BST midnight, 23 hours later
	if !got.Start.Equal(wantStart) || !got.End.Equal(wantEnd) {
		t.Errorf("window = [%s, %s), want [%s, %s)", got.Start, got.End, wantStart, wantEnd)
	}
	if got.Day.Location() != loc {
		t.Errorf("Day is in %s, want %s — it is a local day, and must print as one", got.Day.Location(), loc)
	}
	if got.Expected != 46 {
		t.Errorf("Expected = %d, want 46", got.Expected)
	}
}

func sameKinds(got, want []WarningKind) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
