package collector

import (
	"context"
	"testing"

	"github.com/sweeney/countinghouse/internal/octopus"
)

// ---------------------------------------------------------------------------
// Standing charges
// ---------------------------------------------------------------------------

// scFetcher is a fakeFetcher that can also serve standing charges.
type scFetcher struct {
	*fakeFetcher
	charges []octopus.Rate
	err     error
}

func (f scFetcher) StandingCharges(_ context.Context, _ octopus.TariffCode) ([]octopus.Rate, error) {
	return f.charges, f.err
}

func TestSyncStandingChargesArchivesTheSuppliersOwnFigure(t *testing.T) {
	base := newFakeFetcher(ts(t, "2026-09-30T23:00:00Z"), ts(t, "2026-10-01T22:00:00Z"))
	f := scFetcher{fakeFetcher: base, charges: []octopus.Rate{{
		ValidFrom:   ts(t, "2026-10-01T00:00:00Z"),
		ExcVATPence: 56.19, IncVATPence: 56.19, // zero-rated
	}}}
	h := newHarness(t, ts(t, "2026-10-01T16:10:00Z"), base)
	h.c.fetcher = f // swap in the richer fetcher

	res, err := h.c.SyncStandingCharges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Inserted != 1 {
		t.Fatalf("inserted %d, want 1", res.Inserted)
	}

	got, err := h.store.StandingCharges(context.Background(), testTariffCode,
		ts(t, "2026-10-01T00:00:00Z"), ts(t, "2026-11-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].IncVATPence != 56.19 {
		t.Fatalf("archived %+v, want one charge at 56.19p inc", got)
	}
	// Open-ended survives: the current standing charge always is.
	if got[0].ValidTo != nil {
		t.Errorf("valid_to = %v, want nil", got[0].ValidTo)
	}
}

// A fetcher that cannot serve standing charges is not an error. It is what every
// instance looked like before this existed, and costing falls back to config.
func TestSyncStandingChargesIsANoOpWithoutAStandingChargeFetcher(t *testing.T) {
	f := newFakeFetcher(ts(t, "2026-09-30T23:00:00Z"), ts(t, "2026-10-01T22:00:00Z"))
	h := newHarness(t, ts(t, "2026-10-01T16:10:00Z"), f)

	res, err := h.c.SyncStandingCharges(context.Background())
	if err != nil {
		t.Fatalf("a fetcher without standing charges must not be an error: %v", err)
	}
	if res.Inserted != 0 {
		t.Errorf("inserted %d, want 0", res.Inserted)
	}
}

// Writes are idempotent here too, so the daily sweep re-reading them is free.
func TestSyncStandingChargesIsIdempotent(t *testing.T) {
	base := newFakeFetcher(ts(t, "2026-09-30T23:00:00Z"), ts(t, "2026-10-01T22:00:00Z"))
	f := scFetcher{fakeFetcher: base, charges: []octopus.Rate{{
		ValidFrom: ts(t, "2026-10-01T00:00:00Z"), ExcVATPence: 56.19, IncVATPence: 59.0,
	}}}
	h := newHarness(t, ts(t, "2026-10-01T16:10:00Z"), base)
	h.c.fetcher = f

	ctx := context.Background()
	if _, err := h.c.SyncStandingCharges(ctx); err != nil {
		t.Fatal(err)
	}
	again, err := h.c.SyncStandingCharges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again.Unchanged != 1 || again.Inserted != 0 {
		t.Errorf("re-sync gave %+v, want one unchanged", again)
	}
}
