# Bug: `unmonitored` series returns `avg_w = 0` (should be energy-derived)

**Status:** ✅ Fixed 2026-06-19 — see "Resolution" below.
**Found:** 2026-06-19 (against local `http://localhost:8081`, post-unmonitored ship)
**Severity:** Medium — silently wrong values; blocks the `countinghouse-index.html`
"plot unmonitored like a device" use-case (R3).
**Spec refs:** `countinghouse-unmonitored-consumption.md` §6.3 **C8**, **R3.1**.
**Related:** §0 status table marks R1 / R3 as "✅ shipped" — this defect means
R3.1 and C8 are not actually met, so those rows are overstated.

---

## Summary

The `unmonitored` series carries correct `kwh` / `cost` per bucket but returns
`avg_w = 0` for **every** bucket. The other house series (`monitored`, `meter`)
and all real devices return real `avg_w`. The correct value for unmonitored is the
energy-derived average power (`kwh × 1000 / bucket_hours`), per spec C8; `0` is a
wrong number, not a missing one.

---

## Reproduction

```sh
B=http://localhost:8081
TOK=$(curl -s http://localhost:8765/token | python3 -c 'import sys,json;print(json.load(sys.stdin)["access_token"])')
H="Authorization: Bearer $TOK"

# Synthetic device (the R3 "plot like a device" path):
curl -s -H "$H" "$B/devices/unmonitored/series?window=24h&interval=6h"

# Same values via the house rows:
curl -s -H "$H" "$B/series?window=24h&group_by=house&interval=6h&shape=rows"
```

### Observed (2026-06-19, 6h buckets)

`GET /devices/unmonitored/series?window=24h&interval=6h`:

```jsonc
{
  "kwh":   [3.377, 3.728, 0.847, 2.448, 3.968],
  "cost":  [0.7407, 0.8177, 0.1858, 0.537, 0.8704],
  "avg_w": [0, 0, 0, 0, 0],          // ← all zero despite 14.368 kWh total
  "total_kwh": 14.368, "total_cost": 3.1516
}
```

For contrast, a real device (`bigfridge`) on the same request:

```jsonc
{ "kwh": [0.4, 0.6, 0.4, 0.4, 0.4],
  "avg_w": [115.1, 116.1, 75.2, 104.4, 128.5] }
```

The `unmonitored` rows in `group_by=house&shape=rows` show the same thing — `kwh`
populated, `avg_w: 0`.

### Expected

`avg_w` derived from the bucket's residual energy and duration. For the first 6h
bucket: `3.377 kWh / 6 h = 0.5628 kW ≈ 563 W` — **not** `0`.

---

## Root-cause hypothesis

`avg_w` for real devices / `meter` / `monitored` is (almost certainly) the
**average of `power_w` telemetry samples** in each bucket. `unmonitored` is a
*derived energy residual* (`clamp(meter − monitored)`) with **no `power_w` stream
of its own**, so the telemetry-averaging path finds no samples and yields `0`,
while `kwh`/`cost` are computed on the separate energy path and come out correct.

So this is structural — reusing the telemetry path for a series that has no
telemetry — not a transient or interval-specific glitch. It will read `0` at every
interval.

---

## Why `0` is the worst of the three options

| Return | Verdict |
| --- | --- |
| `0` (current) | **Wrong.** Asserts "average power was zero" when it was ~563 W. Silently breaks any client that plots or peaks on `avg_w` — exactly the R3 single-device path (`countinghouse-index.html` charts `avg_w` as its primary line and shows a `0 W` peak). |
| `null` | Honest absence ("no power telemetry for a derived series"), but defeats R3's "plot like a device" goal and forces every client to special-case. |
| **`kwh × 1000 / bucket_hours`** | **Correct (C8).** The only option that makes the synthetic device behave like a real one. |

`0` is a *false value*, not a *missing* one — even if we decided the quantity were
meaningless, the right encoding would be `null`, never `0`.

---

## Recommended fix

Derive `avg_w` for the `unmonitored` series **server-side** from the per-bucket
clamped residual energy and the bucket duration:

```
avg_w[i] = unmonitored_kwh[i] * 1000 / bucket_hours[i]
```

- Use the **clamped** `kwh` (post-C1/C2) so `avg_w` agrees with the published
  energy and with the stacking invariant.
- Use the **actual** bucket duration so a partial first/last bucket isn't
  over/under-stated (C8's explicit requirement).
- Apply to both surfaces: `/devices/unmonitored/series` (R3) and the
  `group_by=house` rows/columns (R1).

### Caveat to record alongside the fix

This makes `unmonitored.avg_w` **energy-derived**, while `monitored.avg_w` and
`meter.avg_w` are **telemetry-averaged**. Those two methods differ slightly — the
same counter-vs-∫power integration bias documented in the spec (§2.5 / C5a). The
inconsistency is unavoidable (unmonitored has no telemetry to average) and far
better than `0`; note it in code so nobody later "fixes" it back toward the
telemetry path. Do **not** compute `unmonitored.avg_w` as
`meter.avg_w − monitored.avg_w` — that reintroduces the unclamped residual and can
disagree with the published `unmonitored.kwh` at bucket boundaries (mirrors the C9
reasoning for cost).

---

## Acceptance check

- `GET /devices/unmonitored/series?window=24h&interval=6h` returns non-zero
  `avg_w` where `kwh > 0`, equal to `kwh × 1000 / bucket_hours` within rounding.
- The same holds for `unmonitored` rows in `group_by=house`.
- A zero-energy bucket (`kwh == 0`) yields `avg_w == 0` (genuinely zero, fine).
- `countinghouse-index.html` plotting the `unmonitored` device shows a sensible
  power line and a non-zero peak stat (the R3 goal).

---

## Resolution (2026-06-19)

**Root cause** was slightly different from the hypothesis above (which assumed the
telemetry-averaging path returned 0). `deriveUnmonitored` was in fact computing
`avg_w = meter.AvgW − monitored.AvgW`. On real data the **summed monitored power
means exceed the meter's own mean** (the counter-vs-∫power averaging bias, §2.5),
so that difference is routinely negative and was clamped to 0 — hence `0 W`. The
existing `TestAssembleHouseDualSeries` masked it by using monitored power (350) <
meter power (1000), where the subtraction stays positive.

**Fix** (`internal/energy/series.go`): `deriveUnmonitored` now energy-derives
`avg_w = kwh × 1000 / bucket_hours[i]` (the C8 formula), using the **clamped** kwh
and the **actual** bucket duration. The bogus power-difference computation was
removed. `bucketHours` is threaded from `BuildSeries` (which already computes it
via `bucketHours(buckets, win.Stop)`) through `AssembleSeries` → `assembleHouse` →
`deriveUnmonitored`, and to the R2 catch-all / Q4 unclamped paths. Unclamped mode
preserves the signed residual, so `avg_w` can go negative consistently with kwh.

**Regression test:** `TestBuildSeriesUnmonitoredAvgWEnergyDerived` — a house build
where summed monitored power EXCEEDS meter power (the masking condition), asserting
`avg_w == kwh × 1000 / hours` per bucket (was 0, now correct).

**Verified live** (`/devices/unmonitored/series?window=24h&interval=6h`): full 6h
buckets read 563 / 621.3 / 141.2 / 408 W; the **partial** final bucket (4.615 h)
reads 987 W = `4.555 × 1000 / 4.615` — partial-bucket scaling correct per C8.

**Note recorded in code:** `unmonitored.avg_w` is now energy-derived while
`monitored.avg_w` / `meter.avg_w` remain telemetry-averaged — they differ slightly
by the same integration bias (§2.5 / C5a). This is unavoidable (unmonitored has no
telemetry) and documented so nobody reverts it toward the power-difference path.
