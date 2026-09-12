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
| `GET /series?window=&interval=&group_by=&rooms=&floors=&include_unmonitored=&shape=` | Multi-series time-series. `group_by`: `device` (default), `room`, `floor` (the sum of its rooms), `class`, `house` (three series: `monitored` + `unmonitored` + `meter`, where `unmonitored` = clamp(meter − monitored) per bucket). `house` also returns top-level `coverage` (monitored ÷ meter) and `stale_monitored_count`/`stale_monitored_ids` (monitored devices with no telemetry in the window) as confidence signals — only this grouping does, `/devices/unmonitored/series` included. `include_unmonitored=true` adds the rest-of-home as one catch-all series to `device`/`room`/`floor`/`class` groupings so the parts sum to the meter. `rooms=`/`floors=` (CSV) narrow which devices the response covers; an id holding no billed device is a `400`, and neither may be combined with `include_unmonitored=true` or `group_by=house`. `unclamped=true` is a diagnostic mode that returns the raw signed `meter − monitored` (negatives preserved) instead of clamping at 0. |
| `GET /events?devices=&class=&window=&group_by=` | Multi-device event overlay. `group_by`: `device` (default) / `class`. |
| `GET /bill?window=month` | Per-device cost breakdown + standing charge + total + reconciliation vs the whole-house meter. Carries `attribution`, `effective_rate` and `unpriced_kwh` as above; per-device costs sum exactly to `energy_cost`. When no meter is configured, `reconciliation.meter_present` is `false` and `meter_kwh`/`unmonitored_kwh`/`coverage` are omitted. |
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
(rolling windows default by span) and a ~1000-bucket cap.

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

The devices namespace is named by config, so a site reads its own:

```yaml
site:
  id: home
  devices_namespace: devices_home
  floorplan_namespace: floorplan_home
```

**`floorplan_namespace` is required too**, for a quieter version of the same reason.
Omitting it breaks nothing: `/floors` and `/rooms` still list every floor and room
holding a metered device, and every kWh and cost is exactly right. Only the **names** are
lost — so those endpoints answer with ids where labels belong and `null` where storey
order belongs, which is precisely what a floorplan publishing nothing would produce.
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

**`devices_namespace` is required, and the service refuses to start without it.** It
briefly defaulted to `statehouse_devices`, the shared namespace every service read
before devices were split per site. That namespace has been deleted from the config
service, so the default came to name a document that returns 404 — and every layer below
handles that correctly into silence: the fetch fails, the refresh is fail-open and keeps
the last-known snapshot, at startup there is no last-known snapshot, and every endpoint
then reports zero devices. For a billing service that is a wrong answer in the shape of a
right one, so an unnamed namespace is now a refusal to boot rather than a warning.

Both keys are given explicitly. `devices_namespace` is deliberately *not* derived from
`id`: a namespace is a document that either exists or does not, and guessing its name
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
  (the devices namespace named by `site.devices_namespace`, and `energy_tariffs`) via
  `client_credentials`. Fetches are fail-open and reload on `SIGHUP`.
- **`/healthz.remote_config`** is keyed by the namespace actually read, so the devices
  entry is named by `site.devices_namespace` — `devices_home` for this site. There is no
  default: a config naming no namespace does not start. A monitor keyed on a literal
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

No backfill step is needed: a first sync against an empty archive requests an unbounded range
and so pulls the supplier's whole published history in one pass (measured: 34,894 slots in
6.5 s). Afterwards each sync fetches only what is new, detected by a single ~350-byte probe.

`GET /healthz` and `GET /metrics` gain a `prices` block, one entry per collected tariff —
omitted entirely when no collector runs. Two fields answer different questions, and the
difference matters:

| field | means | use |
|---|---|---|
| `known_to` | end of the newest slot held | "prices are arriving" |
| `complete_to` | end of the newest **fully populated** local day | **"we can bill this far"** |

Alert on `complete_to`. A publication routinely advances the horizon across a whole day while
leaving that day two slots short, so `known_to` alone will tell you yes when the answer is no.
`complete_to` at or behind now means today cannot be priced in full, and degrades the
top-level `status`.

### Backing up the archive

The archive is the one thing countinghouse writes and the only state here that is not
rebuildable from Influx, so it is the one thing that gets a backup rather than a
retention policy. `prices.backup` sends it to Cloudflare R2 via `identity/common/backup`,
on a schedule, using `VACUUM INTO` — a consistent snapshot, so a backup can run while the
collector is writing.

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
- The snapshot is staged under `/tmp` and deleted after upload. The systemd unit sets
  `PrivateTmp=true`, so that copy of the archive is the service's alone — worth preserving
  if the hardening is ever revisited.
- `deploy/bootstrap.sh` creates `/etc/countinghouse/r2-secret` (0640, `root:countinghouse`)
  empty. The R2 token itself is minted in the Cloudflare dashboard; scope it to this bucket
  alone.

`/healthz` and `/metrics` gain a `backup` block — omitted entirely when none is configured,
so a zeroed block never reads as a broken backup:

```json
"backup": {
  "bucket": "countinghouse-sqlite", "env": "production",
  "schedule": "daily", "hour": 3,
  "last_attempt": "…", "last_success": "…",
  "last_key": "production/backups/countinghouse/2026/09/12/countinghouse-….sqlite3",
  "successes": 9, "failures": 0
}
```

`last_attempt` moves on every run, `last_success` only on one that worked — so a lagging
`last_success` means we are failing *now*, and `last_key` is how the newest backup is found
without listing the bucket. Three states **degrade** the top-level status (never make it
`unavailable`: nothing served depends on last night's upload):

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
  "summary": { "slots": 24, "current": 45.85, "min": -2.62, "mean": 24.48, "max": 50.22 },
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
  fell to 24.48p against a median of 28.42p, which banded **25 of 48 slots as peak**
  and diluted the signal to nothing. The median shrugs that off — and plunge days are
  exactly the days these endpoints exist for, so the statistic has to survive them.
- **`rank` 1 is the cheapest**, because the question is "when should I run this".
- **`percentile`** is served too, so a consumer that dislikes our thresholds can band
  it differently without refetching.

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
25-hour day makes sense — and is **ex-VAT**, unlike the curve endpoints, because these
are analytical values compared against each other rather than a price on a screen.
`vat_included` states it either way. `spread` is max − min: the single number saying
whether shifting load that day was worth the bother.

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
The standing charge is apportioned the same way — each side's daily rate for its own
days, pro rata for a partial day — and is charged **once, on the bill, never split
across devices**: no device causes a standing charge, so apportioning it would invent a
number that reads like a measurement.

Two fields exist because a cost on its own is not interpretable:

- **`effective_rate`** — VAT-inclusive GBP/kWh actually paid, `cost ÷ priced kWh`. On the
  bill it is the single number saying how well the house played the curve. It can be
  **negative**: Agile prices go below zero, and consuming then is a credit, which the
  whole pipeline carries through rather than clamping.
- **`unpriced_kwh`** — energy in half hours no rate is held for. It is **not** folded
  into `cost` and is absent when zero, so its presence always means the answer is
  incomplete. Charging nothing for real energy is the silent failure this path exists to
  prevent: a visible gap beats a plausible total.

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
