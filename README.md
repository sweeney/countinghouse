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
| `GET /devices/{id}/cost?window=…` | Windowed kWh + VAT-inclusive cost at the effective tariff. |
| `GET /devices/{id}/series?window=&interval=&shape=` | Single-device time-series (kWh / cost / avg W per bucket), for any energy-capable device **including the whole-house meter** (excluded from `/series?group_by=device`, but a request for one device cannot double-count). Reserved id `unmonitored` serves the rest-of-home series in the same shape (404 when no meter is configured). |
| `GET /devices/{id}/events?window=` | State-transition events (for vertical-line overlays). |
| `GET /devices/{id}/intervals?window=` | Derived on/off spans + duty stats. |
| `GET /series?window=&interval=&group_by=&rooms=&floors=&include_unmonitored=&shape=` | Multi-series time-series. `group_by`: `device` (default), `room`, `floor` (the sum of its rooms), `class`, `house` (three series: `monitored` + `unmonitored` + `meter`, where `unmonitored` = clamp(meter − monitored) per bucket). `house` also returns top-level `coverage` (monitored ÷ meter) and `stale_monitored_count`/`stale_monitored_ids` (monitored devices with no telemetry in the window) as confidence signals. `include_unmonitored=true` adds the rest-of-home as one catch-all series to `device`/`room`/`floor`/`class` groupings so the parts sum to the meter. `rooms=`/`floors=` (CSV) narrow which devices the response covers; an id holding no billed device is a `400`, and neither may be combined with `include_unmonitored=true` or `group_by=house`. `unclamped=true` is a diagnostic mode that returns the raw signed `meter − monitored` (negatives preserved) instead of clamping at 0. |
| `GET /events?devices=&class=&window=&group_by=` | Multi-device event overlay. `group_by`: `device` (default) / `class`. |
| `GET /bill?window=month` | Per-device cost breakdown + standing charge + total + reconciliation vs the whole-house meter. When no meter is configured, `reconciliation.meter_present` is `false` and `meter_kwh`/`unmonitored_kwh`/`coverage` are omitted. |
| `GET /tariffs` | Current tariffs keyed by fuel (electricity, gas). |
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
counter to read and both endpoints estimate an integral. They now estimate it the *same*
way — `integral(unit: 1h, interpolate: "linear")`, whole-window for `/devices/{id}/energy`
and per bucket for the series — so for a UPS that is reporting they agree. Until recently
the series used `mean(power_w) × bucket_hours` instead, a sample mean that weights every
reading equally however long it stood; for samples bunched into the start of a bucket that
overstated the bucket by more than a factor of three.

`avg_w` for a UPS is derived back out of that energy (`kwh × 1000 / bucket_hours`), so it is
the bucket's **time-weighted** mean power and cannot contradict the `kwh` printed beside it.
For a steady load on a regular cadence this is the same number the sample mean gave.

One limit remains, and it is the one case where a UPS series and `/devices/{id}/energy` still
part company: a bucket the UPS reported **nothing** in has nothing to integrate and publishes
`0`, while the whole-window integral interpolates straight across the outage and counts the
load. The series is the low one, by roughly the length of the outage. Closing that needs the
readings either side of the gap, which a per-bucket query cannot see.

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
