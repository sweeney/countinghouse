# Countinghouse — Unmonitored Consumption API

**Status:** Draft requirements
**Author:** (via Claude Code, with Martin)
**Date:** 2026-06-19
**Owning service:** `countinghouse` (read-side energy cost/accounting)
**Consumers affected:** `consumer-demos` (`countinghouse-index.html`,
`countinghouse-breakdown.html`), and any future client that wants whole-home
attribution.

---

## 0. Implementation status (2026-06-19)

| Item | Status |
| --- | --- |
| R1 — third `unmonitored` series on `group_by=house` | ✅ shipped |
| R1.6 / C12 — top-level `coverage` on house response | ✅ shipped |
| C13 — `stale_monitored_count` / `_ids` (no-power-telemetry definition) | ✅ shipped |
| R3 — synthetic `unmonitored` device (`/devices`, `/devices/unmonitored/series`) | ✅ shipped |
| R2 — `include_unmonitored=true` catch-all on device/location/class | ✅ shipped |
| C1/C2/C9 — per-bucket clamp before totals; tariff-applied cost | ✅ shipped |
| C8 — `unmonitored.avg_w` energy-derived (`kwh×1000/bucket_hours`) | ✅ shipped (fixed avg_w=0 bug, `docs/bug-unmonitored-avg-w.md`) |
| Q1, Q2, Q6 | ✅ resolved (see §11 / §6.4 / §2.5) |
| C3 / NF4 — drift metric (`drift_buckets_total`) + WARN log, 0.1 kWh threshold | ✅ shipped |
| Q4 — `unclamped=true` diagnostic mode (raw signed residual) | ✅ shipped (not hidden — consumers are us) |
| C10 — `/bill` reconciliation invariant (`unmonitored=meter−monitored`, coverage) | ✅ covered by `TestBill`; cross-endpoint `/bill`↔`/series` agreement ⬜ Phase 3 (live) |
| C11 — cross-service reconciliation observability signal | ⬜ Phase 3 |
| C7 / Q5 — per-bucket `null` gaps (series infra currently zero-fills) | ⬜ deferred |
| Q3 — reserved-id policy: shadowing implemented; hard config rejection | ⬜ open |

Everything that unblocks the two demos (R1 + R3) and the breakdown ergonomics
(R2) plus the confidence signals (coverage/staleness) is live. Remaining items are
hardening/observability and two narrow edges (null gaps, hard id reservation).

---

## 1. Summary

Countinghouse meters individual devices (plug/circuit level) **and** has a
whole-house meter (`electricity_meter`, class `energy_meter`). The difference
between the two — energy the house consumed that no individual device accounts
for — is currently **not exposed by the API**. We call this **unmonitored
consumption** ("rest of home").

Clients can only obtain it by fetching the whole-house total and the monitored
total separately and subtracting them in the browser. This document specifies
making **unmonitored consumption a first-class, server-computed quantity** so
that no client has to derive it.

The driving principle: the service owns the source of truth (tariff, the device
counter quantisation, bucket/DST alignment, where to clamp the residual). The
definition of "unmonitored" must live there too, computed once, consistently,
for every consumer.

---

## 2. Background & current behaviour

The following reflects the **live API as observed on 2026-06-19** against
`https://countinghouse.swee.net`.

### 2.1 Relevant endpoints

| Endpoint | Purpose |
| --- | --- |
| `GET /series` | Multi-series energy time-series over a window |
| `GET /devices` | Device catalogue (id, class, capabilities) |
| `GET /devices/{id}/series` | Single device's time-series (`avg_w`, `kwh`, `cost`) |
| `GET /bill` | Billing summary |
| `GET /tariffs` | Tariff data |

### 2.2 `GET /series` grouping (today)

`group_by` accepts: `device` (default), `location`, `class`, `house`.

Per the current OpenAPI description:

> `group_by` selects **device** (default, one series per metered device,
> **excluding the whole-house meter**), **location**, **class**, or **house**
> (two series: `"monitored"` = sum of devices, `"meter"` = the whole-house
> meter).

So the whole-house meter is *deliberately excluded* from `device`/`location`/
`class` groupings, and `house` is the only mode that surfaces it — as two
series, `monitored` and `meter`.

### 2.3 The gap, with live numbers

`GET /series?window=24h&group_by=house&interval=6h` returned:

| series | `total_kwh` | `total_cost` |
| --- | --- | --- |
| `monitored` (Σ devices) | 16.888 | £3.7042 |
| `meter` (whole-house) | 28.956 | £6.3513 |

The implied **unmonitored** quantity is `meter − monitored`:

```
unmonitored_kwh  = 28.956 − 16.888 = 12.068 kWh   (~41.7% of the home total)
unmonitored_cost = 6.3513 − 3.7042 = £2.6471
```

Roughly **42% of the home's energy in that window is unattributed** — a material
quantity that the breakdown chart currently renders as if it didn't exist, and
that the index chart cannot plot at all.

### 2.4 Why client-side derivation is the wrong home for this

1. **Duplication / drift.** Each demo (and each future client) re-implements the
   subtraction, the negative-residual clamping, and the rounding. Two clients can
   disagree on what "unmonitored" means.
2. **Two round-trips + client math** for something the server already holds both
   halves of.
3. **Server-only knowledge.** Only countinghouse knows the tariff, the device
   counters' **0.1 kWh quantisation** (already worked around in
   `countinghouse-index.html` by plotting cumulative energy), DST/bucket
   alignment, and the correct place to clamp `meter − Σdevices` when it goes
   slightly negative.
4. **Undiscoverable.** Nothing in `/openapi.json` advertises that unmonitored
   consumption is obtainable. A first-class field is self-documenting.

### 2.5 Relationship to statehouse `house_electricity`

Statehouse already publishes a continuously-computed house-level decomposition in
the `house_electricity` measurement (tag `scope=whole_house`):

| Group | Fields | Notes |
| --- | --- | --- |
| Instantaneous power | `gross_w`, `monitored_w`, `unmonitored_w`, `coverage` | `unmonitored_w = gross_w − monitored_w` |
| Rolling energy | `today_kwh`, `week_kwh`, `month_kwh` | calendar-aligned, cumulative |
| Session totals | `session_gross_kwh`, `session_monitored_kwh`, `session_unmonitored_kwh` | **reset on statehouse restart** |
| Health | `stale_device_count` | monitored-device staleness signal |

**Terminology mapping.** statehouse's `gross_w` is the whole-house total — the
**same physical feed** as countinghouse's `meter` (the `electricity_meter`
device) — differing only in derivation: statehouse *integrates instantaneous
power* (`gross_w`), countinghouse *reads the meter's energy counter register*.
Likewise statehouse `monitored_w` ≡ countinghouse `monitored`. So `gross ≡ meter`,
and the two "unmonitored" residuals are the **same quantity computed two ways**,
not two different measurements.

> ✅ **Feed identity confirmed (Q6, 2026-06-19).** `electricity_meter` writes
> `power_w` *and* `energy_kwh` (same CT, same cadence — ~6030 points/24h each).
> statehouse's `gross_w` tracks `electricity_meter.power_w` to **<1% in steady
> buckets** (−0.05% to −0.65% measured); the only large deviations are 5-min
> averaging artifacts when power swings mid-bucket, i.e. sampling-timing skew, not
> a second sensor. So `gross ≡ meter` is a genuine identity: the same physical feed
> derived two ways — statehouse integrates `power_w`, countinghouse reads the
> `energy_kwh` counter register. The C11/C5a band is therefore bounded by the
> **counter-vs-∫power integration bias alone**, not by independent-clamp drift.

So "unmonitored" is **not a new concept** — statehouse computes the same
`gross − monitored` residual live. Countinghouse nonetheless **re-derives it from
its own counter inputs** (per §3) rather than reading statehouse's fields, for
three reasons:

1. **Arbitrary windows.** statehouse's energy is calendar-aligned
   (`today/week/month_kwh`) and `session_*` resets on restart. Countinghouse's
   purpose is *arbitrary* windows (`?window=…`, custom `from`/`to`), which these
   fixed buckets cannot serve.
2. **Counter authority.** statehouse's energy fields are *integrated from
   instantaneous power*, which is biased relative to the **counter registers** on
   both sides — the whole-house meter counter and the per-device `energy_kwh`
   counters (the 0.1 kWh-quantised hardware totals). Measured 2026-06-19:
   statehouse whole-house `week_kwh` ≈ 89.9 vs the **meter counter** increase
   ≈ 89.3 over the same window (~0.7% high). For **billing**, the counter is
   authoritative — and §3 sources both the meter and the device counters
   accordingly.
3. **Tariff.** Only countinghouse holds the tariff, so only it can attach cost.

**Cross-check (not a dependency).** Because both services compute the same
quantity from the same feed, countinghouse's windowed `unmonitored_kwh` SHOULD
reconcile with statehouse's `house_electricity` over an equivalent window —
formalized as **C11**, bounded by the **cross-service reconciliation tolerance**
of **C5a** (a percentage band that absorbs the counter-vs-∫power bias above —
*not* the rounding tolerance C5). This is a data-quality *monitoring* assertion:
it is emitted as an observability signal (NF4), **never** asserted in CI (a
statehouse restart must not fail countinghouse's build) and **never** a runtime
dependency — countinghouse owns its definition and survives statehouse not writing
`house_electricity` at all.

---

## 3. Definitions

| Term | Definition |
| --- | --- |
| **Monitored** | The sum, per time bucket, of all individually metered devices (capability `energy`), excluding the whole-house meter. |
| **Meter** | The whole-house `energy_meter` reading (`electricity_meter`) — authoritative total household consumption. |
| **Unmonitored** | `meter − monitored`, per bucket and in total. Energy the house consumed that no individual monitored device accounts for ("rest of home"). |
| **Residual clamp** | Replacing a slightly-negative `unmonitored` bucket value with `0` (see §6.1). |
| **Bucket** | One interval-sized time slot on the series axis (e.g. a 6h slot). |

**Core invariant (must hold for every bucket and for the window totals):**

```
monitored + unmonitored == meter        (after clamping, within rounding tolerance)
```

---

## 4. Goals & non-goals

### 4.1 Goals

- G1. Expose unmonitored consumption (energy **and** cost) server-computed, both
  as a **time-series** (per bucket) and as **window totals**.
- G2. Let clients plot unmonitored over time **exactly like an individual
  device**, with no special-case client code (the `countinghouse-index.html`
  use-case).
- G3. Let the stacked/pie breakdown include unmonitored as a normal slice/series
  such that the parts **sum to the whole-house meter** (the
  `countinghouse-breakdown.html` use-case).
- G4. Guarantee the §3 invariant on the server, with consistent rounding and a
  single, documented clamping rule.
- G5. Be discoverable in `/openapi.json` and backward-compatible (no breaking
  changes to existing responses).

### 4.2 Non-goals

- N1. **Sub-attributing** unmonitored to rooms/classes/devices. Unmonitored is by
  definition unattributed; it is always a single catch-all series. It must **not**
  be split across `location`/`class` buckets.
- N2. Changing how individual devices or the meter are measured.
- N3. Real-time/streaming semantics beyond what `/series` already offers.
- N4. Backfilling or correcting historical meter/device data.

---

## 5. Functional requirements

The proposal has three independent surfaces. They share one server-side
computation and can ship incrementally. **R1 and R3 are the priority** (they
unblock the two existing demos); R2 is the most ergonomic for breakdown clients.

### R1 — Third series on `group_by=house`

`GET /series?group_by=house` MUST return a **third series** with key
`unmonitored`, alongside `monitored` and `meter`.

- R1.1 — `unmonitored` carries the same per-bucket fields as the other series in
  the chosen `shape` (columnar: `avg_w[]`, `kwh[]`, `cost[]`; rows: one row per
  bucket with `kwh`, `cost`, `avg_w`), plus `total_kwh` / `total_cost`.
- R1.2 — For every bucket: `monitored + unmonitored == meter` after clamping
  (§6.1), within the rounding tolerance of §6.2.
- R1.3 — Window totals: `monitored.total_kwh + unmonitored.total_kwh ==
  meter.total_kwh` (and likewise for cost), within tolerance.
- R1.4 — Series ordering SHOULD be `monitored`, `unmonitored`, `meter` so that a
  naive client stacking the first N−1 series (excluding `meter`) reconstructs the
  whole home.
- R1.5 — `label` SHOULD be human-readable, e.g. `"Unmonitored"` /
  `"Rest of home"`.
- R1.6 — The response SHOULD include a top-level `coverage` figure
  (`monitored / meter` for the window) — see C12 — so consumers can gauge how
  trustworthy the unmonitored split is.

**Example (rows shape):**

```http
GET /series?window=24h&group_by=house&interval=6h&shape=rows
```

```jsonc
{
  "window": "24h", "group_by": "house", "shape": "rows", "interval": "6h",
  "from": "2026-06-18T13:34:43+01:00", "to": "2026-06-19T13:34:43+01:00",
  "coverage": 0.583,                 // monitored/meter = 16.888/28.956 (R1.6/C12)
  "series": [
    { "key": "monitored",   "label": "Monitored",   "total_kwh": 16.888, "total_cost": 3.7042 },
    { "key": "unmonitored", "label": "Unmonitored", "total_kwh": 12.068, "total_cost": 2.6471 },
    { "key": "meter",       "label": "Meter",       "total_kwh": 28.956, "total_cost": 6.3513 }
  ],
  "rows": [
    { "key": "monitored",   "time": "2026-06-18T12:00:00+01:00", "kwh": 4.691, "cost": 1.0289, "avg_w": 2394.0 },
    { "key": "unmonitored", "time": "2026-06-18T12:00:00+01:00", "kwh": 3.34,  "cost": 0.73,   "avg_w": 1705.0 },
    { "key": "meter",       "time": "2026-06-18T12:00:00+01:00", "kwh": 8.031, "cost": 1.7589, "avg_w": 4099.0 }
    // … one triple per bucket
  ]
}
```

### R2 — Optional `unmonitored` bucket in `device` / `location` / `class` groupings

`GET /series?group_by=device|location|class` SHOULD support opting the unmonitored
catch-all into the result so the parts sum to the whole house.

- R2.1 — Gated by a query parameter, default **off** for backward compatibility.
  Proposed: `include_unmonitored=true` (boolean). (Alternative spelling
  `include=unmonitored` is acceptable; pick one and document it.)
- R2.2 — When enabled, the response gains exactly **one** extra series with key
  `unmonitored` (regardless of grouping — never subdivided, per N1).
- R2.3 — The extra series carries the same shape/fields as the grouping's other
  series, so existing client rendering code treats it as just another series.
- R2.4 — Invariant when enabled: `Σ(all grouped series) + unmonitored == meter`
  per bucket and in total, within tolerance.
- R2.5 — When disabled, the response is **byte-for-byte** the behaviour of today
  (no new series, no field changes).
- R2.6 — The synthetic series MUST be distinguishable from a real device, e.g.
  `class: "unmonitored"` / `location: null`, and an id that cannot collide with a
  real device id (recommend reserving `unmonitored`).

### R3 — Synthetic device `unmonitored` for the single-series path

To satisfy the "plot it like a device" use-case (G2) with **zero** client
branching, unmonitored MUST be addressable through the single-device endpoint
shape.

- R3.1 — `GET /devices/unmonitored/series?window=…&interval=…` MUST return the
  **same response schema** as `GET /devices/{id}/series` for a real device:
  `buckets[]`, and a `series[0]` (or equivalent) carrying `avg_w[]`, `kwh[]`,
  `cost[]`, `total_kwh`, `total_cost`, `label`.
- R3.2 — Values equal the R1 `unmonitored` series for the same window/interval.
- R3.3 — `unmonitored` SHOULD appear in `GET /devices` as a synthetic entry so it
  shows up in client device pickers automatically:
  - `id: "unmonitored"`, `label: "Unmonitored (rest of home)"`,
    `class: "unmonitored"`, `capabilities: ["energy"]`, `location: null`.
  - This is the single change that makes `countinghouse-index.html` a **one-line**
    update (the device dropdown already filters on the `energy` capability).
- R3.4 — Endpoints that are meaningless for a synthetic device (e.g.
  `/devices/unmonitored/intervals`, `/events`) SHOULD return `404` or an empty,
  well-typed payload — not a 500. Document which.
- R3.5 — `id: "unmonitored"` MUST be a reserved id that real devices can never
  take.

---

## 6. Computation, edge cases & invariants

### 6.1 Negative residual (clamping)

`meter − Σdevices` can be **slightly negative** in a bucket due to:
- the devices' **0.1 kWh counter quantisation** vs the meter's finer resolution,
- sampling/timing skew between device reads and the meter read,
- rounding at bucket boundaries.

Requirement:
- C1. Per bucket, `unmonitored = max(0, meter − monitored)`.
- C2. Clamping MUST be applied **per bucket before** computing window totals, so
  `total_unmonitored = Σ clamped buckets` (not `total_meter − total_monitored`,
  which could differ once buckets are clamped).
- C3. If clamping materially changes a bucket (configurable threshold, e.g. the
  negative residual exceeds one counter quantum, 0.1 kWh), the server SHOULD emit
  a structured log/metric so we can detect meter/device drift. This is the kind of
  data-quality signal that does **not** belong in a browser.

### 6.2 Rounding & tolerance

- C4. Apply the **same rounding** already used for device/house series (energy to
  3 dp kWh, cost to 4 dp as observed) so unmonitored visually matches its
  siblings.
- C5. **Internal invariant tolerance (rounding).** The *internal* invariant checks
  (R1.2/R1.3/R2.4 — `monitored + unmonitored == meter` within a single response)
  hold within **±1 unit in the last published decimal place**. This absorbs decimal
  rounding only; the inputs are literally the same numbers, so the band is tiny.
- C5a. **Cross-service reconciliation tolerance (percentage band).** The
  *cross-service* check against statehouse (C11) is a different and much larger
  tolerance. It must absorb the counter-vs-∫power integration bias (≈0.7% measured
  2026-06-19, §2.5), so express it as a **percentage band** (recommend **±2%** of
  the window total, tuned against observed drift) — **never** the ±1-ulp band of
  C5. Conflating the two guarantees one of the checks is set wrong.

### 6.3 Missing / unavailable inputs

- C6. **No whole-house meter** configured (or no meter data in the window):
  `unmonitored` is **undefined**, not zero. The server MUST omit the unmonitored
  series (R1/R2) and return `404` for R3, rather than reporting the monitored sum
  as if it were the whole home. Document this explicitly.
- C7. **Meter present but a bucket has a gap** (null meter or null monitored for
  that bucket): that bucket's `unmonitored` SHOULD be `null` (a gap), not `0`, so
  clients can `spanGaps`/skip it rather than draw a misleading floor.
- C8. `avg_w` for unmonitored is derived consistently with how the other series
  derive average power from bucket energy and bucket duration (so a partial first/
  last bucket isn't over/under-stated).

### 6.4 Cost

- C9. Unmonitored **cost** is computed from unmonitored **energy** against the same
  tariff/methodology the meter uses for the same buckets — it MUST NOT be derived
  as `meter_cost − monitored_cost` if that disagrees with applying the tariff to
  `unmonitored_kwh`. **Authoritative basis: tariff-applied-to-energy** (resolves
  Q2). Rationale: the clamping rule (C1/C2) operates on per-bucket *energy*, so
  cost must be taken from the same clamped `unmonitored_kwh` to keep the §3
  invariant and `/bill` reconciliation (C10) exact; `meter_cost − monitored_cost`
  would re-introduce the unclamped residual and can disagree at bucket boundaries.

### 6.5 Consistency with `/bill`

- C10. If `/bill` reports a whole-home figure, the sum of monitored + unmonitored
  over the billing window MUST reconcile with it (within the C5 internal
  tolerance). Add a test asserting this.

### 6.6 Coverage, staleness & cross-service reconciliation

- C11. **Cross-service reconciliation (monitoring, not a gate).** Countinghouse's
  windowed `unmonitored_kwh` SHOULD reconcile with statehouse's `house_electricity`
  over an equivalent window, within the **C5a** percentage band. Emitted as an
  observability signal (NF4); **not** asserted in CI and **never** a runtime
  dependency (§2.5).
- C12. **Coverage field.** The `group_by=house` response (R1.6) SHOULD include a
  `coverage` figure — `monitored / meter` for the window (and MAY include it per
  bucket) — mirroring statehouse's `coverage`. This lets a consumer distinguish
  "this much of the home is *genuinely* unmonitored" from "monitored is
  under-counted because a sensor dropped out," without a second service call.
- C13. **Staleness sensitivity (interpretation caveat).** `unmonitored` is
  **inflated by stale/dropped monitored devices**: a device that stops reporting
  removes its load from `monitored`, and that load reappears in `unmonitored`
  (= meter − monitored) even though it is not genuinely unattributed. This is the
  monitored-side analogue of the C3 drift signal. The server SHOULD surface a
  staleness indicator alongside the decomposition (e.g. a count of stale monitored
  devices in the window, cf. statehouse `stale_device_count`) so consumers — and
  the demos — can flag low-confidence unmonitored figures rather than present them
  as fact.

---

## 7. Non-functional requirements

- NF1. **Backward compatibility.** Existing requests (no new params) return
  unchanged responses. New series/devices appear only when explicitly requested
  (R2 behind a flag; R1 adds a series only to `group_by=house`, which is an
  additive change — confirm no client hard-asserts exactly two house series; the
  demos do not).
- NF2. **Performance.** Unmonitored reuses the already-computed monitored sum and
  meter series — it is one vector subtraction + clamp per request. No additional
  storage I/O beyond what `group_by=house` already does.
- NF3. **Discoverability.** `/openapi.json` MUST document the new series key, the
  `include_unmonitored` param, the synthetic device, the `coverage` and staleness
  fields (C12/C13), the `meter == monitored + unmonitored` invariant, and the
  clamping rule.
- NF4. **Observability.** Emit a metric for clamped/negative-residual buckets
  (C3) and for requests where the meter is missing (C6).
- NF5. **Determinism.** Same inputs ⇒ same outputs; no client-clock dependence.

---

## 8. Recommended phasing

1. **Phase 1 (unblocks both demos):** R1 (third `house` series) + R3 (synthetic
   `unmonitored` device & `/devices/unmonitored/series`). R3.3 makes
   `countinghouse-index.html` a one-line change; R1 powers the breakdown's
   "rest of home" slice.
2. **Phase 2 (ergonomics):** R2 (`include_unmonitored` on
   `device`/`location`/`class`) so breakdown clients get a self-summing stack
   without a second request.
3. **Phase 3 (hardening):** C3/NF4 drift metrics, C10 `/bill` reconciliation
   tests.

---

## 9. Acceptance criteria

A reviewer should be able to verify all of the following against a running
countinghouse:

- AC1. `GET /series?group_by=house&window=24h&interval=6h&shape=rows` returns
  three series `monitored`, `unmonitored`, `meter`; for every bucket
  `monitored.kwh + unmonitored.kwh == meter.kwh` within tolerance; same for cost
  and for window totals.
- AC2. With the live 2026-06-19 figures (or equivalent), `unmonitored.total_kwh`
  ≈ `meter.total_kwh − monitored.total_kwh` after per-bucket clamping (≈12.07 kWh
  in the worked example), **not** a naive total-level subtraction.
- AC3. `GET /devices` includes a synthetic `unmonitored` device with capability
  `energy`.
- AC4. `GET /devices/unmonitored/series?window=24h&interval=6h` returns the same
  schema as a real device and its values match AC1's `unmonitored` series.
- AC5. `GET /series?group_by=device&include_unmonitored=true` adds exactly one
  `unmonitored` series and `Σ(series) == meter` per bucket; the same request with
  the flag absent is identical to today's response.
- AC6. `group_by=location` / `group_by=class` with `include_unmonitored=true`
  still produce exactly **one** unmonitored series (never subdivided).
- AC7. A constructed bucket where `Σdevices > meter` yields `unmonitored == 0`
  (clamped) and increments the drift metric.
- AC8. A window with no meter data omits unmonitored (R1) and 404s the synthetic
  device path (R3), rather than presenting monitored as the whole home.
- AC9. `/openapi.json` documents all of the above, including the invariant and
  clamping rule.
- AC10. `group_by=house` includes a `coverage` figure equal to
  `monitored.total_kwh / meter.total_kwh` for the window (≈0.58 in the worked
  example) plus a stale-monitored-device indicator (C12/C13).
- AC11. The statehouse cross-service reconciliation (C11) is exposed as an
  observability signal within the C5a percentage band, and is **not** a
  CI-asserted test — a statehouse restart cannot fail countinghouse's build.

---

## 10. Impact on `consumer-demos` once shipped

- `countinghouse-index.html` — `unmonitored` appears in the device dropdown (R3.3)
  and plots through the **existing** single-device code path. Expected change:
  effectively none beyond it showing up; optionally a friendlier label/colour.
- `countinghouse-breakdown.html` — add `include_unmonitored=true` (R2) or stack
  the R1 `unmonitored` series; the pie/doughnut "share of whole home" view becomes
  correct, and the recently-added descending-order sort will naturally place the
  (large) unmonitored slice first. No client-side `meter − monitored` math, no
  client-side clamping.
- **Confidence signalling** — both demos SHOULD read `coverage` and the stale-
  device indicator (C12/C13) and visually caveat the unmonitored figure when
  coverage is low, rather than presenting a sensor dropout as genuine "rest of
  home."

---

## 11. Open questions

- ~~Q1. Param spelling: `include_unmonitored=true` vs `include=unmonitored`
  (extensible to future synthetic series)? Recommend the latter if more catch-all
  series are foreseeable.~~ **Resolved: `include_unmonitored=true`** (boolean,
  default false; parsed with Go `strconv.ParseBool`). YAGNI on the extensible
  `include=…` parser until a second synthetic series actually exists.
- ~~Q2. Authoritative unmonitored **cost** basis — tariff-applied-to-energy vs
  `meter_cost − monitored_cost` (§6.4). Which reconciles with `/bill`?~~
  **Resolved (§6.4/C9): tariff-applied-to-energy**, so cost tracks the clamped
  per-bucket energy and reconciles with `/bill`.
- Q3. Reserved id collision policy — is `unmonitored` already a safe reserved id,
  or do we need a namespacing convention (e.g. `__unmonitored__`) for synthetic
  devices?
- ~~Q4. Should `unmonitored` ever be **negative-allowed** in a diagnostic mode
  (unclamped) for data-quality investigation, behind a debug flag?~~ **Resolved:
  yes — `unclamped=true` on `/series` and `/devices/unmonitored/series`. Not
  hidden behind a debug gate (the only consumers are the household), but default
  off so normal responses stay clamped.**
- Q5. Gap policy confirmation (C7): is `null` the right per-bucket representation
  for missing inputs in both shapes, and do existing clients handle it?
- ~~Q6. Feed identity (§2.5): is statehouse's `gross_w` derived from the **same**
  `electricity_meter` CT as countinghouse's meter counter, or an independent clamp?
  Determines how tight the C11/C5a reconciliation band can realistically be.~~
  **Resolved (§2.5, 2026-06-19): same feed.** `electricity_meter` writes both
  `power_w` and `energy_kwh`; `gross_w` matches `electricity_meter.power_w` to <1%
  in steady buckets. The C11/C5a band is bounded by integration bias alone (~0.7%
  measured), so the recommended **±2%** in C5a is comfortable — it could tighten
  toward ±1% on long windows, but ±2% absorbs short-window/partial-bucket skew.
