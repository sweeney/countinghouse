-- The standing-charge archive.
--
-- Same bitemporal model as unit_price, and for the same reason: the supplier's
-- daily standing charge is an external fact with interval validity that we may
-- need to justify a bill by long after the API that served it has changed.
--
-- It is a SEPARATE TABLE rather than a `kind` column on unit_price because the
-- two hold different UNITS — pence per DAY here, pence per kWh there — and a
-- single table would make it possible to sum them with a query that forgot to
-- filter. The Go types are separate for the same reason; the storage path is
-- shared, so the bitemporal machinery has one implementation.
--
-- Applied once, via common/db's schema_migrations ledger. See 001 for the rules.

CREATE TABLE IF NOT EXISTS standing_charge (
    tariff_code    TEXT NOT NULL,
    payment_method TEXT NOT NULL DEFAULT '',
    valid_from     TEXT NOT NULL,

    -- NULL means open-ended, which the CURRENT standing charge always is. This
    -- is the common case here, unlike unit_price where it is the exception.
    valid_to       TEXT,

    -- Pence per DAY, both VAT forms, as delivered. NOT NULL also rejects NaN and
    -- ±Inf, which SQLite would otherwise store as NULL.
    exc_vat_pence  REAL NOT NULL,
    inc_vat_pence  REAL NOT NULL,

    retrieved_at   TEXT NOT NULL,

    PRIMARY KEY (tariff_code, payment_method, valid_from),

    CHECK (valid_to IS NULL OR valid_to > valid_from)
);

CREATE INDEX IF NOT EXISTS idx_standing_charge_range
    ON standing_charge (tariff_code, valid_from);

-- Standing charges that changed AFTER we had already stored them.
--
-- Rarer than a unit-price restatement and worth MORE: a standing charge applies
-- to every day of every bill, so one revised row moves every total it touched.
CREATE TABLE IF NOT EXISTS standing_charge_restatement (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    tariff_code           TEXT NOT NULL,
    payment_method        TEXT NOT NULL DEFAULT '',
    valid_from            TEXT NOT NULL,
    old_exc_vat_pence     REAL NOT NULL,
    old_inc_vat_pence     REAL NOT NULL,
    new_exc_vat_pence     REAL NOT NULL,
    new_inc_vat_pence     REAL NOT NULL,
    previous_retrieved_at TEXT NOT NULL,
    detected_at           TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_standing_charge_restatement_detected
    ON standing_charge_restatement (tariff_code, detected_at);
