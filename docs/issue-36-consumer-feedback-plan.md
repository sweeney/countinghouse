# Issue #36 — plan for the consumer feedback

Issue: https://github.com/sweeney/countinghouse/issues/36

A consumer spent a session against this API trying to answer *"has household load
shifted in response to half-hourly prices, and is the half-hourly tariff beating the
fixed alternative?"* — and wrote up where the API carried them and where it did not.
A follow-up comment verified every claim from outside the service, using a fixture
served behind the real mux, and proposed concrete shapes.

This document is the **plan**, not the build. It records what was checked against the
code, where the issue's own diagnosis needs correcting, how the work splits into pull
requests, and which decisions are settled versus still open.

Nothing here is implemented yet.

---

## 1. What was verified against the code

Three claims underpin most of the plan, so they were checked rather than assumed.

**The harness branch is one clean commit.** `claude/focused-cannon-ix0xla` sits
directly on current `main` — 677 lines of `internal/httpapi/consumer_harness_test.go`
plus nine consumer scripts under `scripts/consumers/`, and nothing else. (A stale
remote-tracking ref makes it look like a 33k-line diff; it is not.) Checked out against
`main`: `go vet` clean, `TestConsumerFixtureIsPlausible` green in 0.6s, and
`TestConsumerHarness` correctly gated behind `CH_HARNESS=1` so `make test` never
stands up a socket.

**P1 (`prices=true`) is cheaper than the issue estimates.** `energy.Pricer` already
answers `RateAt(t) (rate, known)`, and `energy.Granularity` already answers
`RateInterval()`. A `prices[]` array is literally `RateAt` per bucket, and the
`slot` versus `mean_over_bucket` distinction is `RateInterval()` compared against the
bucket width — a comparison `CostBuckets` and `CostingInterval` already reason about.
This is not new design; it is exposing a decision the cost path already makes.

**P6b is plumbing.** `config.NamespaceStatus.FetchedAt` is already populated by
`recordStatus` and already reaches `/healthz`. `handleTariffs` simply does not read it:
it emits `currency`, `source` and `agreements`, and stops.

---

## 2. Three corrections to the issue

### 2.1 N8's diagnosis is wrong, and the obvious fix is wrong too

The follow-up attributes the room-shares gap (£61.94 of shares against a £61.92 meter)
to "per-series rounding". It is not rounding.

`withUnmonitoredCatchAll` computes `clamp(meter — monitored)` **per bucket**. In any
bucket where monitored exceeds meter, the negative residual is zeroed instead of
cancelling against the buckets that run the other way. The shares therefore *exceed*
the meter by exactly the sum of the clamped residuals — which matches the observed
direction. Money is already carried at 4dp (`round.MoneyDP = 4`), so accumulated
rounding across ~960 buckets cannot produce a 2p gap; 2p at a typical inc-VAT rate is
~0.13 kWh, about one counter quantum of clamped residual.

It also breaks a **documented promise**. `internal/energy/series.go:655` states the
catch-all exists "so a stacked chart of the grouping plus this catch-all sums to the
whole-house meter (R2.4)", and the README repeats it. That is a different class of
finding from float noise.

**The obvious fix does not work.** The first instinct — "`DriftStats` already measures
this, stop hiding it behind `json:"-"`" — fails twice:

- **It sees only half the clamping.** `computeDrift` counts a bucket only when
  `resid < -driftQuantumKWh`, and `driftQuantumKWh = 0.1`. Residuals between 0 and
  −0.1 kWh are clamped *silently and counted nowhere* — the code comment calls them
  "routine quantisation/sampling noise". On a gap this small those silent sub-quantum
  clamps are the more likely contributor, and they are precisely the half `DriftStats`
  was designed not to report.
- **It measures incidence, not magnitude.** `ClampedBuckets` is a count,
  `WorstResidualKWh` the single worst, `WorstAt` when. There is no total, so even for
  flagged buckets the size of the gap cannot be reconstructed. Nothing in the service
  currently knows how big this is — not just nothing on the wire.

The two clamps exist for different reasons: the sub-quantum one is deliberate noise
suppression that works correctly, the supra-quantum one is a data-quality alarm.
Collapsing them into one consumer-facing signal would misreport both.

Rounding money to pence would hide the discrepancy while leaving the invariant broken,
which is the one option to rule out.

### 2.2 `prices[]` needs a contract the proposal does not state

`/prices` and its family speak **pence, VAT-inclusive**. `/series.cost` speaks
**pounds, VAT-inclusive**. Whichever unit `prices[]` takes, the identity

```
cost[i] == kwh[i] × prices[i]
```

holds **only when `price_basis == "slot"`**. At coarse intervals cost is accumulated at
slot resolution while the reported price is a *time-weighted mean over the bucket*, so
the identity is deliberately false there. If that is not written into the spec it
becomes the next N2: an arithmetic relationship consumers will assume, that silently
does not hold on exactly the queries where they stop checking.

The contract to publish is:

- `len(prices) == len(buckets)`, always.
- `cost[i] == kwh[i] × prices[i]` **iff** `price_basis == "slot"`.
- `null` in `prices[]` means *no rate is held*, never *free*. `unpriced_buckets` counts
  them so `0` is a positive assertion of completeness.

### 2.3 N7's ETag asymmetry has a legitimate reason

`/prices` can carry an `ETag` because it has a *semantic* fingerprint: a fixed set of
archived slots, plus the current slot. `/series` and `/bill` derive from Influx data
that moves continuously; there is no honest strong validator short of hashing the
response body.

So the answer for those two is `Cache-Control` only, and the spec should say why.
Promising an `ETag` there would mean shipping either a weak validator or a lie, and a
polling budget-tracker is served perfectly well by `max-age`.

### 2.4 Two things the issue corrects about itself, both landing in docs

The follow-up notes the README *does* document the temporary zero rate of VAT (in the
runbook), and *does* document the 31/366 day caps. Both corrections stand. That moves
those items from "missing content" to **discoverability** — the reader never reached the
runbook from the pricing sections. The fix is cross-links, not new prose.

### 2.5 N2 is not a server bug

The 50% silent energy loss at `interval=15m` is the *consumer's* join failing in the
absence of P1. It is the strongest available argument **for** P1, and belongs in that
PR's motivation, not in the bug track.

---

## 3. Triage

| Class | Findings |
|---|---|
| **Docs only** | README `/bill` parameter column; negative `cost` sentence; VAT-runbook cross-link; caps cross-reference |
| **Behaviour bugs** | N8 clamp breaks R2.4; N5 same window `200` on `/prices` but `503` on `/prices/stats` |
| **Additive API** | P1 `prices=true`; P5 bill scope; P6b `fetched_at`/`stale`; P7 `limits`; P2 `/compare`; P4 stats `group_by`; P3a/b archive by code; N7 `Cache-Control` |
| **Internal hardening** | P8 config-rate gates |

---

## 4. Decisions taken

| # | Decision | Rationale |
|---|---|---|
| D1 | **`prices[]` in £/kWh**, with `price_unit` stating it | Self-consistency inside one payload beats consistency with a different endpoint: the array sits beside `cost[]` and `kwh[]`, and `cost ≈ kwh × price` should read directly. A consumer porting from the `/prices` join sees a 100× shift, which is loud, not silent — and `price_unit` names it. |
| D2 | **P3 read-side only** (`/prices/tariffs` + `?tariff_code=`); no collector backfill | The read side is useful the day this site switches tariff and wants its own history reachable by code, and costs no storage or Octopus API budget. Backfilling codes the site is not on has a real cost and is deferred — see Q3. |
| D3 | **A/B/C track split**, bug fixes separate from new API surface | Requested, and right: a reviewer reading a bug fix should not have to review new endpoints in the same diff. |
| D4 | `household_total` **top-level** on `/bill`, not buried in `reconciliation` | Both fields are new, so top-level placement cannot threaten `total`'s existing meaning; `scope` is what makes `total` self-describing. Burying the real figure is how it got missed in the first place (N3: `/bill.total` understates by 55% on a half-covered home). |
| D5 | `scope` on `/compare` **required, no default** | N4 showed the same window and the same question yielding −0.4% or −31.8% depending on scope, supporting opposite conclusions. A default picks one silently, once, for everybody, forever. |
| D6 | `prices[]` **populated across a tariff switchover**, with `tariff_codes[]` | The one-curve-per-response rule exists because a *curve* is a property of one tariff. A per-bucket array is not a curve: each bucket belongs to exactly one tariff, so every value is honest. `/series` already sums cost across tariffs. |

---

## 5. Pull request plan

Ordered. Each Track C PR carries `internal/httpapi/openapi.yaml` **and** `README.md` in
the same change, per the house rules. Each Track B PR is TDD: failing test first,
confirmed red, then green.

### Track A — foundation

**A1 · Land the consumer harness.**
Cherry-pick `17f5d90` as-is. One fixture home behind the real mux on a real socket,
plus nine consumers that talk to it over HTTP and nothing else.

*Why first:* it is the evidence base. Every PR below gets a consumer-level regression
test — "does this shape actually work from outside" — rather than only a unit test that
shares the server's assumptions. It is also the only vantage point from which the
findings above were reproducible.

*Acceptance:* `make test` unchanged in runtime (harness stays skipped);
`TestConsumerFixtureIsPlausible` green in CI.

### Track B — bugs and docs

**B1 · Docs only.** No code.
- README `/bill` row: `GET /bill?window=month` → `GET /bill?window=&from=&to=`,
  matching how every other row in that table is written. This one column cost the
  reporter roughly a third of their analysis code.
- One sentence that negative prices yield negative `cost`, and that
  `sum(abs(cost))` / `max(0, cost)` / log scales are wrong there.
- Cross-link the VAT runbook from the pricing sections.
- Cross-reference the window caps where a caller meets them.

**B2 · The two behaviour bugs.**
- **N8**: per §2.1 the cause is the per-bucket clamp, and `DriftStats` cannot be
  reused as-is. Preferred shape is to accumulate the **clamped kWh total**, reported as
  two bands so the sub-quantum noise and the supra-quantum alarm stay distinguishable,
  surfaced on the `group_by=house` response. R2.4's wording changes either way. The
  alternative of redistributing the residual so the identity holds exactly is rejected:
  it trades a visible 2p for an invisible per-bucket fudge, which is the trade this
  service refuses everywhere else.
- **N5**: `/prices/stats` should mirror `/prices` for a flat-tariff window — `200` with
  the flat shape rather than `503`. The same window answering `200` on one endpoint and
  `503` on its neighbour forces a seasonal-analysis consumer to special-case per
  endpoint instead of per window.

### Track C — API improvements

**C1 · P1 `prices=true`** on `/series` and `/devices/{id}/series`.
Opt-in (`false` default) so no existing payload grows. `prices[]` in £/kWh (D1),
`price_unit`, `price_vat_included`, `price_basis` (`slot` | `mean_over_bucket` |
`flat`), `unpriced_buckets`. Time-weighted mean at coarse intervals, **not**
energy-weighted — energy-weighted is `cost/kwh`, undefined in a zero-kWh bucket, and
those are exactly the buckets that answer *"it was cheap and we did not use it"*.
Populate across a switchover with `tariff_codes[]` (D6). Publish the contract in §2.2.

**C2 · P5 `/bill` scope.** Additive only — **`total` does not change**. Adds `scope`,
plus `unmonitored_cost`, `household_energy_cost` and top-level `household_total` (D4).
The unmonitored quantity is already priced per bucket for `/series?group_by=house`;
routing it through the same pricer is what makes it impossible for the two to disagree.

**C3 · P6b + P7** — two small response-metadata changes, grouped.
- `fetched_at` and `stale` on `/tariffs`, `/floors`, `/rooms`, mirroring `/healthz`.
  `stale` matters more than `fetched_at` given "boot needs truth, running keeps the last
  truth": a consumer can be served an arbitrarily old successful snapshot while
  `/healthz` merely degrades. One boolean makes that visible at the point of use.
- `available_now` per agreement, derived from its dates against the injected clock.
- A machine-readable `limits` block on cap refusals, carrying `max_window_seconds`
  so the bucket-stated `/series` cap and the day-stated `/prices` cap can be reconciled
  by one chunking routine. The prose messages stay — they are good — this makes them
  *also* data.

**C4 · P2 `/compare`.** Depends on C1, C2, and C3's `available_now`.
Repeatable `alt=`, required `scope` (D5), and a `delta` broken into
`energy` / `standing` / `total` with `delta.energy + delta.standing == delta.total`
asserted in tests. Both of the reporter's self-described baseline errors become
structurally inexpressible: an expired baseline is a labelled `available_now: false`
rather than a silence, and the standing-charge difference has its own field that the
totals will not reconcile without. `kind` in build order: `flat`, `tariff`,
`window_mean`, then `shape` last (it needs a stated alignment rule).

**C5+ · Later phase**, each self-contained: P4 (`/prices/stats?group_by=month`),
P3a/b (archive coverage discovery and read-by-code, per D2), P8 (config rates through
`internal/prices/validate.go`'s existing Gate A/B/C model), and N7 (`Cache-Control` on
`/bill` and `/series`, per §2.3).

---

## 6. Still open

**Q1 — P8 and the VAT-basis gap.** The issue correctly notes a plausibility band
**cannot** catch a 5% wrong-basis error: an archive price can be cross-checked against
its own inc/exc pair, but a config rate arrives as a single number with nothing to check
it against. The real fix is therefore not a tighter band — it is requiring an explicit
`basis` field in the agreements namespace document, turning an unverifiable number into
a declared one. That is an upstream `config.swee.net` schema change, outside this repo.
Decide that before building the band, so the band ships scoped as a stopgap with the
gap recorded as an explicit non-goal.

**Q2 — What B2 does to R2.4, and whether the sub-quantum band goes on the wire.**
Reporting the clamped total leaves the stated invariant technically false either way, so
R2.4 becomes something like "sums to the meter, except for clamped buckets, whose total
is reported". The open part is whether the sub-quantum band - deliberately operator-only
today, on the argument that quantisation noise does not belong in a browser - should now
be consumer-visible. It is the band that most likely explains a gap this small, so
withholding it means reporting a number that does not account for the discrepancy the
consumer is looking at.

**Q3 — Collector backfill (P3c), deferred by D2.** Worth recording the reading that
deferred it: AGENT_BRIEF §1's "one exception, and only one" constrains the *kind* of
write — idempotent, immutable, externally-sourced price facts to a store holding
nothing else — not *which tariff codes*. Archiving additional codes arguably needs no
amendment to the brief. What defers it is storage and Octopus API budget, not the
invariant. If it is ever picked up, that distinction is the argument.

**Q4 — `/compare` scope values.** `household` and `monitored` are settled for v1.
Whether `room` and `device` scopes follow is left until there is a caller asking.
