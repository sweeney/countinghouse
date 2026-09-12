-- The price archive.
--
-- Every statement here must be IDEMPOTENT: common/db re-runs every migration
-- file on every boot and keeps no applied-migrations table, so anything that
-- cannot be executed twice will break startup the second time.
--
-- Timestamps are stored as RFC3339 TEXT in UTC with a trailing Z and a fixed
-- width, which makes lexicographic order identical to chronological order. That
-- is what lets the range query below use a plain BETWEEN on an index, while
-- keeping the archive readable by a human with a SQL prompt — which matters for
-- a record intended to outlive the API it came from.

CREATE TABLE IF NOT EXISTS unit_price (
    -- The key. payment_method is part of it because it has to be: on variable
    -- tariffs the same half hour is published twice, once per payment method, at
    -- different prices. A key of (tariff_code, valid_from) would silently
    -- collapse those two rows into one.
    tariff_code    TEXT NOT NULL,
    payment_method TEXT NOT NULL DEFAULT '',
    valid_from     TEXT NOT NULL,

    -- NULL means open-ended: a rate that has not yet been superseded, which is
    -- what the current standing charge always is.
    valid_to       TEXT,

    -- Money in pence, both VAT forms, as delivered. NOT NULL does double duty:
    -- SQLite stores a non-finite REAL as NULL, so this also rejects NaN and
    -- ±Inf, which would otherwise silently turn every bill touching the row into
    -- NaN for as long as it lived.
    exc_vat_pence  REAL NOT NULL,
    inc_vat_pence  REAL NOT NULL,

    -- Transaction time: when we learned this value, as against valid_from, which
    -- is valid time. Keeping both is what makes a restatement detectable.
    retrieved_at   TEXT NOT NULL,

    PRIMARY KEY (tariff_code, payment_method, valid_from),

    -- An inverted interval is un-interpretable, and this is a permanent archive:
    -- refuse it at the engine rather than trusting every future writer.
    CHECK (valid_to IS NULL OR valid_to > valid_from)
);

-- The primary key leads with tariff_code but has payment_method in the middle,
-- so it cannot serve a range scan over valid_from. This index does.
CREATE INDEX IF NOT EXISTS idx_unit_price_range
    ON unit_price (tariff_code, valid_from);

-- The restatement log: prices that changed AFTER we had already stored them.
--
-- A table rather than a log line, because the question "has any price we already
-- billed ever changed?" has to be answerable later, not only in the moment. Rows
-- are append-only; nothing updates or deletes them.
CREATE TABLE IF NOT EXISTS unit_price_restatement (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    tariff_code           TEXT NOT NULL,
    payment_method        TEXT NOT NULL DEFAULT '',
    valid_from            TEXT NOT NULL,

    old_exc_vat_pence     REAL NOT NULL,
    old_inc_vat_pence     REAL NOT NULL,
    new_exc_vat_pence     REAL NOT NULL,
    new_inc_vat_pence     REAL NOT NULL,

    -- Together these bound when the supplier's revision actually happened: we
    -- confirmed the old value at previous_retrieved_at and saw the new one at
    -- detected_at.
    previous_retrieved_at TEXT NOT NULL,
    detected_at           TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_unit_price_restatement_tariff
    ON unit_price_restatement (tariff_code, detected_at);
