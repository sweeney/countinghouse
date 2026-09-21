# Countinghouse

Read-side energy **cost / accounting** service for the swee.net home. It turns the
per-device telemetry that [statehouse](../statehouse) writes to InfluxDB into per-device
**kWh and cost** over arbitrary time windows, decomposes the electricity bill
device-by-device, and serves **chart-ready time-series and on/off event timelines** —
so consumers never touch InfluxDB or Flux.

> Sibling service: `statehouse` (real-time state). Countinghouse owns tariffs, cost,
> billing, windowed reporting, and charting data. See `AGENT_BRIEF.md` and `PLAN.md`
> for the full design.

## Design tenets

- **Read-side only.** No MQTT, no device ingest, no real-time state. Query Influx + apply tariffs.
- **Stateless w.r.t. accumulation.** Answers are derived on query; no running totals are kept.
  The durable truth is the device-side counters already in Influx, so the service is safe to
  restart at any moment with zero data loss. Any future cache must be rebuildable from Influx.
- **Two energy query paths**, chosen by whether a device exposes a hardware energy counter:
  - plug classes + the meter → `increase(energy_kwh)` (reset-safe counter)
  - UPS (`ups_sensor`, power-only) → `integral(power_w)` → kWh

## HTTP API

All times use **Europe/London** window boundaries (DST-aware). Money is **GBP**, computed
from the remote `energy_tariffs` (rates are ex-VAT; VAT is applied). Browser consumers are
supported via permissive CORS.

Auth: every route except `/healthz` and `/openapi.json` requires a Bearer JWT from
`id.swee.net` — **both** user tokens and `client_credentials` service tokens are accepted.

| Method & path | Description |
|---|---|
| `GET /healthz` | Health: aggregated `status` (`ok` / `degraded` = a remote-config fetch failing / `unavailable` = Influx unreachable), version, uptime, Influx reachability, remote-config status. Always HTTP 200. Public. |
| `GET /openapi.json` | This API as JSON (served from `internal/httpapi/openapi.yaml`). Public. |
| `GET /devices` | Device catalog (id, display_name, room, `floor`, `covers`, class, `capabilities`: `energy`/`events`). Includes a synthetic `unmonitored` (rest-of-home) energy device when a whole-house meter is configured. |
| `GET /floors` | Floor catalog (id, name, order, elevation, device_count) — the vocabulary behind `floors=` and `group_by=floor`. |
| `GET /rooms` | Room catalog (id, name, floor, category, area, device_count) — the vocabulary behind `rooms=` and `group_by=room`. |
| `GET /devices/{id}/energy?window=&from=&to=` | Windowed kWh for one device (`source`: counter/integral). |
| `GET /devices/{id}/cost?window=…` | Windowed kWh + VAT-inclusive cost. `attribution` says how it was priced (`flat_rate` / `counter_slot`), `effective_rate` the GBP/kWh it works out to, `unpriced_kwh` any energy no rate was held for. |
| `GET /devices/{id}/series?window=&interval=&shape=` | Single-device time-series (kWh / cost / avg W per bucket), for any energy-capable device **including the whole-house meter** (excluded from `/series?group_by=device`, but a request for one device cannot double-count). Reserved id `unmonitored` serves the rest-of-home series in the same shape — the *same* shape, so it omits the house-only `coverage`/`stale_monitored_*` signals even though deriving it needs the whole-house decomposition; `group_by=house` carries those beside the identical values (404 when no meter is configured). |
| `GET /devices/{id}/events?window=` | State-transition events (for vertical-line overlays). |
| `GET /devices/{id}/intervals?window=` | Derived on/off spans + duty stats. |
| `GET /series?window=&interval=&group_by=&rooms=&floors=&include_unmonitored=&shape=&prices=` | Multi-series time-series. `group_by`: `device` (default), `room`, `floor` (the sum of its rooms), `class`, `house` (three series: `monitored` + `unmonitored` + `meter`, where `unmonitored` = clamp(meter − monitored) per bucket). `house` also returns top-level `coverage` (monitored ÷ meter) and `stale_monitored_count`/`stale_monitored_ids` (monitored devices with no telemetry in the window) as confidence signals — only this grouping does, `/devices/unmonitored/series` included. `include_unmonitored=true` adds the rest-of-home as one catch-all series to `device`/`room`/`floor`/`class` groupings so the parts sum to the meter (see [When the parts do not sum to the meter](#when-the-parts-do-not-sum-to-the-meter) for the one case where they overshoot it). `rooms=`/`floors=` (CSV) narrow which devices the response covers; an id holding no billed device is a `400`, and neither may be combined with `include_unmonitored=true` or `group_by=house`. `unclamped=true` is a diagnostic mode that returns the raw signed `meter − monitored` (negatives preserved) instead of clamping at 0. `prices=true` adds the per-bucket price array — see [The price behind each bucket](#the-price-behind-each-bucket). |
| `GET /events?devices=&class=&window=&group_by=` | Multi-device event overlay. `group_by`: `device` (default) / `class`. |
| `GET /bill?window=&from=&to=` | Per-device cost breakdown + standing charge + total + reconciliation vs the whole-house meter. Carries `scope`, plus `household_energy_cost`/`household_total` — see [What `/bill` covers](#what-bill-covers). Carries `attribution`, `effective_rate` and `unpriced_kwh` as above; per-device costs sum exactly to `energy_cost`. When no meter is configured, `reconciliation.meter_present` is `false` and `meter_kwh`/`unmonitored_kwh`/`coverage` are omitted. |
| `GET /tariffs` | Dated tariff agreements keyed by fuel, oldest first, plus which namespace answered. |
| `GET /prices` | Half-hourly price curve over a window, past or future. |
| `GET /prices/upcoming` | The near future with bands, ranks and cheapest-run windows. |
| `GET /prices/cheapest` | The cheapest contiguous window for a deferrable load. |
| `GET /prices/stats` | Per-day min/max/mean/spread and plunge-slot counts. |
| `GET /metrics` | Query counters, Influx latency, `drift_buckets_total` (negative meter−monitored drift beyond the 0.1 kWh quantum), uptime, goroutines. |

**Windows:** `today`, `week` (starts Monday), `month` — all period-to-date — and `custom`
(requires RFC3339 `from` & `to`). `from`/`to` apply **only** to `window=custom`; passing
them with any other window (period-to-date or rolling) is a `400` (the range would
otherwise be silently discarded). **Rolling windows:** `<N>d` is a trailing N calendar days ending now,
**day-aligned to local midnight** — `7d` = today + the previous 6 days, `1d` ≡ `today`;
`<N>h` is an **exact** trailing N hours (e.g. `24h`), not midnight-aligned. Use these for
"last 7 days" / "last 30 days" (`7d`/`30d`), as distinct from `week`/`month`, which reset
on Monday / the 1st. **Intervals:** `5m,15m,30m,1h,6h,1d` with a smart default per window
(rolling windows default by span) and a ~1000-bucket cap. That cap is stated in
**buckets**, so the window length it allows depends on the interval asked for — at `30m`
it is about 20 days, at `1h` about 41. The price routes cap in **days** instead (31 for
`/prices`, 366 for `/prices/stats` — see [How spend is calculated](#how-spend-is-calculated)),
so a caller chunking a price/consumption join across both needs two chunk sizes, and the
boundaries do not line up.

### Series response shapes (`shape=columns|rows`)

The series endpoints return one of two layouts, selected by `shape` (default `columns`).
Both carry the same numbers; pick whichever maps cleanly onto your consumer.

**`shape=columns`** (default) — a shared `buckets` time axis plus per-series value arrays.
Each array drops straight into a web charting library dataset:

```json
{ "window": "today", "interval": "1h", "group_by": "device", "shape": "columns",
  "buckets": ["2026-06-11T00:00:00+01:00", "2026-06-11T01:00:00+01:00"],
  "series": [
    { "key": "winefridge", "label": "Wine Fridge", "room": "groundfloor.kitchen",
      "class": "continuous_power_device",
      "kwh": [0.05, 0.04], "cost": [0.011, 0.009], "avg_w": [52.1, 41.8],
      "total_kwh": 1.30, "total_cost": 0.28 }
  ] }
```
```js
// Chart.js
{ labels: data.buckets, datasets: data.series.map(s => ({ label: s.label, data: s.kwh })) }
```

**`shape=rows`** — the "tidy"/long form: a flat `rows` list of one object per
(series, bucket), plus lightweight per-series `series` metadata (labels + totals for
legends). Rows are ordered by series then bucket time. Idiomatic for `Codable`
consumers (decode `rows` into a struct array) and grouped native charts:

```json
{ "window": "today", "interval": "1h", "group_by": "device", "shape": "rows",
  "series": [ { "key": "winefridge", "label": "Wine Fridge", "total_kwh": 1.30, "total_cost": 0.28 } ],
  "rows": [
    { "key": "winefridge", "time": "2026-06-11T00:00:00+01:00", "kwh": 0.05, "cost": 0.011, "avg_w": 52.1 },
    { "key": "winefridge", "time": "2026-06-11T01:00:00+01:00", "kwh": 0.04, "cost": 0.009, "avg_w": 41.8 }
  ] }
```
```swift
// Swift Charts — decode rows into [Point], group by key
struct Point: Codable, Identifiable { let id = UUID(); let key: String; let time: Date; let kwh, cost, avg_w: Double }
Chart(points) { p in LineMark(x: .value("t", p.time), y: .value("W", p.avg_w)).foregroundStyle(by: .value("series", p.key)) }
```

Timestamps are RFC3339 with the local offset (parse with JS `new Date(...)` / Swift
`ISO8601`/`Date`). Values are pre-rounded (kWh 3dp, cost 4dp GBP, avg_w 1dp W).

#### Bucket semantics

For a window whose `from` is **not** on an interval boundary, the bucket axis snaps
**down** to the interval grid (anchored at local midnight) so it matches Influx's
aggregation boundaries — e.g. `from=14:23` with `interval=1h` yields buckets starting at
`14:00, 15:00, …`. **The first bucket's timestamp can therefore precede `from`.**

That first bucket is a **partial** bucket: it is *labelled* by the grid boundary it starts
on, but its `kwh`/`cost`/`avg_w` cover only the part inside the window (`14:23` onwards),
exactly as the last bucket covers only up to `to`. Both edges are clipped, so moving `from`
later within the first bucket lowers the reported energy as it should, and summing `kwh`
across buckets never bills electricity from before `from`.

For a **counter-class** device — every plug class, and the meter — `total_kwh` equals
`GET /devices/{id}/energy` `kwh` over the same window, for every window. Both are the same
reset-safe `increase()` anchored at the first reading at or after `from`, so this holds by
definition rather than by arithmetic coincidence, including when a device's readings have a
gap around the window start.

A bucket a counter device reported nothing in is `0`, and the energy that accrued meanwhile
lands in the next bucket that does have a reading. A device that reported nothing at all in
the window is all zeroes, never a share of someone else's total.

**`ups_sensor` is estimated, not counted.** A UPS publishes only `power_w`, so there is no
counter to read and both endpoints estimate an integral. They estimate it the *same* way —
`integral(unit: 1h, interpolate: "linear")`, whole-window for `/devices/{id}/energy` and per
bucket for the series — so a UPS series tracks the scalar endpoint closely, instead of
differing from it by method as it once did. It is not the identity the counter classes get,
though, and two things separate them:

- **Bucket boundaries.** The whole-window integral sees the readings either side of a bucket
  edge; the per-bucket one does not. Where the load *steps* across an edge the two differ by
  **at most** half that step times the gap between the readings straddling it — less when
  those readings sit either side of the edge rather than on it, and nothing at all when they
  straddle it evenly. For a UPS's tens-of-watts moves at a 30s cadence that bound is around
  `0.0002` kWh per boundary.
- **A bucket the UPS reported nothing in** has nothing to integrate and publishes `0`, while
  the whole-window integral interpolates straight across the outage and counts the load. The
  series is the low one, by roughly the length of the outage. Closing that needs the readings
  either side of the gap, which a per-bucket query cannot see.

`avg_w` for a UPS is derived back out of that energy (`kwh × 1000 / bucket_hours`), so it is
the bucket's **time-weighted** mean power and cannot contradict the `kwh` printed beside it.
For a steady load on a regular cadence it is the same number a sample mean would give.

The synthetic `unmonitored` series has no scalar counterpart at all, and its per-bucket
clamping means its total is not the raw residual either.

**Silence is no longer read as zero.** A bucket a device did not report in comes back from
Influx as a null, which used to decode to a perfectly plausible `0 W`. Those buckets are now
recognised as absent, so a device that reported nothing all window is counted by
`stale_monitored_count` instead of appearing to have dutifully drawn zero watts — which was
the exact masking that signal exists to catch. A device that really did report `0 W` is data,
and is not flagged.

Which windows the **grid snap** affects: `window=custom` with an off-grid `from`, and
`window=<N>h` (e.g. `24h`), whose start inherits the current minute and second. `today`,
`week`, `month` and `<N>d` all start at local midnight, which is on every allowed
interval's grid, so their first bucket is a full one.

The **anchoring** above is separate and applies to every window, aligned or not: a counter
series always measures from the first reading at or after `from`. Energy a device
accumulated before `from` and reported just after it belongs to the previous window, not
this one — which is what `/devices/{id}/energy`, `/devices/{id}/cost` and `/bill` have
always returned.

Bucket lengths are real wall-clock lengths, not nominal ones: a `1d` bucket spanning a DST
change is 23h or 25h, and `avg_w` for the derived `unmonitored` series is scaled by the
hours a bucket actually covers.

The OpenAPI document (`internal/httpapi/openapi.yaml`) is the source of truth for request
and response schemas; a path-coverage test fails CI if routes and spec drift.

### Browser access

Every response carries `Access-Control-Allow-Origin: *`, and preflight (`OPTIONS`)
is answered before auth runs, since a preflight carries no `Authorization` header.
The wildcard is safe because the API authenticates by **Bearer token, not cookies**,
so no `Access-Control-Allow-Credentials` is involved.

Responses also carry `Timing-Allow-Origin: *`. CORS does not imply it: without it a
cross-origin consumer's `PerformanceResourceTiming` entry has every phase (DNS, TCP,
TLS, TTFB) and both transfer sizes zeroed, leaving only total duration — so a
dashboard measuring countinghouse cannot tell a slow query from a slow network. The
same token reasoning makes the wildcard safe here: a page with no token can only
time its own 401, and a page with one is already reading the body it is measuring.


## Rooms and floors

`location` used to mean two different things across these services — a geographic site
and a room — so rooms are now `room`, sites are `site`, and floors are `floor`. Room ids
are `<floor>.<slug>`: `groundfloor.kitchen`, `basement.network-cabinet`.

**The floorplan is relayed, never derived.** A device's `floor` is a first-class property
of the devices namespace, so countinghouse passes it through rather than splitting the
room id on its first dot. `GET /floors` and `GET /rooms` relay the floorplan namespace's
own records — name, storey order, elevation, category, area — and report what it does not
declare as unknown (empty name, `null` order/area) rather than title-casing an id or
inventing a position. This is the same namespace and the same shape [greenhouse](../greenhouse)
serves, so a page talking to both services about one house gets one vocabulary.

Both catalogs list exactly what the matching filter accepts: the floors and rooms holding
at least one **billed** device (metered, excluding the whole-house meter). A floorplan
record for a room with nothing metered in it is not listed — it exists in the building,
but not as far as the energy API is concerned — and a room devices declare that the
floorplan has no record for is listed with empty fields. A picker filled from either
catalog therefore cannot build a request `/series` rejects.

`category` is relayed **raw**, deliberately not reduced to a computed flag: a plant room's
consumption is infrastructure rather than household usage, but which rooms "count" is a
per-dashboard policy question — a breakdown, a coverage view and a heat-loss view each
answer it differently.

**Grouped series are labelled with the floorplan's name.** `group_by=room` and
`group_by=floor` set `label` to the published name (`"Room A"`) and fall back to the id
when none is published; `key` is always the id. A room-grouped series also reports its
`room`, so it can be joined to `/rooms` — except the reserved `house` series, which
reports an empty `room` and keeps its key as its label, because a coverage scope is not a
place and `/rooms` never lists it.

**`group_by=floor`** sums a floor's rooms — energy is additive, so sum is the only sane
statistic and there is no `group_fn`. With `include_unmonitored=true` the rest-of-home
residual is returned as its own series and is never attributed to a floor: it is
house-scoped and belongs to no storey.

**`rooms=` / `floors=` filters** (CSV) narrow the device set for `/series`, composing as
AND. They were previously accepted and ignored — a client asking for one room got a 200
carrying the whole house — so an unknown id is now a `400` rather than a silently
unfiltered answer, as is a value carrying only separators (`rooms=,,`, almost always a
join that produced nothing). A bare `rooms=` still means "no filter". They cannot be combined with `include_unmonitored=true` or
`group_by=house`: the unmonitored series is the meter minus **all** monitored devices, so
against a filtered set it would quietly absorb the excluded devices and publish them as
rest-of-home. `/devices/{id}/series` ignores them — the path has already selected.

The deprecated `location` spelling has been removed: `group_by=location` and the
`location` response field are both gone. Use `group_by=room` and `room`.

**Whole-property devices have no room.** The electricity meter, central heating and
hot water report an empty `room` and `covers: "house"` — on `/devices` and in the
`/bill` breakdown — so an empty room is legible rather than mysterious.

A device may also declare `covers`. Under `group_by=room` and `group_by=floor` a device
covering the whole property is grouped under a **`house`** key rather than the place it
sits in — putting
whole-property consumption in one room would be the same conflation under a new name,
and dropping it would break the guarantee that the grouped parts plus `unmonitored`
sum to the meter.

A device sits in a room, but its readings do not always describe that room — `central_heating`, `hot_water` and `electricity_meter` each
sit in one room while metering the whole property. `location` used to record sometimes
one fact and sometimes the other; `room` and `covers` record them separately.

Countinghouse reads whichever the devices namespace carries. A namespace still declaring
`location` keeps working untouched.

## Sites

An instance declares which property it serves, and normally nothing else:

```yaml
site:
  id: home
```

The namespace pointers come from the shared **`sites`** document, keyed by that id. They
are deliberately not a local setting: rename a namespace in `sites` and every other service
follows, while a local copy would quietly keep reading the old document. So `sites`
**wins** over a local value that disagrees — and a warning names both, because an
operator's edit being ignored without a word is its own kind of silent failure.

`floorplan_namespace` and `energy_agreements_namespace` keep a local fallback, which is what
makes the migration safe: a site whose `sites` entry is only partly filled in keeps working
off its own config. **`devices_namespace` has none, and is not settable here at all.** It is
the pointer that decides whether any answer is right — a stale local copy would not degrade
a label, it would bill *another property's* devices while the service looked entirely
healthy. It comes from `sites` or the instance does not start. Nor is it derived from `id`:
a namespace is a document that either exists or does not, so guessing `devices_<id>` would
turn a typo into a 404, a fail-open empty snapshot, and every endpoint honestly reporting
zero devices.

**Both site namespaces are required**, wherever they are supplied from. For the floorplan
that is a quieter version of the same reason. Omitting it breaks nothing: `/floors` and
`/rooms` still list every floor and room holding a metered device, and every kWh and cost is
exactly right. Only the **names** are lost — so those endpoints answer with ids where labels
belong and `null` where storey order belongs, which is precisely what a floorplan publishing nothing would produce.
Nothing distinguishes "not configured" from "configured and empty", and the omission
surfaces days later as a chart legend reading `floor1.room-c` to a human. So it is
declared or the service refuses to start. (This is stricter than greenhouse, which treats
the same namespace as optional.)

### Boot needs truth, running keeps the last truth

Remote-config fetches are **fail-open** — a failure keeps the last-known snapshot — with
one exception: **a namespace that has never been fetched at all aborts startup.** At boot
there is nothing to fall back to, so fail-open would fall open onto emptiness, and the
service would come up answering every question with zero kWh, no tariff, or floor and
room ids where names belong — in the confident shape of a correct response. That is the
same silence the required-namespace checks refuse, one layer later, where a named
namespace turns out to fetch nothing.

```
ERROR remote config: refusing to start, nothing was ever fetched for floorplan_home
      cold_namespaces=[floorplan_home]
```

This costs availability if the config service is down exactly when countinghouse
restarts, and that trade is deliberate: a read-side service that is visibly down beats
one that is invisibly wrong, systemd's restart loop recovers the moment config returns,
and the failure is legible in the unit status rather than in a chart legend.

Once a namespace has landed, the rule inverts. A later failure — including a **SIGHUP**
reload — is *stale*, not *cold*: the last-known snapshot is served, `/healthz` reports
the failing namespace and its `status` goes `degraded`, and the process keeps running.
Killing a healthy instance over a transient config blip would turn fail-open inside out.
`remote_config.<namespace>.ok = false` on a running instance therefore always means
stale, never empty.

`remote_config.base_url` being empty is the one opt-out: nothing is fetched, the cold
check is skipped, and empty snapshots are served — an operator who names no config
service has said they expect that (local dev). `/healthz` reports both site namespaces,
so you can see which property's devices and floorplan an instance believes it serves.

**The devices namespace is not a local key at all.** It comes from the shared `sites`
namespace, or the instance does not start. It briefly defaulted to `statehouse_devices`,
the shared namespace every service read before devices were split per site; that document
has been deleted, so the default came to name a 404 — and every layer below handles that
correctly into silence: the fetch fails, the refresh is fail-open and keeps the
last-known snapshot, at startup there is no last-known snapshot, and every endpoint then
reports zero devices. For a billing service that is a wrong answer in the shape of a
right one. Declaring it locally *as well* is how it would drift — rename it in `sites`
and every other service follows while this one keeps reading the old document — so the
pointer has exactly one home.

`id` is the only site key normally set here. The devices namespace is deliberately
*not* derived from it either: a namespace is a document that either exists or does not, and guessing its name
from the site id would turn a typo in `id` into a silent fetch of nothing rather than a
startup complaint. The mirror case — a namespace with no `id` — stays a warning, because
that instance serves correct numbers and only loses the ability to say which property it
serves; taking it down to fix a label would be the worse trade. The resolved pair is
reported on `/healthz` so which site an instance believes it serves is observable rather
than inferred from behaviour.

## Configuration

Local bootstrap YAML (default `/etc/countinghouse/config.yaml`; see `config/config.example.yaml`),
overlaid by remote config fetched from `config.swee.net`.

```yaml
http:    { listen: ":8585", public_url: "https://countinghouse.swee.net" }
influx:  { url, org: "swee.net", bucket: "statehouse", token_file: /etc/countinghouse/influx-token }
identity:{ base_url: "https://id.swee.net", client_id, client_secret }
remote_config: { base_url: "https://config.swee.net" }
house:   { timezone: "Europe/London" }
```

- **Influx** read token must be scoped (read-only) to the bucket statehouse writes.
- **`identity.client_id`/`client_secret`** are used only to fetch the remote config namespaces
  (the devices namespace named by the `sites` entry, and the tariff namespace) via
  `client_credentials`. Fetches are fail-open and reload on `SIGHUP`.
- **`/healthz.remote_config`** is keyed by the namespace actually read, so the devices
  entry is named by the `sites` entry for this instance — `devices_home` here. There is
  no default: a site whose `sites` entry names no devices namespace does not start. A monitor keyed on a literal
  namespace therefore stops matching when a site migrates, and in most check expressions
  a missing key reads as healthy rather than as an error — so alert on the top-level
  `status` field, which degrades regardless of the key.

### Tariffs: `energy_agreements`

Tariffs are configured as **dated agreements** — one block per tariff you were on,
with explicit bounds. Keyed by fuel; countinghouse bills `electricity` and reads,
validates and ignores the rest. Rates are GBP ex-VAT, and VAT is applied by the cost
layer.

```json
{ "agreements": { "electricity": [
    { "from": "2025-09-23T00:00:00+01:00",
      "to":   "2026-09-10T00:00:00+01:00",
      "name": "Octopus 12M Fixed",
      "type": "fixed",
      "id":   "E-1R-OE-FIX-12M-25-09-09-A",
      "unit": "kWh", "vat_rate": 0.05,
      "unit_rate": 0.208948,
      "daily_standing_charge": 0.529443 },

    { "from": "2026-09-10T00:00:00+01:00",
      "name": "Agile Octopus",
      "type": "variable",
      "id":   "E-1R-AGILE-24-10-01-A",
      "unit": "kWh", "vat_rate": 0.05,
      "daily_standing_charge": 0.591606 }
] } }
```

**`fixed` carries its rate. `variable` deliberately does not.** A variable tariff's
price changes every half hour, so there is no single number to put here — the curve
lives in the price archive and consumers read it from the price endpoints. A
representative rate in this document would be read as *the* price, and would be
wrong. The standing charge stays here either way: it is flat per day on both types.

`variable` is narrower than the industry's use of the word. A supplier's *standard
variable rate* — one number that changes every few months — has no intra-agreement
curve to look up, so it is configured as **successive `fixed` blocks**, one per rate.
`variable` means specifically "priced per half hour from the archive".

Rules, all enforced when the namespace is applied:

- `from` is **inclusive**, `to` **exclusive**. `to` absent means the agreement is
  current. The tariff in force at an instant is the agreement covering it.
- **Gaps are allowed.** No agreement covering an instant is a real state — you were
  not a customer — so the document may say so. But a window touching a gap is
  **refused, not priced**: billing it at a neighbouring rate would be invisible and
  wrong, and returning only the covered parts would silently under-bill.
- **Overlaps are refused.** Two agreements covering one instant means two prices for
  one kWh, and there is no defensible way to pick. Only the most recent agreement may
  omit `to`.
- A window spanning a boundary is billed as **one segment per agreement**, each with
  its own rate, standing charge and VAT multiplier. Segments tile the window exactly,
  so the apportioned standing charge adds up.
- `name` is required — an unnamed agreement surfaces as its code, and a code where a
  name belongs reads as data rather than as a missing label.
- `id` is required on `variable` (it is the key prices are archived under, so without
  it the block cannot be priced) and optional on `fixed`, but validated when present.
- `fixed` requires `unit_rate`; `variable` must **not** set one.
- Blocks may be authored in any order; they are sorted on load.
- An invalid document is treated exactly like a failed fetch: the last-known snapshot
  is kept and `/healthz` degrades. Per *boot needs truth, running keeps the last
  truth*, a namespace that has never been fetched still aborts startup.

#### Runbook: the temporary zero rate of VAT, 1 Oct 2026 – 31 Mar 2027

[HMRC](https://www.gov.uk/government/publications/temporary-zero-rate-of-vat-for-domestic-electricity-in-great-britain/temporary-zero-rate-of-vat-in-great-britain-for-domestic-electricity)
zero-rates qualifying supplies of **domestic electricity in Great Britain** for supplies
made from **1 October 2026 to 31 March 2027**. All other domestic fuel stays at 5%
UK-wide — irrelevant here, since countinghouse bills electricity only, but it is why the
rate lives on the agreement rather than on the service.

**What to do:** split the electricity agreement into three dated blocks that differ
**only** in `vat_rate`, keeping the same `id`. Both boundaries are **local midnight**,
and both fall inside BST, so each is `23:00Z` the day before — a document written in UTC
midnights zero-rates two half hours of 30 September and un-zero-rates two of 31 March.

```jsonc
"electricity": [
  { "from": "2026-01-01T00:00:00Z", "to": "2026-09-30T23:00:00Z",
    "name": "Agile", "type": "variable", "id": "E-1R-AGILE-24-10-01-A",
    "vat_rate": 0.05, "daily_standing_charge": 0.59 },
  { "from": "2026-09-30T23:00:00Z", "to": "2027-03-31T23:00:00Z",
    "name": "Agile (VAT zero-rated)", "type": "variable", "id": "E-1R-AGILE-24-10-01-A",
    "vat_rate": 0,    "daily_standing_charge": 0.59 },
  { "from": "2027-03-31T23:00:00Z",
    "name": "Agile", "type": "variable", "id": "E-1R-AGILE-24-10-01-A",
    "vat_rate": 0.05, "daily_standing_charge": 0.59 }
]
```

**Author the third block now, not in March.** The return to 5% is the half of this
change nobody is watching for, and its failure mode is a bill under-charging VAT rather
than a loud one.

**What happens if you forget.** Nothing is lost, and — since standing charges are
archived too — nothing is wrong. The archive keeps filling: the VAT check is Gate B, so
every slot is stored and flagged `vat_mismatch`. Both energy and the standing charge are
billed from the supplier's own inc-VAT figures, so the statutory change arrives in the
data rather than having to be applied from here. `vat_rate` is now purely an
**expectation**: when it disagrees with the supplier consistently across a batch, the
collector raises an `agreement_drift` alert naming the rate the supplier is actually
charging — which is the number to paste into the document.

The one case that still depends on config is a window with **no archived standing
charge** (a fresh deployment, before the first daily sweep). `/bill` says which it used
in `standing_charge_source`.

**What does not need doing.** `/prices` and `/prices/stats` serve windows spanning these
boundaries normally: the split leaves the tariff code unchanged, so there is still one
curve. Only a genuine tariff change, or a **flat**-rate tariff across a VAT change,
refuses with a 400.

### The standing-charge archive

The supplier's daily standing charge is archived alongside unit prices, in its own
`standing_charge` table with the same bitemporal key and the same restatement log. A
separate table rather than a `kind` column, because the two hold different **units** —
pence per **day** here, pence per kWh there — and one table makes it possible to sum
them with a query that forgot to filter. The Go types are separate for the same reason;
the storage path is shared, so the bitemporal machinery has one implementation.

Fetched on the **daily sweep**, not on every poll: a standing charge moves about once a
year while unit prices move every half hour, so polling it at the unit-price cadence
would be ~288 requests a day to learn nothing.

**Why it exists.** Before this, `/bill` computed the standing charge as
`days × daily_standing_charge × (1 + vat_rate)` — entirely from config. That made it the
**last number in a bill still grossed up from configuration**, and therefore the last one
a stale `vat_rate` could silently get wrong, on a service that already prices energy from
the supplier's own inc-VAT column. Archiving it makes both sides of a bill come from the
same place, and demotes `vat_rate` to a checkable expectation everywhere.

`/bill` reports `standing_charge_source`: `archive` when it used the supplier's figure,
`config` when it fell back. It falls back — whole, never partly — when no archived charge
covers the entire window, when the window spans a genuine tariff change (two codes, two
charges to reconcile; config already segments that correctly), or when the read fails.
A partial total would look like a correct but cheap bill, which is the failure the whole
pricing layer exists to avoid.

### The price archive

A half-hourly (`variable`) agreement has no unit rate in config — its prices live in a local
SQLite archive that countinghouse fills from the supplier. This is the one thing the service
writes; see `docs/octopus-price-data-model.md` for why it is SQLite rather than Influx.

```yaml
prices:
  db_path: "/var/lib/countinghouse/prices.db"
```

- **Unset disables collection**, which is correct for a deployment whose agreements are all
  flat-rate: there are no half-hourly prices to keep.
- **Set it before any agreement becomes `variable`.** Startup refuses that combination rather
  than booting successfully and then being unable to price anything after the switchover.
- The parent directory must exist and be writable by the service user. The file and SQLite's
  `-wal`/`-shm` companions are created mode 0600.
- **Back it up** — `prices.backup`, below. It is rebuildable from the supplier today, and
  the entire reason to keep it is the day that stops being true. Roughly 225 bytes a slot —
  measured at 7.8 MB for two years of one tariff, so ~80 MB over twenty.

#### Collecting without the rest of the service

The supplier's rate endpoints need **no authentication** — they are public product data —
so filling the archive needs no API key, no Influx, no remote config and no identity.
`-collect` runs only the collector:

```sh
# Fill the archive and keep it current. Ctrl-C to stop.
countinghouse -collect -prices-db ~/prices.db -tariff E-1R-AGILE-24-10-01-X -vat 0.05

# One sync and exit, for a cron or a check.
countinghouse -collect -once -prices-db ~/prices.db -tariff …

# Backfill from a date first.
countinghouse -collect -once -back-to 2025-01-01 -prices-db ~/prices.db -tariff …
```

It is the same collector, store, validation gates and migrations the service uses — not a
second implementation that could drift from them and write a subtly different archive. It
exists for three jobs: **start accumulating real prices now**, on any machine, before the
service is deployed (several open questions here are measurements waiting on weeks of data
rather than decisions waiting on thought); a one-shot **backfill**; and reproducing a
collector problem against the live API without standing the service up around it.

`-vat` is optional and only feeds the inc/exc consistency check — pricing never uses it.
Omitting it means *do not check*, which is different from checking against 0%.

Measured against the live API: 34,942 slots in one pass, 0 rejections, and a second run a
clean no-op.

No backfill step is needed: a first sync against an empty archive requests an unbounded range
and so pulls the supplier's whole published history in one pass (measured: 34,894 slots in
6.5 s). Afterwards each sync fetches only what is new, detected by a single ~350-byte probe.

**The supplier's horizon stops two half hours short of the furthest day's end**, and for
about sixteen hours of every day that furthest day is *today*. Measured over 729 archived
days: every historical day is complete, and the only short one is always the newest.
A tail gap that size is therefore healthy — it does not alert and does not void
`complete_to`. An *interior* hole does, at any size. See
`docs/octopus-price-pipeline.md`.

`GET /healthz` and `GET /metrics` gain a `prices` block, one entry per collected tariff —
omitted entirely when no collector runs. Two fields answer different questions, and the
difference matters:

| field | means | use |
|---|---|---|
| `known_to` | end of the newest slot held | "prices are arriving" |
| `complete_to` | how far prices run with **no gaps** | **"we can bill this far"** |

Alert on `complete_to`. It stops at the first *interior* hole and **not** at the supplier's
routine two-slot tail on the furthest published day, because everything before that tail is
billable. `complete_to` at or behind now means today cannot be priced in full, and degrades
the top-level `status`.

In normal operation `complete_to` therefore **equals** `known_to`. That is the signal rather
than a redundancy: the two diverge exactly when a slot the supplier published never reached
us, so `complete_to < known_to` means a real hole rather than a horizon. It was previously
held back a day so the two would look distinct, which cost more than it bought — a field
permanently behind its neighbour carries less information than one that matches until
something is wrong.

**A failing collector reports differently on the two endpoints.** `/metrics` is behind auth
and carries `last_error` — the whole string, which for an upstream failure can include up to
2 KB of somebody else's response body. `/healthz` is unauthenticated, so it carries
`last_error_class` instead: fixed words the collector picks from the *typed* error, one of
`upstream rate limited`, `upstream rejected our request`, `upstream unavailable`,
`upstream timeout`, `upstream error`, `archive error`, or a bare `error` when nothing more
specific is known.
A monitor can route on those without the service ever republishing arbitrary third-party
bytes, or the local archive path, on a public endpoint. Both fields clear on the next
success, and either one present degrades the top-level `status`.

### Backing up the archive

> **Not configured yet, deliberately.** The service goes live without offsite backups
> and they are added afterwards. That is defensible only because the archive is still
> rebuildable from Octopus today — which is precisely the property this section says we
> cannot rely on forever, so it is a debt with a due date rather than a decision. It
> needs an R2 API token scoped to the `countinghouse-sqlite` bucket. Leave the whole
> `prices.backup` block out until then: a **partial** block is refused at startup, on
> the grounds that a backup quietly not happening is worse than no backup.
>
> The restore has also never been performed. A backup that has not been restored is a
> hypothesis.


The archive is the one thing countinghouse writes and the only state here that is not
rebuildable from Influx, so it is the one thing that gets a backup rather than a
retention policy. `prices.backup` sends it to Cloudflare R2 on a schedule, snapshotting with
`VACUUM INTO` so a backup can run while the collector is writing.

```yaml
prices:
  db_path: "/var/lib/countinghouse/prices.db"
  backup:
    env: "production"                 # the R2 key prefix: production | development
    bucket: "countinghouse-sqlite"
    account_id: "…"
    access_key_id: "…"
    secret_access_key_file: "/etc/countinghouse/r2-secret"
    schedule: "daily"                 # daily | weekly (Sun) | monthly (1st) | off
    hour: 3                           # UTC
```

Objects land at `{env}/backups/countinghouse/{YYYY}/{MM}/{DD}/countinghouse-{RFC3339}.sqlite3`,
which is the layout `identity/common/backup` restores from.

- **Omit the whole block to disable backups.** Correct for development and for any
  deployment with no archive. **A partial block is refused at startup** — a backup that is
  quietly not happening is worse than none, because you believe the archive is safe and
  find out otherwise at the only moment it matters.
- **`env` is required and is not defaulted.** It is the key prefix, and the restore tooling
  matches the literals `production` and `development`. Defaulting it either way would file
  one environment's backups under the other's prefix, where they exist and no restore looks
  for them.
- **The secret goes in a file.** `secret_access_key_file` is read only when the inline
  `secret_access_key` is empty — the same pattern as `influx.token_file` — and a named file
  that is missing or empty is a startup refusal rather than an auth error hours later. The
  credential never appears in a log, an error or an HTTP response; errors bound for
  `/healthz` are scrubbed of anything credential-shaped on the way out.
- **One bucket per service**, so the R2 API token can be scoped to it: a leaked
  countinghouse credential then cannot read or overwrite identity's backups.
- **Bad credentials do not stop the service.** They cannot be detected until the first
  upload, and an unreachable bucket should not take the cost API down with it. The failure
  shows up on `/healthz` instead.
- The snapshot is staged under `/tmp` (mode 0600) and deleted after upload. The systemd
  unit sets `PrivateTmp=true`, so that copy of the archive is the service's alone — worth
  preserving if the hardening is ever revisited.
- **It uses `common/backup`'s scheduler.** It did not, for a while: `Manager` could not be
  asked what it had done and could not be tested against a clock, so this service grew its
  own snapshot, schedule, status bookkeeping and credential redactor — about 230 lines.
  All four gaps are closed upstream (sweeney/identity#45), so all four local versions are
  gone and what remains is an adapter from config to the health blocks. `last_error`
  arrives already redacted by the library, whose redactor is key-aware where the local one
  truncated bluntly at the first marker word — and it is still served on `/metrics` only,
  because a redactor that removes credentials does not remove infrastructure.
- **Historically this did not use the scheduler, and the stated reason was wrong twice.**
  First: that `Manager` copied a WAL-mode database with `os.ReadFile` — true of v0.3.0, and
  fixed in v0.4.0 by `VACUUM INTO`, which this repo did not notice because it sat two
  versions behind. Then: that `ScheduleHour == 0` was read as unset — also a v0.3.0 bug,
  also already fixed. Both claims were written from reading an older copy of the library
  rather than the version the build resolves. Recorded because the fix each time was a
  comment, not code, and the habit that prevents it is `go list -m -u all`.
- `deploy/bootstrap.sh` creates `/etc/countinghouse/r2-secret` (0640, `root:countinghouse`)
  empty. The R2 token itself is minted in the Cloudflare dashboard; scope it to this bucket
  alone.

`/healthz` and `/metrics` gain a `backup` block — omitted entirely when none is configured,
so a zeroed block never reads as a broken backup. **The two endpoints do not serve the same
block.** `/metrics` is behind auth and reports everything; `/healthz` is unauthenticated and
reports whether the backup is working, not where it lands:

```json
// GET /healthz — no bucket, no env, no object key, no error text
"backup": {
  "schedule": "daily", "hour": 3,
  "last_attempt": "…", "last_success": "…",
  "successes": 9, "failures": 0,
  "next_run": "…"
}
```

```json
// GET /metrics — the same block, plus what names our infrastructure
"backup": {
  "bucket": "countinghouse-sqlite", "env": "production",
  "schedule": "daily", "hour": 3,
  "last_attempt": "…", "last_success": "…",
  "last_key": "production/backups/…/countinghouse-….sqlite3",
  "last_error": "…",
  "successes": 9, "failures": 0,
  "next_run": "…"
}
```

`last_attempt` moves on every run, `last_success` only on one that worked — so a lagging
`last_success` means we are failing *now*. `last_key` is how the newest backup is found
without listing the bucket, **and it is on `/metrics` only**: an anonymous caller has no use
for an object key, and publishing one names the bucket, the prefix and the backup cadence to
anyone who asks. `/healthz` keeps the timestamps and the counters, which is everything a
monitor needs to alert. Same split for the failure: `/healthz` carries a fixed
`last_error_class` (`"backup failed"`) where `/metrics` carries the message, because the R2
endpoint host and the object key turn up in those strings.

Three states **degrade** the top-level status (never make it `unavailable`: nothing served
depends on last night's upload):

- a failure since the last success — what is in the bucket no longer covers what would be lost;
- attempted and **never** succeeded, which is what a typo'd credential leaves behind. Not
  reported before the first run, or every restart would look like a fault;
- **stale** — no successful backup in 48 hours, with no error to show for it. A wedged
  scheduler produces silence rather than a failure, so a verdict keyed only on the error
  would call it healthy. Not applied when `schedule: off`, where an old backup is what was
  asked for.

### The price endpoints

Four read-only routes over the archive. All need a Bearer token like any data route,
and **service tokens work**, because the consumers are other services.

```
GET /prices/upcoming?hours=12
```

```json
{ "tariff_code": "E-1R-AGILE-24-10-01-A",
  "unit": "p/kWh", "vat_included": true,
  "summary": { "slots": 24, "current": 45.85, "min": -2.62, "mean": 24.48,
               "median": 28.42, "max": 50.22 },
  "slots": [
    { "valid_from": "…T11:00:00+01:00", "price": -2.62, "rank": 1,
      "percentile": 0, "band": "plunge" } ],
  "cheapest": {
    "30m": { "from": "…T11:00:00+01:00", "to": "…T11:30:00+01:00", "mean_price": -2.62 },
    "3h":  { "from": "…T10:30:00+01:00", "to": "…T13:30:00+01:00", "mean_price": -2.49 } },
  "missing": [], "complete": true }
```

The derivations are **served, not left to the caller**. Two dashboards inventing their
own definition of "cheap" is how a house ends up with two screens disagreeing about
whether now is a good time.

- **`band`** is `plunge` / `cheap` / `normal` / `peak`. `plunge` is any price at or
  below zero — free energy, or being paid to take it — kept separate because it is
  categorically different from merely cheap, and it is the signal most worth seeing.
  The rest sit ±15% from the window's **median**.
- **Median, not mean, and not percentiles.** A percentile split always labels a fixed
  share of the window as peak, which on a flat day is false. And the mean is dragged
  about by plunge clusters: on a real published day with ten negative slots the mean
  fell to 23.38p against a median of 28.42p, which would have banded **28 of 48 slots
  as peak** — more than half the day — diluting the signal to nothing. The median
  shrugs that off, and plunge days are exactly the days these endpoints exist for, so
  the statistic has to survive them.
- **`rank` 1 is the cheapest**, because the question is "when should I run this".
- **`percentile`** is served too, so a consumer that dislikes our thresholds can band
  it differently without refetching — and `summary.median` carries the centre our own
  bands are measured from, so that invitation is actually actionable. Offering the
  choice while withholding the figure it turns on is not an offer.

```
GET /prices/cheapest?duration=3h&before=2026-09-12T07:00:00Z
```

Two behaviours worth knowing. A run **never spans a gap** in the prices: a window
whose prices we do not hold cannot honestly be called cheap. And `before` is a
deadline for **finishing**, not starting — a load that overruns into expensive time
was not scheduled, it was merely begun. A duration that is not a whole number of half
hours rounds **up**. No window of that length returns **404**, which is a well-formed
question with no answer rather than a bad request.

`/prices/stats` reports per-**local**-day figures — the only framing in which a 23- or
25-hour day makes sense. `spread` is max − min: the single number saying whether
shifting load that day was worth the bother. Figures are **VAT-inclusive**, as on every
sibling price route, with ex-VAT values alongside under `*_exc_vat` keys. They were the
other way round, and a dashboard plotting a daily mean against a live price was then out
by the VAT rate with nothing on the wire to say so — a trap that had been documented in
three places rather than removed.

All four carry an **ETag** and a short `Cache-Control`, so a dashboard polling every
few seconds gets a 304 rather than re-downloading 48 slots. The tag hashes the
rendered body, so it cannot claim "unchanged" when a band has shifted because the
window slid forward.

A **flat-rate** tariff has no curve. Those routes then answer `half_hourly: false`
with a `flat_price` rather than an empty `slots` array — a different shape of answer,
so nobody goes hunting a collector bug that does not exist. With no archive configured
at all they answer **503**: the route exists and works elsewhere, so it is a
deployment state rather than a missing endpoint.

#### Migrating from `energy_tariffs`

The legacy namespace still works untouched. It holds one current rate per fuel and no
dates, which means **every instant resolves to the same rate** — so a historical
window is priced at today's price, which is wrong whenever the rate has ever changed,
and it cannot express a half-hourly tariff at all.

Migration is opt-in via one local-config key:

Add `energy_agreements_namespace` to this site's entry in the shared **`sites`** namespace,
alongside the pointers that already live there:

```json
{ "sites": [ {
    "id": "home",
    "devices_namespace": "devices_home",
    "floorplan_namespace": "floorplan_home",
    "energy_agreements_namespace": "energy_agreements"
} ] }
```

A tariff is a property of the site — a second property is generally on a different tariff, in
a different region, at different rates — so it belongs with the other per-site pointers rather
than in each service's local config.

**Startup is two-phase** as a result: `sites` is read first to learn which namespaces this
property uses, then those namespaces are fetched. That makes `sites` boot-critical in a way
the others are not — until it has been read there is nothing to fail open *onto*, because we
could not even name what is missing. A failure there aborts, consistent with the cold-start
rule.

Local `site:` keys remain as a **fallback** for `floorplan_namespace` and
`energy_agreements_namespace`, covering a site whose `sites` entry is only partly filled in.
Where both are set, **`sites` wins and a warning names both** — a stale local pointer silently
overriding the correct remote one is exactly the drift this arrangement removes, but an
operator's edit must not be ignored without a word.

**`devices_namespace` has no local fallback and cannot be set locally at all.** It is the
pointer that decides whether any answer is right: a stale copy would not degrade a label, it
would bill *another property's devices* while the service looked entirely healthy. It comes
from `sites` or the instance does not start.

Pointers are resolved **once, at startup**. A later SIGHUP reports a change but does not adopt
it: repointing a running service at another property's data would swap the device inventory
underneath every in-flight answer, so that needs an explicit restart.

Unset, the legacy document is authoritative. Set, the new one is — and the legacy
document is not even fetched. **The two are never merged:** two documents disagreeing
about what a kWh cost has no safe resolution, so exactly one answers and both
`/healthz` and `GET /tariffs` report which. A named namespace that has never been
fetched aborts startup rather than falling back, because falling back would price a
variable tariff at a fixed number.

`GET /tariffs` serves the **dated-block shape either way** — a legacy document is
presented as a single agreement with neither bound — so a consumer handles one shape
rather than two.

### How spend is calculated

A fixed tariff has one number, so cost is one multiplication. A half-hourly tariff has
forty-eight a day, of **either sign**, and `unit_rate` in config is deliberately absent
— the curve lives in the archive. So the cost path asks two questions in order: what
prices the energy in this window, and at what resolution must the energy be measured to
apply it?

```
  FIXED TARIFF                          HALF-HOURLY TARIFF
  one increase() over the window        per-half-hour counter deltas
           x one rate                   each x that half hour's own rate
  ┌────────────────────────────┐        ┌──┬──┬──┬──┬──┬──┬──┬──┬──┬──┐
  │        2.80 kWh            │        │.1│.1│.1│.1│.1│.1│.1│.1│.1│..│ kWh
  └────────────────────────────┘        ├──┼──┼──┼──┼──┼──┼──┼──┼──┼──┤
           x 20.89p                     │23│21│18│ 9│-2│-3│-2│ 4│17│..│ p
  ───────────────────────────────       └──┴──┴──┴──┴──┴──┴──┴──┴──┴──┘
  = £0.6150  (exact, one query)         = Σ  (exact, one 48-bucket query)
  attribution: flat_rate                attribution: counter_slot
```

`attribution` is on the wire because the **method** is load-bearing for how closely a
figure should be read:

- **`flat_rate`** — one whole-window energy figure times one rate. Exact over any
  window, and the path a flat deployment keeps taking, byte for byte. A month's bill
  stays one query per device rather than 1488 buckets for the same answer.
- **`counter_slot`** — each half hour's counter delta priced at that half hour's own
  rate. Chosen over two alternatives and recorded in `docs/per-device-attribution.md`.
  Per-device costs sum *exactly* to `energy_cost`, and *when* a device ran is preserved,
  which is the entire point of the tariff. The cost is that plug counters tick in
  0.1 kWh steps, so a single day's figure for a low-draw device carries a few percent of
  quantisation noise — measured at +1.6% over a month for continuous loads against
  −9.7% for deferrable ones, roughly 6:1 signal to noise, which is why the decision went
  this way.

A window spanning a **switchover** is billed one segment per agreement, each at its own
rate and VAT multiplier, with the boundary half hour belonging to the *later* tariff.
The VAT multiplier is **not** constant across time: see
[the runbook for the temporary zero rate, 1 Oct 2026 – 31 Mar 2027](#runbook-the-temporary-zero-rate-of-vat-1-oct-2026--31-mar-2027).
Any comparison spanning that boundary that assumes one VAT rate throughout is wrong,
and nothing in a response will say so — the figures are correct, the assumption is not.
The standing charge is apportioned the same way — each side's daily rate for its own
days, pro rata for a partial day — and is charged **once, on the bill, never split
across devices**: no device causes a standing charge, so apportioning it would invent a
number that reads like a measurement.

Two fields exist because a cost on its own is not interpretable:

- **`effective_rate`** — VAT-inclusive GBP/kWh actually paid, `cost ÷ priced kWh`. On the
  bill it is the single number saying how well the house played the curve. It can be
  **negative**: Agile prices go below zero, and consuming then is a credit, which the
  whole pipeline carries through rather than clamping.

  The same is true **per bucket**: `/series` returns a negative `cost` in a bucket whose
  slots priced below zero. A consumer doing `sum(abs(cost))`, `max(0, cost)` or a
  log-scale chart is wrong exactly there — and those are the buckets a price-response
  analysis cares most about.
- **`unpriced_kwh`** — energy in half hours no rate is held for. It is **not** folded
  into `cost` and is absent when zero, so its presence always means the answer is
  incomplete. Charging nothing for real energy is the silent failure this path exists to
  prevent: a visible gap beats a plausible total.

All four price routes carry an `ETag` and honour `If-None-Match`, answering `304` when
nothing that determines the answer has moved — the tariff, the window truncated to the
slot grid, and the prices themselves. Not `generated_at`, which changes every request:
hashing the rendered body meant the 304 could never fire and a dashboard re-downloaded
every slot on every poll.

`/prices` and `/prices/stats` **refuse a window spanning a tariff change** (400, naming
the boundary). A curve belongs to one tariff, and answering about only the first half is
what `/bill` — which does segment, because a cost can be summed across tariffs where a
curve cannot — would then contradict. They also cap the window: 31 days for `/prices`
(a row per half hour) and 366 for `/prices/stats` (a row per day). Note these are stated
in **days**, while `/series` caps in **buckets** (~1000) — a join across both is chunked
by two different rules.

What counts as "a tariff change" is the **curve identity**, not the number of agreement
blocks. An agreement split that leaves the tariff code unchanged — see the VAT runbook
above — is served normally, because the archive holds the supplier's own inc-VAT prices
and a tax change simply arrives in them. A **flat** tariff across a VAT change is still
refused: `flat_price` is one inc-VAT number derived from the config rate, and it
genuinely differs either side.

`/prices/stats` emits **both VAT bases**: the unsuffixed keys are inc-VAT, matching the
other price routes, and `*_exc_vat` siblings carry the analytical figures.

A **flat-rate** tariff is a valid state on the price routes, not a service failure.
`/prices`, `/prices/stats` and `/prices/upcoming` all answer `200` with
`half_hourly: false` and a `flat_price`, their curve array (`slots`/`days`) present and
empty — the same window must not be a `200` on one of these and a `503` on its
neighbour. `/prices/cheapest` is the deliberate exception: it still refuses, because
every window ties under a flat rate, so naming one would read as a recommendation.
A window that **no agreement covers** is a `503` on all four, and says so in those
words rather than blaming half-hourliness.

`/prices` carries **`known_to`** (the archive horizon, independent of the window asked
for) and **`summary.current`** (the price of the slot covering now, absent when none
does). Both exist so a live dashboard can draw retrospective context *and* watch for the
daily publication from one call — `/prices/upcoming` is forward-only, so a chart showing
the last few hours previously needed both routes to learn one number. `known_to` is part
of the ETag, so a publication that extends the horizon without touching a past window's
slots still invalidates it.

On the price routes `window=today` means the **whole local day**, not the elapsed part
as on the consumption routes. Today's prices are published in full before today begins,
so a to-date curve would hand a dashboard half a chart and report it `complete`. `week`
and `month` stay period-to-date, since the supplier publishes only about a day and a
half ahead.

`/healthz` carries a `reasons` array naming every failing condition, sorted and omitted
when healthy.

### What `/bill` covers

`energy_cost` is the sum of the device rows, and `total` is that plus the standing
charge. **Both exclude the unmonitored remainder.** On a home where the meter sees
roughly twice what the monitored plugs do, `/bill.total` is about **45% of the
household bill** — and the endpoint is called `/bill`, so it gets quoted.

The existing fields keep their exact meanings; a silent numerical change to `total`
would be worse than the ambiguity it fixes. What is added is the scope, stated, and the
household figures beside it:

```json
{
  "scope": "monitored_devices",
  "energy_cost": 21.2032,
  "standing_charge": 11.832,
  "total": 33.0352,

  "household_energy_cost": 61.9237,
  "household_total": 73.7557,

  "reconciliation": {
    "meter_present": true,
    "monitored_kwh": 209.3,
    "meter_kwh": 418.336,
    "unmonitored_kwh": 209.0,
    "unmonitored_cost": 40.7205,
    "unmonitored_priced_kwh": 209.0,
    "coverage": 0.5074
  }
}
```

`household_total` is **top-level, not inside `reconciliation`**. Both fields are new, so
their placement cannot threaten `total`'s meaning — and burying the real figure inside
the block that explains the gap is how it got missed in the first place.

`unmonitored_cost` is priced through the **same pricer the device rows went through**,
from the same build: under a half-hourly tariff that is per half hour at each half
hour's own rate, not the remainder at some average. It is the same quantity
`/series?group_by=house` reports as the `unmonitored` series' cost, so the chart and the
bill cannot disagree.

`unmonitored_priced_kwh` is the energy that cost was computed from, and it is **not
always `unmonitored_kwh`**. `unmonitored_kwh` is `meter − monitored` over the whole
window, signed; the bucketed path prices the per-bucket residual with each bucket
clamped at zero (see [When the parts do not sum to the
meter](#when-the-parts-do-not-sum-to-the-meter)). Reporting both keeps the pair
self-consistent instead of implying an effective rate nobody charged.

With **no meter** configured there is no remainder to price, so `household_energy_cost`
and `household_total` are omitted rather than sent as a copy of the monitored total.
`scope` is still present, because what `total` covers is worth saying either way.

### The price behind each bucket

`/series` returns `cost`, but cost is `kwh × price` — and you cannot recover the price
from a bucket where `kwh` is zero. Those are exactly the buckets that answer *"it was
cheap and we did **not** use it"*, which is what a load-shifting analysis is really
asking. So `prices=true` returns the factor alongside the product:

```
GET /series?window=7d&interval=30m&group_by=house&prices=true
```

```json
{
  "buckets": ["2026-08-24T00:00:00+01:00", "2026-08-24T00:30:00+01:00", "..."],
  "prices": [0.13977, 0.12104, null, "..."],
  "price_unit": "GBP/kWh",
  "price_vat_included": true,
  "price_basis": "slot",
  "unpriced_buckets": 1,
  "tariff_codes": ["E-1R-AGILE-24-10-01-C"],
  "series": [{ "key": "meter", "kwh": [], "cost": [], "avg_w": [] }]
}
```

It is **opt-in**, so no existing payload grows, and it is on `/devices/{id}/series` and
`/devices/unmonitored/series` too.

**`len(prices) == len(buckets)`, same order, always.** That is the contract to zip
against. A `null` means **no rate is held** for that bucket, never free — the same
distinction `unpriced_kwh` draws — and `unpriced_buckets` counts them, so `0` is a
positive assertion that the window is fully priced.

Each bucket is priced over **the part of it the window covers**, not over its nominal
span. The axis is built on calendar boundaries, so a `custom` window starting mid-bucket
has a first bucket labelled *before* the window begins — and since its `kwh` and `cost`
already describe only the covered part, its price has to as well, or the three do not
belong in one row.

**Pounds, not pence.** The price family speaks `p/kWh`; this array speaks `GBP/kWh`,
because it sits beside `cost[]` and `kwh[]` in the same response and self-consistency
inside one payload beats consistency with a different endpoint. `price_unit` says so
either way.

`price_basis` says how each value was arrived at, and it is the field that makes the
array safe to do arithmetic with:

| `price_basis` | when | value |
|---|---|---|
| `slot` | the bucket sits inside one rate interval (`5m`, `15m`, `30m` against a half-hourly tariff) | that interval's own rate — exact |
| `mean_over_bucket` | the bucket spans several (`1h`, `6h`, `1d`) | the **time-weighted** mean across them |
| `flat` | one rate covers all time | that rate, repeated |

**The identity `cost[i] == kwh[i] × prices[i]` holds only when `price_basis` is
`slot`.** At coarser buckets the cost accumulates on the rate grid while the reported
price is a mean over the bucket, so the identity is deliberately false there — stated
here because an unstated arithmetic relationship is how the old string join failed.

The mean is **time-weighted, not energy-weighted**: energy-weighted is `cost ÷ kwh`,
which is undefined in a zero-kWh bucket, and those are the buckets that matter most
here. A coarse bucket is priced only when **every** interval inside it is; otherwise it
is `null`, because a mean over the slots that happen to be held is a plausible-looking
wrong number.

`tariff_codes` is plural because a `/series` window **may** span a switchover. `/prices`
refuses one — a price *curve* is a property of a single tariff — but a per-bucket array
is not a curve: each bucket belongs to exactly one tariff, so every value in it is
honest.

**Fixed agreements are named too**, so `len(tariff_codes)` can be trusted as the number
of tariffs in the window — the natural reading of a plural array. Each entry is the
supplier tariff code where there is one, the agreement's own `id` otherwise, and its
`name` when a fixed block carries neither. Entries de-duplicate by that label, so a
VAT-only agreement split — two blocks describing one tariff, as in the
[zero-rate runbook](#runbook-the-temporary-zero-rate-of-vat-1-oct-2026--31-mar-2027) —
collapses to a single entry, because curve identity is what makes a tariff change rather
than block count.

### When the parts do not sum to the meter

`unmonitored` is `clamp(meter − monitored)` **per bucket**, so a bucket where monitored
reads above the meter contributes `0` rather than that negative. Real counter
quantisation produces such buckets routinely, and over a window the grouped parts plus
the catch-all therefore **overshoot** the meter — by a couple of pence on a real home,
small enough to look like float noise and not that.

`/series` reports that overshoot as a top-level `clamp` block whenever it is non-zero:

```json
"clamp": { "kwh": 0.132, "buckets": 7, "drift_buckets": 0 }
```

`clamp.kwh` is exactly `parts − meter`, so the discrepancy is always accounted for, and
the block's **absence means the parts sum exactly** — the same convention as
`unpriced_kwh`. It is carried on `shape=rows` too, because shape is a rendering choice
and must not change what a response explains, and it is **not** house-only: the
`include_unmonitored` catch-all is clamped the same way.

The two counts answer different questions. `buckets` counts every clamped bucket and is
the honest denominator for the gap. `drift_buckets` counts only those beyond one counter
quantum (0.1 kWh) — the data-quality alarm for a monitored device over-counting, a
mis-scaled meter, or skewed clocks. `drift_buckets: 0` alongside a non-zero `buckets`
means the gap is entirely routine quantisation, which is the common case. Merging the
two would turn routine noise into a fault report and hide a real fault inside routine
noise.

One consequence of `omitempty` worth knowing before you depend on it: **absence means
"nothing was clamped" only on a server that has this field at all.** A server predating
it omits the block for a different reason, and the two are the same bytes on the wire.
That is true of every additive field, and the general answer is the same — ask
`/openapi.json`, which lists `clamp` only where it exists. It is called out here rather
than left implicit because this particular absence is load-bearing: a consumer reading
it as "the parts sum exactly" on an older server would draw precisely the wrong
conclusion, which is the failure the block was added to prevent.

`unclamped=true` applies no clamp — it serves the raw signed residual, negatives
preserved — so it carries no `clamp` block, and its parts deliberately do not sum.

`/series` and `/bill` price buckets through **one** shared function, so a chart and a
bill cannot disagree about what a window cost. Totals accumulate at full precision and
round once — summing per-bucket costs already rounded for the wire drifts a month's
half-hourly bill by several pence.

Sharing that function is necessary but not sufficient, because **`/series` buckets are
usually coarser than the price grid**: the default interval is `1h` for `window=today`
and `1d` for `week`/`month`, while there are 48 prices a day. So when the tariff is
half-hourly, the money is computed on the **30-minute grid regardless of the requested
interval** and the costs are then folded up into the display buckets:

```
  requested 1d  ─────────────────────────────────────────────▶  1 display bucket
  costed on     ┌──┬──┬──┬──┬──┬──┬──┬──┬──┬──┬──┬──┬──┬──┬──┐
                │  │  │  │  │  │  │  │  │  │  │  │  │  │  │  │  48 × 30m, each at
                └──┴──┴──┴──┴──┴──┴──┴──┴──┴──┴──┴──┴──┴──┴──┘  its own rate
                                   │
                                   └─▶ summed into cost[0], exact
```

`kwh[]` keeps the resolution the caller asked for and `cost[]` is exact, so `/series`
agrees with `/bill` **by construction** rather than at one particular interval. Pricing
a coarse bucket at the rate holding at its start instant measured **+43.6%** on a real
recorded day and understates badly on a typical cheap-night/dear-evening one.
Approximating instead — spreading a bucket's energy evenly over its slots — is exact
for a fridge and badly wrong for a dishwasher, which is the load the tariff exists to
shift. A **flat** tariff skips all of this: the price cannot change inside a bucket, so
a monthly chart stays one query per day.

## Run locally

```sh
make build
./bin/countinghouse -config /path/to/config.yaml
```

The binary boots even if Influx/identity are unreachable (`/healthz` still serves; data routes
degrade gracefully). Logs are structured JSON on stderr.

## Demo consumer apps

Browser consumer demos for countinghouse (and the other swee.net read-side
services) live in the sibling **`consumer-demos`** repo (`../consumer-demos`),
alongside a shared dev token broker that handles id.swee.net auth. See its README
to run them — countinghouse must be running locally (default base
`http://localhost:8585`).

## Development

```sh
make test        # go test -race -count=1 ./...
make lint        # go vet ./...
make lint-spec   # spectral lint of the OpenAPI spec
make fmt         # gofmt -w .
```

CI (`.github/workflows/ci.yml`) runs build, vet, `test -race`, gofmt, staticcheck, and spectral
on push/PR to `main`.

**House rules:** run `gofmt` before committing; keep **both** the OpenAPI spec *and this README*
in sync when endpoints or behaviour change; write a failing test first for bug fixes.

## Deploy

Prod host is `garibaldi` (systemd). Deploy only when asked:

```sh
./deploy/deploy.sh sweeney@garibaldi
```

One-time host setup is in `deploy/install.sh` (creates the user/dirs/config/unit) and
`deploy/sudoers.sh`. See `PLAN.md` §14 for the deploy-time prerequisites (Influx read token,
identity client credentials).

**When a deploy ends with the service down**, the script does not just dump the journal: it
renders the service's own refusal (including the config block to add), classifies the
cause, and says whether it will fix itself. The distinction that matters at 3am is
self-healing vs not — an unreachable config service recovers on the unit's 5s restart
loop, while a missing config key or a 404 namespace never will. It also prints the
rollback command for the previous build, with the caveat that fits the cause: after a
cold-namespace failure, rolling back is the *wrong* move, because an older build boots
happily and serves the empty snapshots this one refuses to.

There is deliberately **no preflight check** on the host's config. A deploy that would
fail is allowed to fail; it just has to explain itself.
