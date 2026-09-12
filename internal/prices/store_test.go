package prices

import (
	"context"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The archive's persistence.
//
// Every test here runs against a real SQLite database, usually in memory. That
// is deliberate: the whole reason this is SQLite rather than a file of our own
// is that the ENGINE enforces the key and the constraints, and a fake store
// would test none of that. `:memory:` makes it hermetic and fast enough that
// there is no excuse for not doing it properly.
//
// Tariff codes use region "A" throughout: nothing here depends on which region
// the service is deployed for.
// ---------------------------------------------------------------------------

const (
	tariffA = "E-1R-AGILE-24-10-01-A"
	tariffB = "E-1R-AGILE-24-10-01-C"
)

// openMemory returns a store backed by an in-memory database.
func openMemory(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open(:memory:): %v", err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck
	return s
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad timestamp %q: %v", s, err)
	}
	return ts.UTC()
}

func ptr(t time.Time) *time.Time { return &t }

// slot builds a half-hourly slot starting at from.
func slot(t *testing.T, from string, exc, inc float64, retrieved string) Slot {
	t.Helper()
	start := at(t, from)
	end := start.Add(30 * time.Minute)
	return Slot{
		TariffCode:  tariffA,
		ValidFrom:   start,
		ValidTo:     ptr(end),
		ExcVATPence: exc,
		IncVATPence: inc,
		RetrievedAt: at(t, retrieved),
	}
}

// ---------------------------------------------------------------------------
// Schema and basic round-trip
// ---------------------------------------------------------------------------

func TestOpenAppliesMigrationsAndIsReRunnable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prices.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	ctx := context.Background()
	if _, err := s.Put(ctx, []Slot{slot(t, "2026-09-10T00:00:00Z", 20, 21, "2026-09-10T16:05:00Z")}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// common/db re-runs every migration file on every boot, so the migrations
	// must be idempotent. Reopening must neither fail nor lose data.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (migrations must be idempotent): %v", err)
	}
	defer s2.Close() //nolint:errcheck

	got, err := s2.Range(ctx, tariffA, at(t, "2026-09-10T00:00:00Z"), at(t, "2026-09-10T01:00:00Z"))
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d slots after reopen, want 1 — the archive must be durable", len(got))
	}
}

// Money and timestamps must survive the round trip exactly. An archive that
// rounds on the way in has destroyed the fact it exists to preserve.
func TestPutRangeRoundTripsExactly(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	want := Slot{
		TariffCode:    tariffA,
		PaymentMethod: "DIRECT_DEBIT",
		ValidFrom:     at(t, "2026-09-10T21:30:00Z"),
		ValidTo:       ptr(at(t, "2026-09-10T22:00:00Z")),
		// Values with more decimals than any rounding policy we would choose.
		ExcVATPence: 59.1606,
		IncVATPence: 62.11863,
		RetrievedAt: at(t, "2026-09-10T16:05:02Z"),
	}
	if _, err := s.Put(ctx, []Slot{want}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Range(ctx, tariffA, at(t, "2026-09-10T21:30:00Z"), at(t, "2026-09-10T22:00:00Z"))
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d slots, want 1", len(got))
	}
	g := got[0]
	if g.ExcVATPence != want.ExcVATPence || g.IncVATPence != want.IncVATPence {
		t.Errorf("money = (%v, %v), want (%v, %v) unrounded",
			g.ExcVATPence, g.IncVATPence, want.ExcVATPence, want.IncVATPence)
	}
	if !g.ValidFrom.Equal(want.ValidFrom) || g.ValidFrom.Location() != time.UTC {
		t.Errorf("ValidFrom = %v (%v), want %v in UTC", g.ValidFrom, g.ValidFrom.Location(), want.ValidFrom)
	}
	if g.ValidTo == nil || !g.ValidTo.Equal(*want.ValidTo) {
		t.Errorf("ValidTo = %v, want %v", g.ValidTo, want.ValidTo)
	}
	if !g.RetrievedAt.Equal(want.RetrievedAt) {
		t.Errorf("RetrievedAt = %v, want %v", g.RetrievedAt, want.RetrievedAt)
	}
	if g.PaymentMethod != "DIRECT_DEBIT" {
		t.Errorf("PaymentMethod = %q", g.PaymentMethod)
	}
}

// Negative and exactly-zero prices must round-trip. Both are real: the grid goes
// oversupplied, and 0.00 p/kWh genuinely occurs. Neither may be treated as a
// sentinel for "missing".
func TestPutPreservesSignedAndZeroPrices(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	in := []Slot{
		slot(t, "2026-04-11T12:00:00Z", -10.69, -11.2245, "2026-04-10T16:05:00Z"),
		slot(t, "2026-04-11T12:30:00Z", 0, 0, "2026-04-10T16:05:00Z"),
		slot(t, "2026-04-11T13:00:00Z", 15.15, 15.9075, "2026-04-10T16:05:00Z"),
	}
	if _, err := s.Put(ctx, in); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Range(ctx, tariffA, at(t, "2026-04-11T12:00:00Z"), at(t, "2026-04-11T13:30:00Z"))
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d slots, want 3 — a zero price is a row, not an absence", len(got))
	}
	for i := range in {
		if got[i].ExcVATPence != in[i].ExcVATPence {
			t.Errorf("slot %d exc = %v, want %v", i, got[i].ExcVATPence, in[i].ExcVATPence)
		}
	}
}

// An open-ended slot — a standing charge that has not been superseded — must
// keep its nil ValidTo. Coercing it to a zero time would make a live rate look
// long expired.
func TestPutPreservesOpenEndedValidTo(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	open := Slot{
		TariffCode:  tariffA,
		ValidFrom:   at(t, "2024-09-30T23:00:00Z"),
		ValidTo:     nil,
		ExcVATPence: 59.1606,
		IncVATPence: 62.11863,
		RetrievedAt: at(t, "2026-09-10T16:05:00Z"),
	}
	if _, err := s.Put(ctx, []Slot{open}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// An open-ended slot covers any later instant, so a much later window finds it.
	got, err := s.Range(ctx, tariffA, at(t, "2026-09-10T00:00:00Z"), at(t, "2026-09-11T00:00:00Z"))
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d slots, want 1 — an open-ended slot covers everything after its start", len(got))
	}
	if got[0].ValidTo != nil {
		t.Errorf("ValidTo = %v, want nil preserved", got[0].ValidTo)
	}
}

// ---------------------------------------------------------------------------
// The key
// ---------------------------------------------------------------------------

// The collision that forced payment_method into the key: on a variable tariff
// the same half hour is published twice at different prices. Both rows must
// coexist. If the key were (tariff_code, valid_from) one would overwrite the
// other and the archive would silently hold whichever arrived last.
func TestPaymentMethodIsPartOfTheKey(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	from := at(t, "2022-11-01T00:00:00Z")
	base := Slot{TariffCode: tariffA, ValidFrom: from, ValidTo: ptr(from.Add(30 * time.Minute)),
		RetrievedAt: at(t, "2026-09-10T16:05:00Z")}

	dd := base
	dd.PaymentMethod, dd.ExcVATPence, dd.IncVATPence = "DIRECT_DEBIT", 32.1552, 33.76296
	ndd := base
	ndd.PaymentMethod, ndd.ExcVATPence, ndd.IncVATPence = "NON_DIRECT_DEBIT", 32.812, 34.4526

	res, err := s.Put(ctx, []Slot{dd, ndd})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if res.Inserted != 2 {
		t.Fatalf("Inserted = %d, want 2 — the two payment methods are distinct rows", res.Inserted)
	}

	got, err := s.Range(ctx, tariffA, from, from.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows for one half hour, want 2", len(got))
	}
	prices := map[string]float64{}
	for _, g := range got {
		prices[g.PaymentMethod] = g.ExcVATPence
	}
	if prices["DIRECT_DEBIT"] == prices["NON_DIRECT_DEBIT"] {
		t.Error("the two payment methods should hold different prices")
	}
}

// Tariffs are isolated. A query for one must never see another's prices — that
// would be the quiet, catastrophic failure of billing on the wrong region.
func TestTariffsAreIsolated(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	a := slot(t, "2026-09-10T00:00:00Z", 20, 21, "2026-09-10T16:05:00Z")
	b := slot(t, "2026-09-10T00:00:00Z", 99, 103.95, "2026-09-10T16:05:00Z")
	b.TariffCode = tariffB

	if _, err := s.Put(ctx, []Slot{a, b}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Range(ctx, tariffA, at(t, "2026-09-10T00:00:00Z"), at(t, "2026-09-10T01:00:00Z"))
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d slots for tariff A, want 1", len(got))
	}
	if got[0].ExcVATPence != 20 {
		t.Errorf("tariff A price = %v, want 20 — the other tariff leaked in", got[0].ExcVATPence)
	}

	known, err := s.KnownTo(ctx, tariffB)
	if err != nil {
		t.Fatalf("KnownTo: %v", err)
	}
	if !known.Equal(at(t, "2026-09-10T00:30:00Z")) {
		t.Errorf("KnownTo(B) = %v, want B's own horizon", known)
	}
}

// A batch containing the same key twice is a programming error, not data. It
// must fail loudly rather than silently resolving to one of them — otherwise the
// second row looks like a restatement of the first and fires a false alarm.
func TestPutRejectsDuplicateKeysInOneBatch(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	a := slot(t, "2026-09-10T00:00:00Z", 20, 21, "2026-09-10T16:05:00Z")
	b := slot(t, "2026-09-10T00:00:00Z", 25, 26.25, "2026-09-10T16:05:00Z")

	if _, err := s.Put(ctx, []Slot{a, b}); err == nil {
		t.Fatal("want an error for a batch containing a duplicate key")
	}

	// And the batch must not have partially applied.
	got, err := s.Range(ctx, tariffA, at(t, "2026-09-10T00:00:00Z"), at(t, "2026-09-10T01:00:00Z"))
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d slots; a rejected batch must be atomic", len(got))
	}
}

// ---------------------------------------------------------------------------
// Idempotency and restatement
// ---------------------------------------------------------------------------

// Re-writing identical slots is the normal result of the daily catch-up sweep
// re-reading a day we already hold. It must be free of side effects and must not
// be reported as a write.
func TestPutIsIdempotent(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()
	in := []Slot{
		slot(t, "2026-09-10T00:00:00Z", 20, 21, "2026-09-10T16:05:00Z"),
		slot(t, "2026-09-10T00:30:00Z", 22, 23.1, "2026-09-10T16:05:00Z"),
	}

	first, err := s.Put(ctx, in)
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if first.Inserted != 2 || first.Unchanged != 0 || first.Restated != 0 {
		t.Fatalf("first Put = %+v, want 2 inserted", first)
	}

	// The same slots again, seen at a later instant.
	again := []Slot{
		slot(t, "2026-09-10T00:00:00Z", 20, 21, "2026-09-11T16:05:00Z"),
		slot(t, "2026-09-10T00:30:00Z", 22, 23.1, "2026-09-11T16:05:00Z"),
	}
	second, err := s.Put(ctx, again)
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if second.Inserted != 0 || second.Unchanged != 2 || second.Restated != 0 {
		t.Errorf("second Put = %+v, want 2 unchanged and nothing else", second)
	}

	got, err := s.Range(ctx, tariffA, at(t, "2026-09-10T00:00:00Z"), at(t, "2026-09-10T01:00:00Z"))
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d slots, want 2 — a re-put must not duplicate rows", len(got))
	}
	// retrieved_at advances on an unchanged re-fetch. That is what makes a later
	// restatement's PreviousRetrievedAt a TIGHT bound on when the revision
	// happened, rather than pointing at the first time we ever saw the value.
	for _, g := range got {
		if !g.RetrievedAt.Equal(at(t, "2026-09-11T16:05:00Z")) {
			t.Errorf("RetrievedAt = %v, want it advanced to the latest confirmation", g.RetrievedAt)
		}
	}
	if n := s.mustCount(t, ctx); n != 2 {
		t.Errorf("table holds %d rows, want 2", n)
	}
}

// A price that changes after we stored it is a RESTATEMENT: a bill we may
// already have issued just moved. The new value wins, and the old one is
// recorded rather than destroyed.
func TestPutRecordsRestatement(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	original := slot(t, "2026-09-10T00:00:00Z", 20, 21, "2026-09-09T16:05:00Z")
	if _, err := s.Put(ctx, []Slot{original}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	revised := slot(t, "2026-09-10T00:00:00Z", 24, 25.2, "2026-09-12T16:05:00Z")
	res, err := s.Put(ctx, []Slot{revised})
	if err != nil {
		t.Fatalf("Put revised: %v", err)
	}
	if res.Restated != 1 || res.Inserted != 0 || res.Unchanged != 0 {
		t.Fatalf("Put = %+v, want exactly 1 restated", res)
	}

	// The current row carries the NEW value.
	got, err := s.Range(ctx, tariffA, at(t, "2026-09-10T00:00:00Z"), at(t, "2026-09-10T00:30:00Z"))
	if err != nil {
		t.Fatalf("Range: %v", err)
	}
	if len(got) != 1 || got[0].ExcVATPence != 24 {
		t.Fatalf("current row = %+v, want the revised price 24", got)
	}

	// And the old one is auditable.
	rs, err := s.Restatements(ctx, tariffA, 10)
	if err != nil {
		t.Fatalf("Restatements: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("got %d restatements, want 1", len(rs))
	}
	r := rs[0]
	if r.OldExcVATPence != 20 || r.NewExcVATPence != 24 {
		t.Errorf("restatement = old %v new %v, want old 20 new 24", r.OldExcVATPence, r.NewExcVATPence)
	}
	if !r.PreviousRetrievedAt.Equal(at(t, "2026-09-09T16:05:00Z")) {
		t.Errorf("PreviousRetrievedAt = %v, want when we learned the old value", r.PreviousRetrievedAt)
	}
	if !r.DetectedAt.Equal(at(t, "2026-09-12T16:05:00Z")) {
		t.Errorf("DetectedAt = %v, want when we learned it had changed", r.DetectedAt)
	}
	if !r.ValidFrom.Equal(at(t, "2026-09-10T00:00:00Z")) {
		t.Errorf("restatement ValidFrom = %v", r.ValidFrom)
	}
}

// A change in the VAT-inclusive value alone still counts: the two are stored
// independently precisely so a VAT change is visible, so a revision to either
// must be recorded.
func TestRestatementDetectedOnIncVATAlone(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	if _, err := s.Put(ctx, []Slot{slot(t, "2026-09-10T00:00:00Z", 20, 21, "2026-09-09T16:05:00Z")}); err != nil {
		t.Fatal(err)
	}
	// Same ex-VAT, different inc-VAT: a VAT rate change.
	res, err := s.Put(ctx, []Slot{slot(t, "2026-09-10T00:00:00Z", 20, 24, "2026-09-12T16:05:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Restated != 1 {
		t.Errorf("Put = %+v, want 1 restated for an inc-VAT-only change", res)
	}
}

func TestRestatementsAreNewestFirstAndLimited(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	// Three slots, each later revised.
	for i, from := range []string{"2026-09-10T00:00:00Z", "2026-09-10T00:30:00Z", "2026-09-10T01:00:00Z"} {
		if _, err := s.Put(ctx, []Slot{slot(t, from, 20, 21, "2026-09-09T16:05:00Z")}); err != nil {
			t.Fatal(err)
		}
		detected := at(t, "2026-09-12T16:05:00Z").Add(time.Duration(i) * time.Hour)
		revised := slot(t, from, 30, 31.5, detected.Format(time.RFC3339))
		if _, err := s.Put(ctx, []Slot{revised}); err != nil {
			t.Fatal(err)
		}
	}

	all, err := s.Restatements(ctx, tariffA, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d restatements, want 3", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].DetectedAt.After(all[i-1].DetectedAt) {
			t.Errorf("restatements not newest-first at %d", i)
		}
	}

	limited, err := s.Restatements(ctx, tariffA, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Errorf("got %d with limit 2", len(limited))
	}
}

// ---------------------------------------------------------------------------
// Range semantics
// ---------------------------------------------------------------------------

// Range is an OVERLAP query, not a containment query. A window starting mid-slot
// must still receive the slot covering its start, or the first part of the window
// would be priced at nothing.
func TestRangeReturnsOverlappingSlots(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	in := []Slot{
		slot(t, "2026-09-10T00:00:00Z", 10, 10.5, "2026-09-10T16:05:00Z"),
		slot(t, "2026-09-10T00:30:00Z", 20, 21, "2026-09-10T16:05:00Z"),
		slot(t, "2026-09-10T01:00:00Z", 30, 31.5, "2026-09-10T16:05:00Z"),
		slot(t, "2026-09-10T01:30:00Z", 40, 42, "2026-09-10T16:05:00Z"),
	}
	if _, err := s.Put(ctx, in); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		from, to string
		want     []float64
	}{
		{
			name: "a window starting mid-slot includes the covering slot",
			from: "2026-09-10T00:20:00Z", to: "2026-09-10T00:40:00Z",
			want: []float64{10, 20},
		},
		{
			name: "exact slot bounds return just that slot",
			from: "2026-09-10T00:30:00Z", to: "2026-09-10T01:00:00Z",
			want: []float64{20},
		},
		{
			name: "the window end is exclusive, so a slot starting there is excluded",
			from: "2026-09-10T00:00:00Z", to: "2026-09-10T01:00:00Z",
			want: []float64{10, 20},
		},
		{
			name: "a window entirely before the data returns nothing",
			from: "2026-09-09T00:00:00Z", to: "2026-09-09T12:00:00Z",
			want: nil,
		},
		{
			name: "a window entirely after the data returns nothing",
			from: "2026-09-11T00:00:00Z", to: "2026-09-11T12:00:00Z",
			want: nil,
		},
		{
			name: "a wide window returns everything, ascending",
			from: "2026-09-09T00:00:00Z", to: "2026-09-12T00:00:00Z",
			want: []float64{10, 20, 30, 40},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Range(ctx, tariffA, at(t, tc.from), at(t, tc.to))
			if err != nil {
				t.Fatalf("Range: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d slots, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i].ExcVATPence != tc.want[i] {
					t.Errorf("slot %d = %v, want %v", i, got[i].ExcVATPence, tc.want[i])
				}
			}
			for i := 1; i < len(got); i++ {
				if !got[i].ValidFrom.After(got[i-1].ValidFrom) {
					t.Errorf("not ascending at %d", i)
				}
			}
		})
	}
}

func TestRangeRejectsInvertedWindow(t *testing.T) {
	s := openMemory(t)
	if _, err := s.Range(context.Background(), tariffA,
		at(t, "2026-09-11T00:00:00Z"), at(t, "2026-09-10T00:00:00Z")); err == nil {
		t.Fatal("want an error when to precedes from")
	}
}

// ---------------------------------------------------------------------------
// KnownTo — the health signal
// ---------------------------------------------------------------------------

func TestKnownTo(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	// Empty: the zero time, NOT an error. "We hold nothing yet" is a legitimate
	// state at first boot and must be distinguishable from a failure.
	got, err := s.KnownTo(ctx, tariffA)
	if err != nil {
		t.Fatalf("KnownTo on an empty archive should not error: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("KnownTo = %v, want the zero time", got)
	}

	if _, err := s.Put(ctx, []Slot{
		slot(t, "2026-09-10T00:00:00Z", 10, 10.5, "2026-09-10T16:05:00Z"),
		slot(t, "2026-09-10T21:30:00Z", 28.52, 29.946, "2026-09-10T16:05:00Z"),
	}); err != nil {
		t.Fatal(err)
	}

	got, err = s.KnownTo(ctx, tariffA)
	if err != nil {
		t.Fatal(err)
	}
	// The END of the newest slot, not its start: that is where our knowledge
	// actually runs out.
	if want := at(t, "2026-09-10T22:00:00Z"); !got.Equal(want) {
		t.Errorf("KnownTo = %v, want %v", got, want)
	}
}

// An open-ended slot means we know prices indefinitely from its start. Reporting
// its (nil) end as the horizon would be meaningless, so the start is used.
func TestKnownToWithOpenEndedSlot(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	if _, err := s.Put(ctx, []Slot{{
		TariffCode: tariffA, ValidFrom: at(t, "2024-09-30T23:00:00Z"), ValidTo: nil,
		ExcVATPence: 59.1606, IncVATPence: 62.11863, RetrievedAt: at(t, "2026-09-10T16:05:00Z"),
	}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.KnownTo(ctx, tariffA)
	if err != nil {
		t.Fatal(err)
	}
	if got.IsZero() {
		t.Error("an open-ended slot should still yield a horizon")
	}
}

// ---------------------------------------------------------------------------
// Defence in depth
// ---------------------------------------------------------------------------

// Gate A rejects non-finite prices before they ever reach here, but the store is
// the last line and must not persist one either: a NaN in the archive would
// silently turn a bill into NaN for as long as the row lived.
func TestStoreRefusesNonFinitePrices(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name     string
		exc, inc float64
	}{
		{name: "NaN ex-VAT", exc: math.NaN(), inc: 21},
		{name: "NaN inc-VAT", exc: 20, inc: math.NaN()},
		{name: "positive infinity", exc: math.Inf(1), inc: math.Inf(1)},
		{name: "negative infinity", exc: math.Inf(-1), inc: math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := slot(t, "2026-09-10T00:00:00Z", tc.exc, tc.inc, "2026-09-10T16:05:00Z")
			if _, err := s.Put(ctx, []Slot{bad}); err == nil {
				t.Fatal("want an error; a non-finite price must never be stored")
			}
			got, err := s.Range(ctx, tariffA, at(t, "2026-09-10T00:00:00Z"), at(t, "2026-09-10T00:30:00Z"))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Errorf("a non-finite price was persisted: %+v", got)
			}
		})
	}
}

// An inverted interval is refused by a CHECK constraint, so a bug upstream
// cannot leave an un-interpretable row in a permanent archive.
func TestStoreRefusesInvertedInterval(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	from := at(t, "2026-09-10T01:00:00Z")
	bad := Slot{
		TariffCode: tariffA, ValidFrom: from, ValidTo: ptr(from.Add(-30 * time.Minute)),
		ExcVATPence: 20, IncVATPence: 21, RetrievedAt: at(t, "2026-09-10T16:05:00Z"),
	}
	if _, err := s.Put(ctx, []Slot{bad}); err == nil {
		t.Fatal("want an error for valid_to before valid_from")
	}
}

func TestPutEmptyBatchIsANoOp(t *testing.T) {
	s := openMemory(t)
	res, err := s.Put(context.Background(), nil)
	if err != nil {
		t.Fatalf("an empty batch should not error: %v", err)
	}
	if res.Inserted != 0 || res.Unchanged != 0 || res.Restated != 0 {
		t.Errorf("res = %+v, want all zero", res)
	}
}

// The collector's sweeps can overlap by design — a startup sweep, the
// publication watch and the daily catch-up may all be in flight. Concurrent Puts
// must not corrupt the archive or deadlock.
func TestConcurrentPutsAreSafe(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			// Each writer writes the SAME day, as overlapping sweeps would.
			batch := make([]Slot, 0, 48)
			start := at(t, "2026-09-10T00:00:00Z")
			for i := 0; i < 48; i++ {
				from := start.Add(time.Duration(i) * 30 * time.Minute)
				batch = append(batch, Slot{
					TariffCode: tariffA, ValidFrom: from, ValidTo: ptr(from.Add(30 * time.Minute)),
					ExcVATPence: float64(i), IncVATPence: float64(i) * 1.05,
					RetrievedAt: at(t, "2026-09-10T16:05:00Z"),
				})
			}
			if _, err := s.Put(ctx, batch); err != nil {
				errs <- err
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Put: %v", err)
	}

	if n := s.mustCount(t, ctx); n != 48 {
		t.Errorf("table holds %d rows, want 48 — concurrent writers duplicated or lost rows", n)
	}
	// All writers wrote identical values, so nothing should look restated.
	rs, err := s.Restatements(ctx, tariffA, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 0 {
		t.Errorf("got %d restatements from identical concurrent writes, want 0", len(rs))
	}
}

// A whole realistic day, to be sure nothing chokes at the scale the collector
// actually writes.
func TestPutFullDay(t *testing.T) {
	s := openMemory(t)
	ctx := context.Background()

	start := at(t, "2026-09-09T23:00:00Z")
	batch := make([]Slot, 0, 48)
	for i := 0; i < 48; i++ {
		from := start.Add(time.Duration(i) * 30 * time.Minute)
		batch = append(batch, Slot{
			TariffCode: tariffA, ValidFrom: from, ValidTo: ptr(from.Add(30 * time.Minute)),
			ExcVATPence: 20 + float64(i)/10, IncVATPence: (20 + float64(i)/10) * 1.05,
			RetrievedAt: at(t, "2026-09-09T16:05:00Z"),
		})
	}
	res, err := s.Put(ctx, batch)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if res.Inserted != 48 {
		t.Errorf("Inserted = %d, want 48", res.Inserted)
	}
	got, err := s.Range(ctx, tariffA, start, start.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 48 {
		t.Errorf("Range returned %d, want 48", len(got))
	}
}
