# Spot-price pipeline: detect → verify → store → spend → publish

Status: **implementation design.** Companion to `octopus-price-data-model.md` (the schema and
the store choice) and `octopus-agile-spot-prices.md` (API reference).

Four stages, in the order the data moves:

1. **Detect** a new publication (§1)
2. **Verify** it, then store (§2)
3. **Spend** — price a window from the archive (§3)
4. **Publish** the forward curve for other services (§4)

Everything below was checked against the live API on 2026-09-10. Facts marked ✅ are verified;
**OPEN** items need a decision.

---

## 1. Detecting a new tariff block

### Don't detect on the clock — detect on the horizon

Agile publishes tomorrow's slots at roughly 16:00 UK, but "roughly" isn't a contract: it
slips, and some days land in the evening. So the schedule decides *when to look*; the API
decides *whether it happened*.

The signal is one request:

```
GET /products/{product}/electricity-tariffs/{tariff}/standard-unit-rates/?page_size=1
```

Results are newest-first, so the first row's `valid_to` is the **horizon** — the end of the
last slot anyone has published. ✅ Verified: 344 bytes, one round trip. Comparing it to our
own `known_to` detects publication without fetching anything we already hold.

✅ Observed directly: at 14:32Z the horizon was `2026-09-10T22:00Z`; by 16:08Z it had moved to
`2026-09-11T22:00Z` and `count` rose 34,798 → 34,846. A day of slots had landed.

### Horizon moving is *not* the same as a day being complete

✅ **Verified, and this is the important one.** At 16:08Z the horizon claimed all of
2026-09-11, but local day 2026-09-11 held **46 of its 48 slots** — missing the last two:

```
local day 2026-09-11: window 23:00Z -> 23:00Z, have 46 slots
MISSING 2 slot(s):
  2026-09-11 22:00Z  = 23:00 BST
  2026-09-11 22:30Z  = 23:30 BST
-> gap is at the TAIL, not interior
```

Historical BST days all carry 48, so this is a transient partial publication, not a rule. A
collector that treated "horizon advanced" as "day published" would have stored a day with a
hole in it and stopped looking. Two separate states are therefore needed:

| State | Meaning | Drives |
|---|---|---|
| `known_to` | end of the newest slot held | "are prices arriving?" |
| `complete_to` | end of the newest **contiguous, fully-populated local day** | "can we stop polling?" / what the API advertises |

**Expected slot count comes from the local calendar, never from the constant 48.** A London
day is 46, 48 or 50 slots across a DST changeover (✅ 2025-10-26 = 50, 2026-03-29 = 46).
Compute it as `(local_midnight_next − local_midnight) / 30m`.

### Schedule

All of it clock-injected — `testutil.FakeClock` must be able to drive a whole publication day,
including a late one. No `time.Now()` in the scheduler.

| Trigger | Cadence | Purpose |
|---|---|---|
| Publication watch | every 5 min from 15:45 local, until tomorrow is **complete** | catch the daily block, and its tail |
| Backstop | hourly, always | a publication that misses the watch window entirely |
| Startup sweep | once, on boot | a redeploy self-heals |
| Catch-up sweep | daily, last 7 days | heal gaps from any outage |

Give the watch a deadline (say 23:00 local) so a day Octopus never completes doesn't spin
forever — it stops, and `/healthz` carries the incomplete day.

Writes are idempotent, so all four can overlap safely. Total cost is a handful of requests a
day, well inside any rate limit.

**OPEN (7):** back off the 5-minute watch after N failures, or keep it flat until the
deadline? Flat is simpler and the requests are tiny.

---

## 2. Verifying before storing

Three gates, in order, with one rule above all: **a rejected slot is never silently dropped.**
It is quarantined with its reason, logged, counted, and surfaced on `/healthz` — because a
quietly-discarded slot becomes unpriced energy weeks later, at which point nobody can tell
whether the price was missing or the collector ate it.

### Gate A — structural (per row; reject)

| Check | Rationale |
|---|---|
| `valid_from` / `valid_to` parse as UTC instants | the API sends `Z`; anything else is a contract break |
| `valid_from` lands on :00 or :30 | ✅ Agile slots always do; a drifted boundary means we misunderstood the feed |
| `valid_to − valid_from == 30m`, or `valid_to` is null | guards a silently changed slot length |
| both values finite (reject NaN, ±Inf) | a NaN price propagates into money and poisons a bill |
| `\|inc − exc × (1 + vat)\| < 1e-6` | ✅ verified **exactly** 1.05 across 1,440 April slots, negatives included (`exc −3.680 → inc −3.8640`). Catches a VAT change and a unit error in one assertion |
| `tariff_code` is the one we requested | guards a wrong-region fetch — the failure mode that would silently bill us on London prices |

### Gate B — plausibility (per row; accept **and** flag)

Flag, never reject. Agile's extremes are real, and a collector that refuses surprising prices
would discard exactly the slots worth knowing about.

| Check | Observed range |
|---|---|
| `inc ≤ 100 p/kWh` — the product's documented cap | ✅ max seen 76.67 exc (~80.5 inc) |
| `inc ≥ −50 p/kWh` — sanity floor, no contractual floor exists | ✅ min seen −11.11 exc |
| jump vs adjacent slots beyond a threshold | ✅ median intra-day spread 24.3 p/kWh, so the threshold must be generous |

### Gate C — set-level (per day; gates the *signal*, not the rows)

Store every row that passes A. Gate C decides only whether the day counts as complete:

- **contiguity** — no gaps, no overlaps across the local day
- **count** — equals the local-calendar expectation (46/48/50)

Partial days are stored and advertised as partial. That is the §1 finding made operational.

### Restatement

A slot we already hold, re-fetched with a different value, is a restatement. Per the model
doc the store is append-only, so it appends a record with a later `retrieved_at` — and it
**logs loudly, increments a metric, and shows on `/healthz`**. It must never be a silent
overwrite; a bill we already issued just changed.

### Quarantine

`rejected/YYYY-MM.jsonl`, each line the raw payload plus the failing check. Evidence is kept,
not binned — the same reasoning as `fail-loud-not-silent`.

### Health and metrics

`/healthz` gains a `prices` block: `last_fetch`, `last_success`, `known_to`,
`complete_to`, `incomplete_days`, `rejected`, `restatements`. "Do we have prices?" must
be answerable without running a query.

---

## 3. Calculating spend

The algorithm is in `octopus-price-data-model.md` §4a: cut the window at every boundary that
can change the price of a kWh (slot, agreement, standing charge), and sum over the resulting
segments. Each segment has exactly one price by construction, so a window spanning the
2026-09-10 switchover prices each side correctly with no special case.

Implementation notes specific to this service:

- **One query, all devices.** `BuildCounterSeriesFlux` already takes a device *set*, so a
  month of 30m buckets across every counter device is a single Flux query. The per-device
  split and the price multiply both happen in Go, where cost already lives.
- **`MaxBuckets` must be bypassed on the costing path.** 1488 buckets for a month at 30m
  exceeds the cap of 1000, but that cap is a *response-size* guard (`interval.go:8`) and the
  cost path returns scalars. Bypass it internally; don't raise it for `/series`.
- **Unpriced energy is a field, not an error.** A window with unpriced slots still returns a
  bill, and still reports the unpriced kWh. ✅ 25 slots in 11 months priced at exactly
  `0.00p`, so zero is data and absence must be a distinct thing.
- **Per-device attribution** names its method on the response (`attribution: "counter_slot"`)
  and reports a per-device `effective_rate`, so a noisy small-load figure is interpretable
  rather than merely wrong-looking. Still **OPEN (4)** per the model doc.

> ⚠️ **#27 is a prerequisite for short-window spend.** The series path currently bills energy
> from before `win.Start` (the first bucket is never clipped). Under a flat tariff that was a
> small kWh error; at slot resolution the over-counted energy is priced at a *specific* slot's
> rate. Accurate spend over a window that doesn't start on a 30m boundary needs #27 fixed.

---

## 4. Publishing the forward curve

This is the behaviour-shaping half. All read-side, all cacheable, and the boundary holds:
**countinghouse publishes the curve and answers "when is cheapest". It does not switch
anything on.** The actor stays statehouse/HA, where real-time control already lives.

### `GET /prices/upcoming?hours=24`

The dashboard endpoint. Shaped so a consumer can render without recomputing anything, and so
two different dashboards agree on what "cheap" means:

```json
{
  "tariff_code": "E-1R-AGILE-24-10-01-N",
  "generated_at": "2026-09-10T17:08:00+01:00",
  "known_to": "2026-09-11T23:00:00+01:00",
  "complete_to": "2026-09-11T22:00:00+01:00",
  "unit": "p/kWh",
  "vat_included": true,
  "summary": { "slots": 46, "min": 14.70, "max": 39.78, "mean": 25.37, "current": 22.49 },
  "slots": [
    { "valid_from": "2026-09-10T17:30:00+01:00", "valid_to": "2026-09-10T18:00:00+01:00",
      "price": 39.78, "rank": 46, "percentile": 1.00, "band": "peak" },
    { "valid_from": "2026-09-11T02:30:00+01:00", "valid_to": "2026-09-11T03:00:00+01:00",
      "price": 14.70, "rank": 1, "percentile": 0.02, "band": "cheap" }
  ],
  "cheapest": {
    "30m": { "from": "2026-09-11T02:30:00+01:00", "mean_price": 14.70 },
    "1h":  { "from": "2026-09-11T02:30:00+01:00", "mean_price": 14.95 },
    "2h":  { "from": "2026-09-11T02:00:00+01:00", "mean_price": 15.40 },
    "3h":  { "from": "2026-09-11T01:30:00+01:00", "mean_price": 16.02 }
  }
}
```

Design choices worth defending:

- **`rank` / `percentile` / `band` are served, not left to the client.** Otherwise every
  dashboard invents its own threshold and two screens in the same house disagree about
  whether now is cheap. Bands are derived from the returned window, so "cheap" means cheap
  *relative to what's coming* — which is the only definition that shapes behaviour.
- **`known_to` *and* `complete_to`.** A dashboard must be able to say "prices to
  23:00; tomorrow not published yet" instead of silently drawing a short axis. §1's partial
  day is precisely why both are on the wire.
- **`cheapest` by duration** is the payload that changes behaviour — "run the dishwasher at
  02:30". ✅ Worth it: median intra-day spread is 24.3 p/kWh.
- **Prices VAT-inclusive, in pence.** This is a display surface; the archive keeps both forms
  (model doc §2). `unit` and `vat_included` are explicit so nobody has to guess.

### The rest of the surface

| Route | Purpose |
|---|---|
| `GET /prices?from=&to=` | the curve over any window; past archive and future published slots are the same query |
| `GET /prices/cheapest?duration=3h&before=...` | one well-tested function, for automation rather than display |
| `GET /prices/stats?window=month` | daily min/max/mean, plunge-hour count |

Caching: the curve changes about once a day, so serve `ETag` + `Cache-Control` and let
dashboards poll cheaply.

Auth: these are data routes, so they require a token — and **service tokens must work**
(`ParseServiceToken`, per CLAUDE.md), because the consumers are other services.

Per house rules the same change updates `internal/httpapi/openapi.yaml` **and** `README.md`,
and `spec_test.go` keeps the new routes honest.

---

## 5. Build order

| M | Deliverable | Notes |
|---|---|---|
| 1 | `internal/octopus`: typed client, pagination (`page_size` is ✅ silently clamped to 1500 — follow `next`), retry/backoff, `Retry-After` | `httptest` + recorded fixtures; no live calls in tests |
| 2 | Tariff effective-date history (`UNIT_PRICE` relation, real `TariffFor(t)`) | **Fixes a live bug**: pre-April-2026 windows are mispriced by 3.34p/kWh today |
| 3 | `internal/prices`: store (append-only), validation gates A/B/C, quarantine, restatement detection | fake store mirroring `influx/fake.go` |
| 4 | Collector: horizon probe, completeness tracking, the four triggers, `/healthz` + `/metrics` | `FakeClock` drives a whole publication day, late and partial |
| 5 | Backfill mode | full history is ~24 paged requests, once |
| 6 | Slot-resolution spend in `/bill` + `/devices/{id}/cost`; `MaxBuckets` bypass; unpriced reporting | needs #27 for off-grid windows |
| 7 | `/prices`, `/prices/upcoming`, `/prices/cheapest`, `/prices/stats` + OpenAPI + README | the behaviour-shaping half |

M2 first is deliberate: it is an outstanding correctness fix, not an Agile feature, and the
relation it introduces is what everything else prices against.

### Test matter

- `httptest` Octopus serving recorded JSON: a paginated response, a 429 with `Retry-After`,
  a day with negative rates, and **a partial day missing its tail** (the ✅ observed case).
- `FakeClock` driving: a normal 16:00 publication; a late one; a missed day healed by the
  sweep; a day that never completes hitting the deadline.
- Table tests for slot alignment on both DST changeover days (46- and 50-slot), a window
  spanning the flexible→Agile switchover, and the capped/negative/exactly-zero extremes.
- A restatement: same key, different value, later `retrieved_at` → appended, flagged, counted.
