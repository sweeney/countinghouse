// Package prices is the half-hourly spot-price archive: the one place
// countinghouse writes.
//
// It holds an immutable external fact — what a supplier said a kWh cost during
// a given half hour — keyed by that half hour. Writes are idempotent upserts,
// so a re-fetch, a restart and an overlapping sweep all converge on the same
// table. See CLAUDE.md for the invariant this is the single exception to, and
// docs/octopus-price-data-model.md for why it is SQLite rather than Influx.
//
// The archive is NOT a cache. It is rebuildable from the supplier's API today,
// and the entire reason to keep it is the day that stops being true — a retired
// product, a closed account, a changed API. So it is primary durable state, and
// it gets an enforced key, a restatement log and a backup rather than a
// retention policy.
//
// Responsibilities are split deliberately:
//
//   - internal/octopus parses what the API returned and judges nothing.
//   - this package decides what is fit to store (validate.go) and stores it
//     (store.go). It can be tested end to end with no network at all.
//   - the cost layer reads it and prices energy. It never writes.
package prices

import (
	"context"
	"time"

	"github.com/sweeney/countinghouse/internal/octopus"
)

// Key identifies one priced interval uniquely.
//
// PaymentMethod is part of the key because it has to be: on variable tariffs the
// SAME half hour appears twice, once as DIRECT_DEBIT and once as
// NON_DIRECT_DEBIT, at different prices. Verified against the live API — a key
// of (tariff_code, valid_from) alone would silently collapse those two rows into
// one and lose whichever arrived first.
type Key struct {
	TariffCode    string
	PaymentMethod string // "" when the supplier sent null, as it does for Agile
	ValidFrom     time.Time
}

// Slot is one priced interval of one tariff, as archived.
//
// Money is stored in PENCE with BOTH VAT forms, exactly as delivered and
// unrounded. inc is never derived from exc: the relationship is exactly x1.05
// today, but a computed value would bake today's VAT rate into a permanent
// record, and the delivered inc carries more precision than any rounding policy
// we would pick. Conversion to GBP happens in the cost layer, where the VAT
// policy already lives.
//
// Both values are SIGNED. Prices go negative when the grid is oversupplied, and
// exactly 0.00 is a real price — so there is no unsigned type anywhere, and
// absence is a missing ROW, never a zero value.
type Slot struct {
	TariffCode    string
	PaymentMethod string

	// ValidFrom is the inclusive start, always UTC. ValidTo is the EXCLUSIVE
	// end, nil when open-ended (the supplier's null). nil survives as nil:
	// coercing it to a zero time would make a live rate look long expired.
	ValidFrom time.Time
	ValidTo   *time.Time

	ExcVATPence float64
	IncVATPence float64

	// RetrievedAt is TRANSACTION time — when we learned this value — as against
	// ValidFrom, which is VALID time. Keeping both is what makes a restatement
	// visible: if the supplier ever revises a price we have already billed, the
	// new row is distinguishable from the old rather than silently replacing it.
	RetrievedAt time.Time
}

// Key returns the slot's identity.
func (s Slot) Key() Key {
	return Key{TariffCode: s.TariffCode, PaymentMethod: s.PaymentMethod, ValidFrom: s.ValidFrom}
}

// Duration returns the interval's length, or 0 when it is open-ended.
func (s Slot) Duration() time.Duration {
	if s.ValidTo == nil {
		return 0
	}
	return s.ValidTo.Sub(s.ValidFrom)
}

// Covers reports whether t falls in [ValidFrom, ValidTo). An open-ended slot
// covers everything from ValidFrom onwards.
func (s Slot) Covers(t time.Time) bool {
	if t.Before(s.ValidFrom) {
		return false
	}
	return s.ValidTo == nil || t.Before(*s.ValidTo)
}

// FromRate converts a parsed API rate into an archivable slot.
//
// retrievedAt comes from the caller's injected clock rather than time.Now, so a
// test can drive a whole publication day deterministically and so the
// transaction time of a backfilled batch is the batch's, not each row's.
func FromRate(tariffCode string, r octopus.Rate, retrievedAt time.Time) Slot {
	return Slot{
		TariffCode:    tariffCode,
		PaymentMethod: r.PaymentMethod,
		ValidFrom:     r.ValidFrom.UTC(),
		ValidTo:       utcPtr(r.ValidTo),
		ExcVATPence:   r.ExcVATPence,
		IncVATPence:   r.IncVATPence,
		RetrievedAt:   retrievedAt.UTC(),
	}
}

// FromRates converts a batch, sharing one retrievedAt so the whole fetch is
// attributable to a single instant.
func FromRates(tariffCode string, rates []octopus.Rate, retrievedAt time.Time) []Slot {
	out := make([]Slot, 0, len(rates))
	for _, r := range rates {
		out = append(out, FromRate(tariffCode, r, retrievedAt))
	}
	return out
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// PutResult reports what a write actually did.
//
// The counts are separated because they mean different things operationally:
// Unchanged is the healthy steady state of a re-running sweep, while Restated is
// an event worth waking somebody for — a price we may already have billed has
// changed.
type PutResult struct {
	Inserted  int
	Unchanged int
	Restated  int
}

// Restatement records a price that changed after we had already stored it.
//
// This is the audit trail the archive exists to provide. It is a table rather
// than a log line because the question "has any price we billed ever changed?"
// must be answerable later, not only at the moment it happened.
type Restatement struct {
	Key
	OldExcVATPence float64
	OldIncVATPence float64
	NewExcVATPence float64
	NewIncVATPence float64

	// PreviousRetrievedAt is when we learned the old value; DetectedAt is when
	// we learned it had changed. Together they bound when the revision happened.
	PreviousRetrievedAt time.Time
	DetectedAt          time.Time
}

// Store is the archive's persistence seam.
//
// It exists so handler and collector tests can run against a fake, and so the
// storage decision stays reversible. The real implementation is SQLite; see
// store.go.
type Store interface {
	// Put upserts slots idempotently and records any restatement. Re-writing an
	// identical slot is not an error and not a write — it is the normal result
	// of the catch-up sweep re-reading a day we already hold.
	Put(ctx context.Context, slots []Slot) (PutResult, error)

	// Range returns the slots for a tariff overlapping [from, to), oldest
	// first. A slot is included when it overlaps the range at all, so a window
	// starting mid-slot still gets the price that covers its start.
	Range(ctx context.Context, tariffCode string, from, to time.Time) ([]Slot, error)

	// KnownThrough returns the end of the newest slot held for a tariff, or the
	// zero time when none are. This is what /healthz reports so "do we have
	// prices?" is answerable without a query.
	KnownThrough(ctx context.Context, tariffCode string) (time.Time, error)

	// Restatements returns the most recent recorded restatements, newest first.
	Restatements(ctx context.Context, tariffCode string, limit int) ([]Restatement, error)

	// Close releases the underlying handle.
	Close() error
}
