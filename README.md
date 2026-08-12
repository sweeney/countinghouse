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
| `GET /devices` | Device catalog (id, display_name, room, `covers`, class, `capabilities`: `energy`/`events`). Includes a synthetic `unmonitored` (rest-of-home) energy device when a whole-house meter is configured. |
| `GET /devices/{id}/energy?window=&from=&to=` | Windowed kWh for one device (`source`: counter/integral). |
| `GET /devices/{id}/cost?window=…` | Windowed kWh + VAT-inclusive cost at the effective tariff. |
| `GET /devices/{id}/series?window=&interval=&shape=` | Single-device time-series (kWh / cost / avg W per bucket). Reserved id `unmonitored` serves the rest-of-home series in the same shape (404 when no meter is configured). |
| `GET /devices/{id}/events?window=` | State-transition events (for vertical-line overlays). |
| `GET /devices/{id}/intervals?window=` | Derived on/off spans + duty stats. |
| `GET /series?window=&interval=&group_by=&include_unmonitored=&shape=` | Multi-series time-series. `group_by`: `device` (default), `room`, `class`, `house` (three series: `monitored` + `unmonitored` + `meter`, where `unmonitored` = clamp(meter − monitored) per bucket). `house` also returns top-level `coverage` (monitored ÷ meter) and `stale_monitored_count`/`stale_monitored_ids` (monitored devices with no telemetry in the window) as confidence signals. `include_unmonitored=true` adds the rest-of-home as one catch-all series to `device`/`room`/`class` groupings so the parts sum to the meter. `unclamped=true` is a diagnostic mode that returns the raw signed `meter − monitored` (negatives preserved) instead of clamping at 0. |
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

For a `window=custom` whose `from` is **not** on an interval boundary, the bucket axis
snaps **down** to the interval grid (anchored at local midnight) so it matches Influx's
aggregation boundaries — e.g. `from=14:23` with `interval=1h` yields buckets starting at
`14:00, 15:00, …`. The first bucket is widened to its grid boundary (the pre-`from` slice
carries no in-window data). Period-to-date windows (`today`/`week`/`month`) already start
at local midnight, so they are unaffected.

The OpenAPI document (`internal/httpapi/openapi.yaml`) is the source of truth for request
and response schemas; a path-coverage test fails CI if routes and spec drift.


## Rooms

`location` used to mean two different things across these services — a geographic site
and a room — so rooms are now `room`, sites are `site`, and floors are `floor`. Room ids
are `<floor>.<slug>`: `groundfloor.kitchen`, `basement.network-cabinet`.

The deprecated `location` spelling has been removed: `group_by=location` and the
`location` response field are both gone. Use `group_by=room` and `room`.

**Whole-property devices have no room.** The electricity meter, central heating and
hot water report an empty `room` and `covers: "house"` — on `/devices` and in the
`/bill` breakdown — so an empty room is legible rather than mysterious.

A device may also declare `covers`. Under `group_by=room` a device covering the whole
property is grouped under a **`house`** key rather than the room it sits in — putting
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
```

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
