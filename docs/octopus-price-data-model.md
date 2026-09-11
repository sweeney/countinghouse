# Modelling Octopus spot prices

Status: **data model proposal.** Companion to `octopus-agile-spot-prices.md` (which covers
the API, the collector schedule and the query surface). This document answers one question:
**how do we represent half-hourly spot prices so they are still correct and still here in
twenty years?**

Every fact below was verified against the live API on 2026-09-10, not inferred from docs.
Decisions needing the user are marked **OPEN**.

---

## 0. The facts that constrain the schema

Verified live, region `N`, product `AGILE-24-10-01`:

| Fact | Value | What it forces |
|---|---|---|
| Our GSP region | **`_N`** (South Scotland), resolved from the site postcode | Not `C`. The API returns `_N` with a leading underscore; tariff codes use bare `N`. **Strip it.** |
| Our tariff | `E-1R-AGILE-24-10-01-N`, live **from 2026-09-10** | The switchover is *today* |
| Slots held to date | 34,798 | Full product history is re-fetchable — for now |
| `(tariff_code, valid_from)` unique? | **NO** | `payment_method` splits VAR tariffs into `DIRECT_DEBIT` / `NON_DIRECT_DEBIT` rows *for the same slot*. It must be in the key. |
| Price range seen | −11.11 … +76.67 p/kWh | Signed. No unsigned types, no clamping at 0 |
| Slots priced exactly `0.00` | **25** | **Zero is a legitimate price.** Absence can never be encoded as 0 |
| Slots per *local* day | 46 / 48 / **50** (2025-10-26 = 50, 2026-03-29 = 46) | Never key or bucket by local calendar day |
| `valid_to` | `null` on open-ended rates | Intervals are half-open and may be unbounded |
| Decimal places | `exc_vat` 1–2; `inc_vat` up to 5 (`62.11863`) | Don't round on ingest; don't recompute VAT |
| Size | 17,520 rows/yr → 350k rows / 20 yr | **Not a big-data problem.** Measured on the real archive: 7.8 MB for 34,894 slots, i.e. ~225 bytes/row once SQLite's index and text timestamps are counted — so ~80 MB over 20 years, not the ~17 MB an earlier estimate here assumed. Still trivial, and query performance is still a non-issue |

That last row is the one that reframes everything. This is not a big-data problem. It is a
*durability and correctness* problem about a small table.

---

## 1. Is Influx the right place to write this?

**No — and the reason is specific, not aesthetic.**

The case for Influx was that prices could be joined against energy in Flux. **That join does
not exist.** `internal/influx` contains no money logic whatsoever; cost is computed in Go,
per bucket, at `internal/energy/series.go:679` and `cost.go:63`. Influx returns kWh buckets
and Go multiplies. Putting prices in Influx would not let us join them in Flux — we would
still pull both sides into Go. The headline benefit is illusory.

Once that's gone, what's left is a set of mismatches:

1. **Retention is a deletion machine, and we were asked for perpetuity.** The existing
   `statehouse` bucket has 2-year retention. The archive's whole purpose is to outlive that.
   One wrong bucket name and the thing we promised to keep forever is silently expired.
   The risk is asymmetric: the failure is invisible until someone queries 2029 for 2026.

2. **Influx enforces nothing.** No primary key, no NOT NULL, no CHECK. A typo in a tag
   doesn't error — it creates a *parallel series*, silently splitting the history in two.
   For billing reference data, we want the store to refuse bad input. Influx never refuses.

3. **Writes are silently lossy in exactly the case we care about.** Point identity is
   `(measurement, tagset, timestamp)`, so re-writing a slot overwrites it with no trace.
   That gives idempotency for free — good — but it makes "we re-fetched the same value"
   indistinguishable from "Octopus restated the price and we destroyed the original". For a
   record that justifies a bill, that's the wrong default.

4. **A price is an interval; an Influx point is an instant.** Agile's `valid_to` is always
   `+30m` so it can be implied, but flat tariffs carry open-ended rates (`valid_to: null`).
   Modelling "applies until further notice" in an instant-keyed store means demoting
   `valid_to` to a field, where it can't be queried as an interval. Flux has no interval
   join; we'd hand-roll as-of matching anyway.

5. **It's the wrong shape of data.** Influx is for measurements of a thing over time:
   high write volume, recent-biased reads, downsampling, eventual expiry. This is an
   immutable external reference table with validity intervals, read uniformly across all
   history and never expired. That's a dimension, not a metric stream.

One further point, and it matters for CLAUDE.md: **the archive is not a cache.** The house
rule is "any cache must be rebuildable from Influx". This cannot be — it's rebuildable from
*Octopus*, and the entire reason to keep it is the day that stops being true. So it is
primary durable state, and the invariant needs amending explicitly rather than quietly.

### Options

| | Where | Enforces key | Perpetuity risk | Audit trail | New dep |
|---|---|---|---|---|---|
| **S1** | **SQLite via `common/db`** | **yes, in the engine** | none | second table, queryable | **none — already in `go.mod`** |
| S2 | Append-only JSONL, monthly files | in Go, at write | none | free (append-only) | none |
| S3 | Influx `energy_prices` bucket, infinite retention | no | retention misconfig | none | none |
| S4 | `energy_tariffs` config namespace | n/a | n/a | git history | none |

**S4 is out**: 17.5k rows/year against a 64 KB document limit.

**Decision: S1 — SQLite through `github.com/sweeney/identity/common/db`.**

An earlier draft of this document recommended append-only JSONL, on two arguments that do
not survive contact with the shared module:

- *"no new dependency"* — `identity/common v0.3.0` is already a direct dependency, and it
  already pulls `modernc.org/sqlite v1.46.2`. SQLite costs **nothing** new, and
  `modernc.org` is pure Go so `CGO_ENABLED=0` still holds for the cross-compiled deploy.
- *"backup is `cp -r`"* — `common/backup` is a better answer than `cp -r`: an `Uploader`
  (S3) plus a `Manager` with a schedule, debounce and coalescing. The perpetuity requirement
  is better served by a scheduled offsite copy than by a file that is merely easy to copy.

What SQLite then adds over JSONL is exactly the thing §2 says we want — **the store itself
refusing bad data**:

```sql
PRIMARY KEY (tariff_code, payment_method, valid_from)   -- §2's key, enforced
CHECK (valid_to IS NULL OR valid_to > valid_from)
CHECK (exc_p = exc_p)                                   -- rejects NaN
```

A typo'd tariff code can no longer quietly create a parallel history, and the idempotent
upsert is `INSERT … ON CONFLICT DO UPDATE`, which the engine makes atomic. The restatement
log becomes a second table rather than appended lines — strictly better, because it is
queryable ("show me every price that ever changed").

`common/db` suits this particularly well:

- `OpenWithMigrations(path, embed.FS, "migrations")` — migrations embedded and versioned in
  the repo, applied at boot.
- It re-runs **every** migration file on every boot (tolerating `duplicate column name`), so
  migrations must be written idempotently: `CREATE TABLE IF NOT EXISTS`,
  `CREATE INDEX IF NOT EXISTS`. There is no applied-migrations table.
- WAL, `foreign_keys=ON`, `busy_timeout=5000`, `MaxOpenConns(1)`, files chmod `0600`.
- **`:memory:` is supported**, which makes every store test hermetic and fast — no temp
  files, no fixtures on disk, real SQL semantics. That is a materially better test story
  than faking a file store.

Follow the house pattern (`identity/internal/db/db.go`): a thin `internal/db` wrapper with
`//go:embed migrations/*.sql`, aliasing `commondb.Database`, migrations named `NNN_name.sql`.

The read/write path still sits behind a `prices.Store` interface, so this stays reversible
and trivially fakeable for handler tests.


---

## 1a. One service, or a separate one?

**Recommendation: one service.** The collector and store live in countinghouse behind
`prices.Store` / `prices.Collector`.

The reasoning starts from what cannot move: **the costing join must live in countinghouse**,
because countinghouse is the only thing holding consumption. Splitting the work means the
*archive* goes elsewhere while the hard part stays. And the hard part is demonstrably the
join, not the storage — §4a is a live over-billing bug in the bucketing layer, found in an
afternoon, with nothing to do with where prices are kept. A separate service moves the easy
half away and buys nothing against accuracy.

Then the boundary itself doesn't survive contact:

- **Share the store directly** → two processes, one database. The boundary is fiction and
  now there are two deploys that can corrupt one file.
- **Serve prices over HTTP** → countinghouse needs up to 17,520 rows to price a year, per
  query, and *cannot answer "what did August cost?" while the price service is down*. A
  historical accounting question acquiring a runtime dependency on a live service is a bad
  trade — it's the opposite of the restart-safety the brief is built around.
- **Cache the archive in memory** (it's 17 MB, it fits) → then countinghouse holds the state
  anyway, and the second service bought nothing except a thing to keep running.

Scale argues nothing either way: 17,520 rows/year is not an operational pressure.

The multi-consumer case is real — greenhouse and HA will want the curve — but the answer is
countinghouse's **`/prices` endpoint**, already planned. Publishing the curve serves other
consumers without a second deployment.

**What would change this:** if the archive needed a different owner or lifecycle than
countinghouse — recording regions and tariffs we don't bill (a public-interest dataset), or
a second site deployed separately. Then a `pricehouse` earns its keep. Until then it's a
repo, a systemd unit, an id.swee.net client, a deploy script, a `/healthz` and a CI pipeline
in exchange for invariant tidiness.

Which is the honest cost of the alternative: the invariant. Amend it explicitly —

> **Read-side with respect to the home.** Countinghouse never ingests device telemetry. The
> single exception is the external price archive: idempotent, append-only writes of
> immutable, externally-sourced facts to a store that holds nothing else.

Both interfaces are the extraction seam if this is ever wrong, so it is a reversible
decision made cheaply now rather than an expensive one made early.

---

## 2. The grain, and the key

One row per **priced interval of one tariff**:

```
(tariff_code, payment_method, valid_from)  ->  value_exc_vat_p, value_inc_vat_p, valid_to
```

- `valid_from` is a **UTC instant**. Always. The API gives `Z`; keep it that way.
- The interval is **half-open**: `[valid_from, valid_to)`. Agile's is always `+30m`; store
  `valid_to` anyway rather than implying it, because flat tariffs need it and `null`
  (unbounded) must be representable.
- `payment_method` is in the key because **it has to be** (§0). For Agile it is `null`;
  normalise that to `""` so the key is a comparable value type in Go and not a pointer.

Two hard rules that fall out of the verified value domain:

- **Absence is not zero.** 25 real slots price at exactly `0.00p`. A missing slot must be a
  *missing key*, never a zero value — and must surface on the response as explicit unpriced
  energy, never as free energy. This is the same failure mode `fail-loud-not-silent` records.
- **Store what was delivered.** Keep pence, keep *both* VAT forms, keep full precision.
  Don't convert to £ and don't recompute `inc` from `exc` — `62.11863` is not reconstructible
  from `59.1606 × 1.05` under any rounding policy we'd pick, and baking today's 5% VAT
  assumption into a permanent record is how a 2029 reader gets a wrong answer. Convert at the
  cost layer, where `Tariff.Multiplier()` already lives.

---

## 3. Three relations, not one

The current `energy_tariffs` namespace conflates two different things: *what a tariff costs*
and *which tariff we were on*. Agile forces them apart, and the account API confirms the
separation is real — it returns our agreements as explicit dated records:

```
E-1R-VAR-22-11-01-N         2025-06-04 -> 2025-09-23
E-1R-OE-FIX-12M-25-09-09-N  2025-09-23 -> 2026-09-10
E-1R-AGILE-24-10-01-N       2026-09-10 -> 2027-09-10
```

So the model is three relations:

```
AGREEMENT     (fuel, valid_from, valid_to) -> tariff_code, payment_method
                 "which tariff applied to us, when"        — a handful of rows

UNIT_PRICE    (tariff_code, payment_method, valid_from) -> exc_p, inc_p, valid_to
                 "what a kWh cost on that tariff"         — the archive, 17.5k rows/yr

STANDING_CHARGE (tariff_code, payment_method, valid_from) -> exc_p, inc_p, valid_to
                 "what a day cost on that tariff"         — a handful of rows
```

Pricing any window is then one algorithm, for every tariff we have ever been on:

```
cost(window) = Σ over (consumption bucket ∩ agreement ∩ unit_price interval)
                  kWh × rate × vat_multiplier
             + Σ over (window ∩ agreement ∩ standing_charge interval)
                  days × daily_rate × vat_multiplier
```

### The unification worth noticing

**A flat tariff is a spot-price curve with very long slots.** `E-1R-OE-FIX-12M-25-09-09-N`
is literally two `UNIT_PRICE` rows (24.2348p until 2026-03-31T23:00Z, then 20.8948p). Agile
is 17,520 rows a year. *Same relation, same key, same algorithm* — only the row count
differs.

That's the payoff of this model: there is no `kind: flat` vs `kind: agile` branch in the
cost path. The proposed `kind` discriminator in `octopus-agile-spot-prices.md` §8 becomes
unnecessary — it describes *row density*, which the data already expresses. One code path,
exercised by both tariffs, which is also the one we can test hardest.

It also fixes a live bug by construction. `TariffFor(t)` ignores `t`
(`internal/config/tariffs.go:43`), so countinghouse prices *all* history at the current
rate — and the fixed tariff already changed rate on 2026-03-31. Every window before April
2026 is currently mispriced by 3.34p/kWh. Effective-date history isn't an Agile feature
request; it's an outstanding correctness fix that Agile merely makes unmissable.

**OPEN (2):** `AGREEMENT` — hand-maintained in `energy_tariffs` config, or synced from the
account API (which already has it right, and would have told us the switchover date without
anyone typing it)? Config is fewer moving parts; the API cannot drift from reality.

---

## 4. Restatement, and what "for perpetuity" has to mean

Octopus publishes a slot once and normally never changes it. *Normally* is not a schema.
If a slot is restated and we overwrite in place, the bill we issued last month becomes
unreproducible and nothing records that it changed.

So `retrieved_at` is part of the record, and the store is append-only:

```json
{"tariff_code":"E-1R-AGILE-24-10-01-N","payment_method":"","valid_from":"2026-09-10T21:30:00Z",
 "valid_to":"2026-09-10T22:00:00Z","exc_p":28.52,"inc_p":29.946,"retrieved_at":"2026-09-10T16:05:02Z"}
```

Reads take the highest `retrieved_at` per key. A re-fetch that agrees is a duplicate line
(cheap, harmless, and proof we checked). A re-fetch that *disagrees* is a restatement: it
appends, it is detectable by diff, and it should **log loudly and count on `/metrics`**.
This is the bitemporal minimum — valid-time in the key, transaction-time in the record —
and it costs one field.

Perpetuity also means being explicit that the archive is the authority. Today Octopus
serves all 34,798 slots, so the archive and the API agree and the archive looks redundant.
The moment `AGILE-24-10-01` is retired it stops being redundant and starts being the only
copy. Which implies:

- a **dedicated, never-expiring location** (no shared bucket, no retention policy);
- it is in the **backup set** — and note an Influx bucket and a directory of files are very
  different propositions for the person restoring it;
- **`known_to`** (the last slot we hold) on `/healthz`, so "do we have prices?" is
  answerable without running a query.

---

## 4a. Costing a range that spans more than one slot

This is where the accuracy risk actually lives — not in the store. Worth stating the
algorithm precisely, because three different interval families have to line up.

### Segments

A window's cost is a sum over **segments**: the intervals produced by cutting the window at
every boundary that can change the price of a kWh.

```
boundaries = sorted unique {
    window.start, window.stop,
    every UNIT_PRICE      boundary strictly inside the window,   // every 30m on Agile
    every AGREEMENT       boundary inside the window,            // e.g. 2026-09-10 switchover
    every STANDING_CHARGE boundary inside the window,
}
segments = consecutive pairs of boundaries
```

Each segment has, by construction, **exactly one** unit price, one agreement and one
standing charge. Then:

```
energy_cost = Σ segments  kWh(segment) × rate(segment) × vat(segment)
standing    = Σ segments  days(segment) × daily_rate(segment) × vat(segment)
```

A window spanning the flexible→Agile switchover prices each side with its own tariff and
its own VAT multiplier, and no code path knows it was special. Note `vat` is per-segment,
not per-window: VAT rates change, and a historical bill must use the rate of the day.

### The part to get right: edge segments

✅ **Verified: a 30m `aggregateWindow` lands on :00/:30, which is exactly where Agile slots
sit.** London's offset is always a whole number of hours, so interior buckets map 1:1 onto
price slots with no alignment work. DST days fall out correctly (46/48/50 slots) because the
axis steps in real time, not calendar days.

✅ **Fixed upstream by #30** (this section described it while it was live). Originally verified
2026-09-10 against `/devices/electricity_meter/series`:

| window start | first bucket | kWh |
|---|---|---|
| `14:00` (aligned) | `14:00` | 0.781 |
| `14:17` (mid-slot) | `14:00` | **0.781** |
| `14:29` (mid-slot) | `14:00` | **0.781** |

All three report the same energy, while the response echoes the *unsnapped* `from`. So a
window starting 1 minute before a slot boundary is credited with the whole slot's energy.
The measured `avg_w` for that bucket (1884 W) is independently inconsistent with 0.781 kWh
in 13 minutes (which would need 3.6 kW), confirming the kWh figure covers the full 30
minutes rather than the requested 13.

Under a flat tariff this is a small kWh error nobody noticed. Under Agile it is a
**mispriced** error: the over-counted energy is billed at that specific slot's rate, which
may be the day's most expensive. It is bounded by one slot at each edge — negligible over a
month (2 of 1488), but up to ~30% on a "what did the last 90 minutes cost?" query, which is
exactly the kind of question Agile invites.

**How it was actually fixed**, which is cleaner than the three-query approach proposed here:
the counter series stopped padding its range and now ranges from `win.Start`, letting
`increase()` re-base there so bucket 0 cannot contain pre-window energy at all. `bucketHours`
takes `start` and clips the head symmetrically with the tail. No extra queries.

The regression is pinned upstream: a window starting at `14:29` reports strictly less energy
than one starting at `14:00`, and `/series` now agrees with `/devices/{id}/energy` over an
off-grid window.

~~OPEN (6)~~ — settled upstream: the clip applies to `/series` as well, so the two endpoints
agree by construction rather than by coincidence.

### Unpriced segments

A segment with no price in the archive must **never** contribute 0. It contributes to
`Curve.Missing`, and the response carries the unpriced kWh explicitly. 25 real slots price
at exactly `0.00p`, so zero is data and absence is not.

---

## 5. Go shape

```go
// Slot is one priced interval of one tariff, exactly as Octopus delivered it.
// Money stays in pence with both VAT forms; conversion to £ happens in the cost
// layer where Tariff.Multiplier() already lives.
type Slot struct {
    TariffCode    string     // "E-1R-AGILE-24-10-01-N"
    PaymentMethod string     // "" | "DIRECT_DEBIT" | "NON_DIRECT_DEBIT" — in the key
    ValidFrom     time.Time  // UTC instant, inclusive
    ValidTo       *time.Time // exclusive; nil = open-ended
    ExcVATPence   float64    // signed: negative and exactly-zero are both real
    IncVATPence   float64    // as delivered; never recomputed from ExcVATPence
    RetrievedAt   time.Time  // transaction time — makes restatement visible
}

func (s Slot) Key() SlotKey // {TariffCode, PaymentMethod, ValidFrom}
func (s Slot) Covers(t time.Time) bool // ValidFrom <= t && (ValidTo == nil || t < *ValidTo)

// Store is the seam. A fake implementation backs handler tests, mirroring
// influx/fake.go, so no test touches a real store or the network.
type Store interface {
    Put(context.Context, []Slot) error            // idempotent; append-only
    Range(context.Context, string, time.Time, time.Time) ([]Slot, error)
    KnownTo(context.Context, string) (time.Time, error)
}

// Curve is a window's slots, indexed for pricing. Missing is the explicit
// record of buckets with NO price — surfaced to the caller as unpriced energy,
// never silently priced at zero.
type Curve struct {
    slots   map[SlotKey]Slot
    Missing []time.Time
}
```

`Missing` being a field of the result rather than an error is the deliberate choice: a
window with three unpriced slots should still return a bill, and should still say so.

---

## 6. What this changes in the existing plan

`octopus-agile-spot-prices.md` stands, with these amendments:

- **§3 (A1–A5)** — superseded by §1 here. The question isn't only *who writes*, it's *what
  store*; Influx's advantage was assumed rather than checked.
- **§4 (schema)** — superseded by §2–§3 here: `payment_method` joins the key, `valid_to` and
  `retrieved_at` become fields, and `AGREEMENT` is split out as its own relation.
- **§8 (config)** — drop the `kind: flat | agile` discriminator (§3 here: row density, not a
  type). `region: C` → `N`.
- **§9 (build order)** — M5 (effective-date history) moves ahead of M2: it is a live
  mispricing bug independent of Agile, and the `UNIT_PRICE` relation is the fix for both.
- **M1 is done**: API shape, region, tariff code, switchover date and the settled-consumption
  cross-check are all verified above.

---

## 7. Open decisions

1. ~~Store~~ — **decided: SQLite via `common/db`** (§1). Behind `prices.Store`.
2. **`AGREEMENT` from config, or synced from the account API?**
3. **Amend the CLAUDE.md invariant** to permit one narrow write path for external facts —
   and note the archive is *not* a rebuildable-from-Influx cache, so the existing cache
   clause doesn't cover it.
4. **Backfill depth** — from our 2026-09-10 switchover, or all 34,798 slots? The full pull
   is ~24 paged requests, once.
5. **Export** — no export MPAN on the account (`is_export: false` throughout), so
   `direction` stays a key field with one value for now.
