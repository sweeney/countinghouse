# Octopus Agile spot prices — research, plan and options

Status: **research + options, no code.** Written 2026-09-10 for the move to Octopus
Agile. Decisions marked **OPEN** need the user before implementation starts.

> **Superseded in part.** The API shape below is now **verified live** (2026-09-10), with
> corrections marked ✅/❌ inline. The *data model* — §3 (where prices live) and §4 (schema)
> — is superseded by **`octopus-price-data-model.md`**, which recommends against Influx on
> evidence this document assumed the other way.

The ask: record the half-hourly Agile spot price for perpetuity, price the bill with
it, and in time use the forward curve to shape usage.

---

## 1. How Agile works, and what the API gives us

Agile Octopus is a half-hourly settled import tariff. Every day at **~16:00 UK time**
Octopus publishes a unit rate for each of the next day's half-hour slots. Rates track
wholesale power, are **capped at £1/kWh** on the current product version
(`AGILE-24-10-01`), and have **no floor** — they go negative when the grid is
oversupplied ("plunge pricing"), i.e. you are paid to consume. The daily standing
charge is unchanged in character: still a flat pence-per-day.

### The REST API

Base `https://api.octopus.energy/v1/`. The endpoints we need are **public — no
authentication**:

| Purpose | Path |
|---|---|
| Half-hourly unit rates | `/products/{product}/electricity-tariffs/{tariff_code}/standard-unit-rates/` |
| Daily standing charge | `/products/{product}/electricity-tariffs/{tariff_code}/standing-charges/` |
| Product metadata | `/products/{product}/` |
| Postcode → region (GSP) | `/industry/grid-supply-points/?postcode=...` |

`tariff_code` is `E-1R-{product}-{GSP}`, e.g. `E-1R-AGILE-24-10-01-C` for London.
The GSP is a single letter (A–P, 14 regions) derived from the postcode or the MPAN.

> ❌ **Our region is `N` (South Scotland), not `C`.** Verified by resolving the site's
> postcode through `/industry/grid-supply-points/`, which returns `_N`. Our tariff is
> **`E-1R-AGILE-24-10-01-N`**, live from **2026-09-10** — today.
> Note the endpoint returns `_N` *with* a leading underscore while tariff codes use bare `N`.
> Region N's standing charge is **59.1606p/day ex-VAT**, not London's 37.6525p.

Query parameters: `period_from`, `period_to` (ISO 8601, **always with a trailing `Z`** —
local-time values are misread across the DST changeover), `page_size` (default 100,
max 1500), `page`.

Response is DRF-style paginated:

```json
{ "count": 1488, "next": "https://...&page=2", "previous": null,
  "results": [
    { "value_exc_vat": 21.42, "value_inc_vat": 22.491,
      "valid_from": "2026-09-10T16:30:00Z", "valid_to": "2026-09-10T17:00:00Z",
      "payment_method": null }, ... ] }
```

Notes that matter for us:

- **Values are pence per kWh**, not pounds. Our `energy_tariffs` schema is £. Convert
  once, at a documented boundary.
- Both `value_exc_vat` and `value_inc_vat` are given. `valid_to` is `null` on an
  open-ended rate (flat tariffs; not normally Agile).
- Results come newest-first; the slot interval is a half-open `[valid_from, valid_to)`.
- One month = 1488 slots = **2 pages** at `page_size=1500`. A year is ~17,520 rows.
- ✅ `page_size` is **silently clamped** to 1500, not rejected: asking for 2000 returns 200
  with 1500 rows and a `next` link. A client must follow `next` and never assume it got
  what it asked for.
- Rate limiting is undocumented for the public REST endpoints but real (429 with
  `Retry-After`; the GraphQL API documents a punitive escalating limiter). Our polling
  is a handful of requests a day, so this only constrains a naive
  fetch-on-every-request design.

**✅ Now verified live** (2026-09-10, direct calls). The response shape, pagination,
`period_from`/`period_to` windowing, newest-first ordering and the pence/VAT fields are all
exactly as described. Two corrections are marked above. One addition the schema must respect:
**`(tariff_code, valid_from)` is not a unique key** — `payment_method` splits VAR tariffs
into `DIRECT_DEBIT` and `NON_DIRECT_DEBIT` rows for the *same* slot. See the model doc §2.

### Optional: the account API

✅ **Done — the key works and the cross-check passed.** Countinghouse's Influx-derived
meter series matches Octopus's settled half-hourly consumption to **0.00% over August 2026**
(mean per-slot error 1.9 Wh over 1,488 slots), so slot-resolution whole-house pricing rests
on sound data. The account also supplied our exact agreement history (§3 of the model doc).

With an account API key (HTTP Basic, key as username) `/v1/accounts/{number}/` reports
the tariff the account is *actually* on, and
`/v1/electricity-meter-points/{mpan}/meters/{serial}/consumption/` returns the
**settled half-hourly consumption Octopus bills us for**. That is a strictly better
reconciliation target than our own meter counter — it is the bill. It costs us a stored
credential. See Option E.

---

## 2. What Agile breaks in countinghouse today

Countinghouse currently prices a window with one number:

```
cost = kWh_window × unit_rate × (1 + vat_rate)          # energy/cost.go
```

Under Agile that is simply wrong. Cost becomes a sum over slots:

```
cost = Σ_slots  kWh_slot × rate_slot × (1 + vat_rate)
```

Four consequences, in descending order of how much work they are:

**(a) Energy must be bucketed at 30m for costing.** Good news: it already can be.
`internal/influx/series_query.go` builds per-bucket kWh for both query paths
(`increase()`+`aggregateWindow`+`difference()` for counters; mean-power × bucket-hours
for the UPSs), and `30m` is already an allowed interval token. Slot alignment is free:
Octopus slots sit on UTC :00/:30, London's offset is always a whole hour, so a 30m
`aggregateWindow` in `Europe/London` lands on the same boundaries. DST days
(46 or 50 slots) fall out correctly because the axis is stepped in real time, not in
calendar days.

**(b) `MaxBuckets = 1000` is in the way.** A month at 30m is 1488 buckets, so the
series path refuses it. That cap is a *response-size* guard (§A of PLAN.md) and the
cost path returns a scalar, not buckets — so the fix is to let the internal costing
path bypass the cap, not to raise it for `/series`.

**(c) Tariff effective-date history stops being optional.** `TariffFor(t)` today
ignores `t` and returns the current rate (`internal/config/tariffs.go` — the seam is
already written and commented). The very first Agile bill month **spans the switchover**
from the flexible tariff, so pricing it correctly requires splitting the window at the
boundary and billing each side with its own tariff. This is a prerequisite, not a
follow-up.

**(d) Per-device attribution collides with counter quantisation.** This is the one
non-obvious problem. PLAN.md's backlog records that plug `energy_kwh` counters tick in
**0.1 kWh steps**. At 30m resolution a low-draw device's bucket reads 0, 0, 0, 0.1 —
and that 0.1 lands in whichever slot the counter happened to roll over, which may be a
cheap slot or a £1/kWh one. Whole-house cost stays exact (the meter counter is far
finer, and is the billing reference); **per-device Agile cost at slot resolution is
noisy for small loads**. Options in §5.

---

## 3. Option set A — where prices live (the decision that shapes everything)

This is the real fork, because "record for perpetuity" is durable state and CLAUDE.md
says **read-side only, no ingest**.

Worth being precise about which invariant is at stake. "Stateless w.r.t. accumulation"
forbids *running totals* — state that cannot be reconstructed and that a redeploy would
silently corrupt. A price archive is not that: each slot is an **immutable external fact,
keyed by its own timestamp**, so writes are idempotent upserts and a restart or a
double-run converges to the same table. What Agile actually asks us to amend is the
narrower "no ingest" rule.

| | Approach | Perpetuity | Invariant cost | Verdict |
|---|---|---|---|---|
| **A1** | Fetch from Octopus on every query, cache in memory | ✗ — depends on Octopus serving history forever; product retirement and outages lose the bill record | none | Insufficient alone |
| **A2** | Put prices in the `energy_tariffs` config namespace | ✗ | none | Wrong shape — 17.5k rows/yr is not a hand-maintained config document |
| **A3** | **Countinghouse writes prices to a dedicated Influx bucket** | ✓ | amends "no ingest" | **Recommended** |
| **A4** | A separate service (`pricehouse`) writes; countinghouse reads | ✓ | none | Purest, but a whole new repo, systemd unit and id.swee.net client for one poller |
| **A5** | systemd timer + shell script on garibaldi writing line protocol | ✓ | none (not our code) | Cheapest to build, worst to operate: untested, unversioned, fails silently |

**Recommendation: A3, with the exception written down rather than smuggled in.** The
invariant becomes:

> Read-side with respect to *the home*. Countinghouse never ingests device telemetry.
> The single exception is the external price archive: idempotent writes of immutable,
> externally-sourced facts to a bucket that holds nothing else.

Two hard requirements come with it:

1. **A separate bucket, `energy_prices`, with infinite retention.** The `statehouse`
   bucket has a 2-year retention policy — writing prices there would quietly delete
   the thing we were asked to keep for perpetuity. This also keeps the blast radius
   honest: the write token is scoped to `energy_prices` only, so countinghouse
   physically cannot write device data.
2. **A second Influx credential** in local config (`influx.write_token` /
   `write_token_file`), distinct from the existing read token.

If the user would rather not touch the invariant at all, **A4 is the fallback** and
nothing else in this plan changes — §4–§7 move to the other service, and countinghouse
reads the same bucket.

---

## 4. Option set B — archive schema

Recommended (Influx, in the `energy_prices` bucket):

```
measurement: energy_price
  tags:   product_code = AGILE-24-10-01
          tariff_code  = E-1R-AGILE-24-10-01-C
          region       = C
          fuel         = electricity
          direction    = import          # forward-compat: AGILE-OUTGOING is export
  fields: value_exc_vat_p (float)        # exactly as Octopus returned it
          value_inc_vat_p (float)
  time:   valid_from                     # slot is half-open [valid_from, +30m)

measurement: energy_standing_charge
  tags:   same
  fields: value_exc_vat_p, value_inc_vat_p
  time:   valid_from                     # changes rarely; one point per change
```

Two principles behind it:

- **Archive the source fact, derive the presentation.** Store pence exactly as
  delivered, both VAT forms. Convert to £ in the cost layer, where the existing
  `Multiplier()` VAT policy lives. Storing a pre-converted £ figure would bake today's
  VAT assumption into a permanent record.
- **Timestamp = `valid_from`, tags identify the tariff.** That makes a re-fetch of a
  slot an overwrite of the same point rather than a duplicate, which is what makes
  backfill, catch-up sweeps and restarts all safe to re-run.

Volume: ~17.5k points/year, ~350k over twenty years. Nothing.

Rejected: SQLite or flat files alongside Influx (a second storage engine to back up and
query, for data that wants to be joined against energy series that are already in
Influx); the config service (§A2).

---

## 5. Option set C — per-device attribution under quantisation

Whole-house Agile cost is exact. Per-device is the question. Three defensible answers:

- **C1 — Slot-price the counter deltas, and say so.** Simplest and internally
  consistent (per-device costs sum to the house energy cost). Noisy for small loads:
  a 0.1 kWh tick priced at a spike slot over-attributes, at a plunge slot
  under-attributes. Errors are unbiased and wash out over a month.
- **C2 — Shape from power, magnitude from the counter.** Use `integral(power_w)` per
  slot to get the *shape* of each device's draw, rescale the shape so the window total
  matches the authoritative counter total, then price the rescaled slots. Fixes the
  timing error without inheriting the known high bias of raw ∫power (PLAN.md backlog:
  winefridge counter 1.3 kWh vs ∫power 2.1 kWh in one day). More query load and a
  concept to document.
- **C3 — Effective-rate pricing.** Give each device the window's kWh-weighted average
  house price. Cheap, stable, and honest — but it can't show that the tumble dryer ran
  during the peak, which is precisely the insight Agile is for.

**Recommendation: C1 by default, with the response naming its method** (e.g.
`"attribution": "counter_slot"`) and reporting a per-device `effective_rate` so the
number is interpretable. Add C2 behind a parameter later if the noise proves annoying —
it reuses the `source=counter|power` seam already sketched in PLAN.md's backlog.

Whatever we pick: **a slot with no known price must never silently price at £0.** It
should surface as an explicit unpriced-energy figure on the response. Silence that
reads as data is the failure mode this repo keeps refusing to ship.

---

## 6. Option set D — the collector's schedule

Prices for tomorrow appear ~16:00 UK. Recommended posture:

- Poll at **16:05 local**, then retry with backoff until covered (Octopus is
  occasionally late; some days rates land later in the evening).
- A **daily catch-up sweep** re-fetching the last 7 days, to heal gaps from any outage.
- A **sweep on startup**, so a redeploy self-heals.
- Idempotent writes make all three safe to overlap.
- All of it driven by the **injected clock** — `testutil.FakeClock` must be able to
  drive a whole publication day deterministically. No `time.Now()` in the scheduler.

Health and observability, which is where this design earns its keep:

- `/healthz` gains a `prices` block: `last_fetch`, `last_success`, **`known_to`**
  (the timestamp of the last slot we hold) and a gap count. "Do we have prices?" must
  be answerable without a query.
- `/metrics` gains fetch counts, errors, slots written.
- Fail-open on *fetching* (keep the archive, degrade `/healthz`), never fail-quiet on
  *pricing* (see §5).

Backfill: a one-shot mode (`-backfill from=...`) that walks the product's history. Two
sub-decisions — backfill from our switchover date (enough for our bills), or the
product's whole history (a more useful public-interest archive for ~2 pages of
requests). Recommend the latter; it costs nothing.

---

## 7. Option set E — the usage-shaping surface (the "in time" half)

Once the archive exists, the forward curve is free — the same table already holds
tomorrow's published slots. Proposed endpoints, all read-side and cacheable:

- `GET /prices?from=&to=` — the curve over a window (past archive and future published
  slots are the same query).
- `GET /prices/next` — everything currently known about the future. This is what an
  automation consumer polls.
- `GET /prices/cheapest?duration=3h&before=2026-09-11T07:00Z` — the cheapest contiguous
  run of slots for a deferrable load (dishwasher, EV, immersion). One small,
  well-tested function; enormous practical value.
- `GET /prices/stats?window=month` — daily min/max/mean, plunge-hour count. Dashboard
  fodder.

**Boundary to hold:** countinghouse publishes the curve and answers "when is cheapest".
It does not switch anything on. The actor is statehouse/HA, which stays where the
real-time control already lives. That keeps this service read-side in the sense that
actually matters.

Optional extra (needs an account API key): pull Octopus's own settled half-hourly
consumption and reconcile against it. Today `/bill` reconciles against our meter
counter; that would let it reconcile against **the actual bill**, turning the
`unmonitored` remainder from an inference into a checked figure.

---

## 8. Config changes

`energy_tariffs` gains a tariff kind and the history the switchover forces:

```json
{ "tariffs": {
    "electricity": {
      "kind": "agile",
      "product_code": "AGILE-24-10-01",
      "region": "C",
      "tariff_code": "E-1R-AGILE-24-10-01-C",
      "daily_standing_charge": 0.5294,
      "unit": "kWh",
      "vat_rate": 0.05,
      "effective_from": "2026-10-01",
      "previous": [
        { "kind": "flat", "unit_rate": 0.2089, "daily_standing_charge": 0.5294,
          "vat_rate": 0.05, "effective_from": "2025-04-01",
          "effective_to": "2026-10-01" } ] } } }
```

`kind: flat` keeps every existing deployment working unchanged; `unit_rate` is simply
absent under `kind: agile` because the price comes from the archive. Local
`config.yaml` gains the Octopus base URL (overridable for tests) and the
`energy_prices` bucket plus its write token.

Per house rules, the same change updates `internal/httpapi/openapi.yaml` and
`README.md`, and the `spec_test.go` path-coverage test keeps the new `/prices*` routes
honest.

---

## 9. Suggested build order

| M | Deliverable |
|---|---|
| 1 | Confirm one live API response from garibaldi; confirm our GSP letter and switchover date. Create the `energy_prices` bucket (infinite retention) and its write token. No code. |
| 2 | `internal/octopus`: typed client — unit rates, standing charges, pagination, retry/backoff, `Retry-After`. Tested against an `httptest` server with recorded fixtures. No live calls in tests. |
| 3 | `internal/prices`: write path (idempotent line protocol) and read path (Flux → `[]Slot`), plus gap detection. Fake store for handler tests, mirroring `influx/fake.go`. |
| 4 | Collector: clock-driven scheduler, startup sweep, backfill mode, `/healthz` + `/metrics` surfaces. |
| 5 | Tariff effective-date history: `TariffFor(t)` becomes real, windows split at boundaries. **Prerequisite for the first Agile bill.** |
| 6 | Slot-resolution costing in `/bill` and `/devices/{id}/cost`; `MaxBuckets` bypass on the internal path; unpriced-energy reporting; `attribution` + `effective_rate` fields. |
| 7 | `/prices`, `/prices/next`, `/prices/cheapest`, `/prices/stats` + OpenAPI + README. |
| 8 | Optional: C2 power-shaped attribution; account-API consumption reconciliation. |

Milestones 1–5 must land before the first Agile bill is asked for. 6 is the payoff,
7 is the "shape our usage" half.

### Testing notes

Per CLAUDE.md, fake doubles and an injected clock throughout: an `httptest` Octopus
serving recorded JSON (including a 429 with `Retry-After`, a paginated response, and a
day with negative rates); `FakeClock` driving the 16:00 publication cycle, a late
publication, and a missed day healed by the sweep; table tests for slot alignment on
both DST changeover days (46- and 50-slot days), for a window spanning the
flexible→Agile switchover, and for the capped and negative price extremes.

---

## 10. Open decisions for the user

1. **A3 vs A4** — may countinghouse hold one narrow write path (recommended), or should
   a separate service own the archive?
2. **Our GSP region letter** and the **switchover date** to Agile.
3. **Backfill depth** — from our switchover, or the product's full history?
4. **Per-device attribution** — C1 (recommended), C2, or C3?
5. **Account API key** — do we want Octopus's settled consumption for reconciliation
   (§7), accepting a stored credential?
6. **Export** — is outgoing/export Agile (`AGILE-OUTGOING`) in scope now, or just
   left room for (as the `direction` tag does)?

---

## Sources

- Octopus Energy REST API docs — [endpoints](https://docs.octopus.energy/rest/guides/endpoints/),
  [API basics](https://docs.octopus.energy/rest/guides/api-basics/)
- [Agile Octopus product page](https://octopus.energy/smart/agile/) — publication time,
  cap, plunge pricing
- [`danopstech/octopusenergy`](https://github.com/danopstech/octopusenergy) — Go client;
  path templates, `page_size` limits, response structs
- [Guide to the Octopus API — Guy Lipman](https://www.guylipman.com/octopus/api_guide.html)
- [energy-stats.uk — Agile tariff pricing](https://energy-stats.uk/octopus-agile-tariff-pricing/)
