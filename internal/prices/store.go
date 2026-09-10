package prices

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"math"
	"time"

	commondb "github.com/sweeney/identity/common/db"
)

//go:embed migrations/*.sql
var migrations embed.FS

// timeLayout is how timestamps are stored: RFC3339, UTC, fixed width.
//
// Fixed width and a trailing Z together make lexicographic order identical to
// chronological order, which is what lets a range query be a plain BETWEEN on an
// index. It also keeps the archive legible to a human at a SQL prompt, which
// matters for a record meant to outlive the API it came from.
const timeLayout = "2006-01-02T15:04:05Z"

// SQLiteStore is the durable price archive.
//
// It is the concrete Store (see prices.go). SQLite rather than Influx because
// this is immutable reference data with interval validity and a composite key
// that we want the ENGINE to enforce, not a metric stream — see
// docs/octopus-price-data-model.md §1. It comes from identity/common/db, so it
// shares the house PRAGMAs (WAL, foreign_keys, busy_timeout), 0600 file
// permissions and the single-connection policy.
type SQLiteStore struct {
	db *commondb.Database
}

// Open opens (or creates) the archive at path and applies its migrations.
//
// path may be ":memory:", which is what tests use: a real engine with real
// constraints and no file to clean up. The whole reason for choosing SQLite is
// that the engine enforces the key, so testing against a fake store would test
// none of what matters.
func Open(path string) (*SQLiteStore, error) {
	database, err := commondb.OpenWithMigrations(path, migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("prices: open archive at %s: %w", path, err)
	}
	return &SQLiteStore{db: database}, nil
}

// Close releases the handle.
func (s *SQLiteStore) Close() error { return s.db.Close() }

// Put upserts slots idempotently, recording any restatement.
//
// The whole batch is one transaction, so a rejected slot leaves nothing behind.
// That matters because the collector's sweeps overlap by design and a partially
// applied day would be indistinguishable from a genuinely partial publication.
//
// Three outcomes per slot, reported separately because they mean different things
// operationally:
//
//   - Inserted: new.
//   - Unchanged: we already held this exact value. This is the normal, healthy
//     result of the daily catch-up sweep re-reading a day we have. retrieved_at
//     still advances, which tightens the bound on any later restatement.
//   - Restated: we held a DIFFERENT value. The new one wins and the old one is
//     written to the restatement log. This is the case worth waking somebody
//     for: a bill we may already have issued has moved.
func (s *SQLiteStore) Put(ctx context.Context, slots []Slot) (PutResult, error) {
	var res PutResult
	if len(slots) == 0 {
		return res, nil
	}

	// A batch containing the same key twice is a programming error, not data.
	// Caught before opening a transaction so the message is about the bug rather
	// than about a constraint: processed in order, the second row would look
	// like a restatement of the first and fire a false alarm.
	seen := make(map[Key]struct{}, len(slots))
	for _, sl := range slots {
		if _, dup := seen[sl.Key()]; dup {
			return PutResult{}, fmt.Errorf("prices: batch contains duplicate key %s/%s/%s",
				sl.TariffCode, sl.PaymentMethod, sl.ValidFrom.Format(timeLayout))
		}
		seen[sl.Key()] = struct{}{}

		// Checked here as well as by the NOT NULL columns so the error names the
		// actual problem. SQLite turns a non-finite REAL into NULL, which would
		// otherwise surface as a confusing constraint violation.
		if err := checkFinite(sl); err != nil {
			return PutResult{}, err
		}
	}

	tx, err := s.db.DB().BeginTx(ctx, nil)
	if err != nil {
		return PutResult{}, fmt.Errorf("prices: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	for _, sl := range slots {
		outcome, err := putOne(ctx, tx, sl)
		if err != nil {
			return PutResult{}, err
		}
		switch outcome {
		case outcomeInserted:
			res.Inserted++
		case outcomeUnchanged:
			res.Unchanged++
		case outcomeRestated:
			res.Restated++
		}
	}

	if err := tx.Commit(); err != nil {
		return PutResult{}, fmt.Errorf("prices: commit: %w", err)
	}
	return res, nil
}

type outcome int

const (
	outcomeInserted outcome = iota
	outcomeUnchanged
	outcomeRestated
)

// priceEpsilon is the tolerance for deciding two stored prices are "the same".
//
// A float64 survives the round trip through SQLite's REAL (also IEEE 754
// double) exactly, so this could be an equality test. It is a tiny epsilon
// instead purely so that a representation quirk can never manufacture a false
// restatement — and a false restatement is costly, because a restatement is
// meant to be rare enough to act on.
const priceEpsilon = 1e-9

func putOne(ctx context.Context, tx *sql.Tx, sl Slot) (outcome, error) {
	var (
		oldExc, oldInc float64
		oldRetrieved   string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT exc_vat_pence, inc_vat_pence, retrieved_at
		  FROM unit_price
		 WHERE tariff_code = ? AND payment_method = ? AND valid_from = ?`,
		sl.TariffCode, sl.PaymentMethod, sl.ValidFrom.Format(timeLayout),
	).Scan(&oldExc, &oldInc, &oldRetrieved)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := insert(ctx, tx, sl); err != nil {
			return 0, err
		}
		return outcomeInserted, nil
	case err != nil:
		return 0, fmt.Errorf("prices: read existing slot: %w", err)
	}

	unchanged := math.Abs(oldExc-sl.ExcVATPence) < priceEpsilon &&
		math.Abs(oldInc-sl.IncVATPence) < priceEpsilon

	if !unchanged {
		prev, perr := time.Parse(timeLayout, oldRetrieved)
		if perr != nil {
			// A stored timestamp we cannot parse means the archive is already
			// damaged; do not compound it by writing on top.
			return 0, fmt.Errorf("prices: unparseable stored retrieved_at %q: %w", oldRetrieved, perr)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO unit_price_restatement (
				tariff_code, payment_method, valid_from,
				old_exc_vat_pence, old_inc_vat_pence,
				new_exc_vat_pence, new_inc_vat_pence,
				previous_retrieved_at, detected_at)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			sl.TariffCode, sl.PaymentMethod, sl.ValidFrom.Format(timeLayout),
			oldExc, oldInc, sl.ExcVATPence, sl.IncVATPence,
			prev.UTC().Format(timeLayout), sl.RetrievedAt.UTC().Format(timeLayout),
		); err != nil {
			return 0, fmt.Errorf("prices: record restatement: %w", err)
		}
	}

	// The newest value wins either way; retrieved_at advances even when the value
	// is unchanged, so it always reads as "last confirmed".
	if _, err := tx.ExecContext(ctx, `
		UPDATE unit_price
		   SET valid_to = ?, exc_vat_pence = ?, inc_vat_pence = ?, retrieved_at = ?
		 WHERE tariff_code = ? AND payment_method = ? AND valid_from = ?`,
		nullableTime(sl.ValidTo), sl.ExcVATPence, sl.IncVATPence,
		sl.RetrievedAt.UTC().Format(timeLayout),
		sl.TariffCode, sl.PaymentMethod, sl.ValidFrom.Format(timeLayout),
	); err != nil {
		return 0, fmt.Errorf("prices: update slot: %w", err)
	}

	if unchanged {
		return outcomeUnchanged, nil
	}
	return outcomeRestated, nil
}

func insert(ctx context.Context, tx *sql.Tx, sl Slot) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO unit_price (
			tariff_code, payment_method, valid_from, valid_to,
			exc_vat_pence, inc_vat_pence, retrieved_at)
		VALUES (?,?,?,?,?,?,?)`,
		sl.TariffCode, sl.PaymentMethod,
		sl.ValidFrom.UTC().Format(timeLayout), nullableTime(sl.ValidTo),
		sl.ExcVATPence, sl.IncVATPence, sl.RetrievedAt.UTC().Format(timeLayout),
	)
	if err != nil {
		return fmt.Errorf("prices: insert slot %s/%s: %w",
			sl.TariffCode, sl.ValidFrom.Format(timeLayout), err)
	}
	return nil
}

// Range returns the slots for a tariff OVERLAPPING [from, to), oldest first.
//
// Overlap, not containment: a window beginning part-way through a slot must still
// receive the slot that covers its start, or the opening minutes of the window
// would be priced at nothing. An open-ended slot (valid_to NULL) overlaps
// anything at or after its start.
func (s *SQLiteStore) Range(ctx context.Context, tariffCode string, from, to time.Time) ([]Slot, error) {
	if !to.After(from) {
		return nil, fmt.Errorf("prices: range stop (%s) must be after start (%s)",
			to.Format(time.RFC3339), from.Format(time.RFC3339))
	}

	rows, err := s.db.DB().QueryContext(ctx, `
		SELECT tariff_code, payment_method, valid_from, valid_to,
		       exc_vat_pence, inc_vat_pence, retrieved_at
		  FROM unit_price
		 WHERE tariff_code = ?
		   AND valid_from < ?
		   AND (valid_to IS NULL OR valid_to > ?)
		 ORDER BY valid_from ASC, payment_method ASC`,
		tariffCode, to.UTC().Format(timeLayout), from.UTC().Format(timeLayout))
	if err != nil {
		return nil, fmt.Errorf("prices: range query: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []Slot
	for rows.Next() {
		sl, err := scanSlot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sl)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("prices: range rows: %w", err)
	}
	return out, nil
}

// KnownThrough returns the point our knowledge of a tariff's prices runs out:
// the end of the newest slot held, or its start when that slot is open-ended.
//
// An empty archive yields the ZERO TIME and no error. "We hold nothing yet" is a
// legitimate state at first boot and has to be distinguishable from a failure —
// this is what /healthz reports, so conflating them would make the health signal
// lie in exactly the situation it exists for.
func (s *SQLiteStore) KnownThrough(ctx context.Context, tariffCode string) (time.Time, error) {
	// COALESCE so an open-ended slot contributes its start rather than NULL:
	// reporting a nil end as the horizon would be meaningless.
	var newest sql.NullString
	err := s.db.DB().QueryRowContext(ctx, `
		SELECT MAX(COALESCE(valid_to, valid_from)) FROM unit_price WHERE tariff_code = ?`,
		tariffCode).Scan(&newest)
	if err != nil {
		return time.Time{}, fmt.Errorf("prices: known-through query: %w", err)
	}
	if !newest.Valid || newest.String == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(timeLayout, newest.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("prices: unparseable stored timestamp %q: %w", newest.String, err)
	}
	return t.UTC(), nil
}

// Restatements returns recorded restatements for a tariff, newest first.
func (s *SQLiteStore) Restatements(ctx context.Context, tariffCode string, limit int) ([]Restatement, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.DB().QueryContext(ctx, `
		SELECT tariff_code, payment_method, valid_from,
		       old_exc_vat_pence, old_inc_vat_pence,
		       new_exc_vat_pence, new_inc_vat_pence,
		       previous_retrieved_at, detected_at
		  FROM unit_price_restatement
		 WHERE tariff_code = ?
		 ORDER BY detected_at DESC, id DESC
		 LIMIT ?`, tariffCode, limit)
	if err != nil {
		return nil, fmt.Errorf("prices: restatements query: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []Restatement
	for rows.Next() {
		var (
			r                             Restatement
			validFrom, prevRetr, detected string
		)
		if err := rows.Scan(&r.TariffCode, &r.PaymentMethod, &validFrom,
			&r.OldExcVATPence, &r.OldIncVATPence,
			&r.NewExcVATPence, &r.NewIncVATPence,
			&prevRetr, &detected); err != nil {
			return nil, fmt.Errorf("prices: scan restatement: %w", err)
		}
		var err error
		if r.ValidFrom, err = time.Parse(timeLayout, validFrom); err != nil {
			return nil, fmt.Errorf("prices: unparseable restatement valid_from %q: %w", validFrom, err)
		}
		if r.PreviousRetrievedAt, err = time.Parse(timeLayout, prevRetr); err != nil {
			return nil, fmt.Errorf("prices: unparseable previous_retrieved_at %q: %w", prevRetr, err)
		}
		if r.DetectedAt, err = time.Parse(timeLayout, detected); err != nil {
			return nil, fmt.Errorf("prices: unparseable detected_at %q: %w", detected, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("prices: restatement rows: %w", err)
	}
	return out, nil
}

// scanSlot reads one unit_price row.
func scanSlot(rows *sql.Rows) (Slot, error) {
	var (
		sl        Slot
		validFrom string
		validTo   sql.NullString
		retrieved string
	)
	if err := rows.Scan(&sl.TariffCode, &sl.PaymentMethod, &validFrom, &validTo,
		&sl.ExcVATPence, &sl.IncVATPence, &retrieved); err != nil {
		return Slot{}, fmt.Errorf("prices: scan slot: %w", err)
	}
	var err error
	if sl.ValidFrom, err = time.Parse(timeLayout, validFrom); err != nil {
		return Slot{}, fmt.Errorf("prices: unparseable stored valid_from %q: %w", validFrom, err)
	}
	if validTo.Valid {
		to, err := time.Parse(timeLayout, validTo.String)
		if err != nil {
			return Slot{}, fmt.Errorf("prices: unparseable stored valid_to %q: %w", validTo.String, err)
		}
		utc := to.UTC()
		sl.ValidTo = &utc
	}
	if sl.RetrievedAt, err = time.Parse(timeLayout, retrieved); err != nil {
		return Slot{}, fmt.Errorf("prices: unparseable stored retrieved_at %q: %w", retrieved, err)
	}
	sl.ValidFrom = sl.ValidFrom.UTC()
	sl.RetrievedAt = sl.RetrievedAt.UTC()
	return sl, nil
}

// checkFinite refuses a non-finite price.
//
// The NOT NULL columns would catch this too — SQLite stores a non-finite REAL as
// NULL — but the resulting error would talk about a constraint rather than about
// the actual problem. A NaN that reached the archive would silently turn every
// bill touching the row into NaN for as long as it lived.
func checkFinite(sl Slot) error {
	for _, v := range []struct {
		name  string
		value float64
	}{{"exc_vat_pence", sl.ExcVATPence}, {"inc_vat_pence", sl.IncVATPence}} {
		if math.IsNaN(v.value) || math.IsInf(v.value, 0) {
			return fmt.Errorf("prices: slot %s/%s has non-finite %s (%v)",
				sl.TariffCode, sl.ValidFrom.Format(timeLayout), v.name, v.value)
		}
	}
	return nil
}

// nullableTime renders an optional timestamp for storage, mapping nil to SQL NULL
// so "open-ended" stays distinct from any real instant.
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(timeLayout)
}

// mustCount is a test helper: the number of rows in unit_price. It lives here
// rather than in the test file so it can use the unexported handle.
func (s *SQLiteStore) mustCount(t interface {
	Fatalf(string, ...any)
	Helper()
}, ctx context.Context) int {
	t.Helper()
	var n int
	if err := s.db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM unit_price`).Scan(&n); err != nil {
		t.Fatalf("count unit_price: %v", err)
	}
	return n
}
