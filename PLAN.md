# Countinghouse — Design & Implementation Plan

Status: design finalized 2026-06-11. Core unknowns resolved against live systems
(Influx on garibaldi, config.swee.net, statehouse source). This is the build plan;
code follows the milestones in §13.

Countinghouse is a **read-side energy cost/accounting service**. It turns the
per-device telemetry statehouse writes to InfluxDB into per-device kWh and cost
over arbitrary windows, and decomposes the electricity bill device-by-device.

---

## 1. Invariants (from CLAUDE.md / AGENT_BRIEF — do not violate)

- **Read-side only.** No MQTT, no ingest, no real-time state. Query Influx + apply tariffs.
- **Stateless w.r.t. accumulation.** Derive on query; never hold running totals. Truth
  lives in device-side Influx counters, so the service survives restart with zero data
  loss. Any future cache must be rebuildable from Influx.
- **Mirror statehouse conventions** (`../statehouse`) unless noted: HTTP shape, config,
  auth, test discipline.

---

## 2. Resolved decisions (was AGENT_BRIEF §7 + discovery)

| Topic | Resolution |
|---|---|
| Influx bucket / org | bucket **`statehouse`** (migrated from `statehouse_test`), org `swee.net`, **2-year** retention. Config values, not hardcoded. |
| Counter vs integral routing | By **`energy_kwh` presence** (verified live): 16 plug devices + `electricity_meter` → counter; 2 UPS → integral. `energy_strategy` is irrelevant to this. |
| Standing charge | **Total only** — in the bill total, not attributed per-device. |
| Window timezone | **Europe/London** (configurable via `house.timezone`), week starts Monday. |
| Compute model | **Query-on-demand**, fully stateless. Cache only if long-window latency proves bad, and only if rebuildable. |
| `energy_tariffs` | **Already exists** in config.swee.net — do not create. £ units, ex-VAT. |
| VAT | Stored rates are **ex-VAT**; apply `×(1 + vat_rate)` (5%). |
| Tariff history | **Current rate only** in v1; effective-date history needs a schema extension later (seam left in code). |

Still open (deploy-time only): countinghouse Influx **read token** (scoped to
`statehouse` bucket); countinghouse **`client_id`/`client_secret`** in id.swee.net.

---

## 3. Module & dependencies

`go.mod`: module `github.com/sweeney/countinghouse`, **go 1.25.0** (match statehouse,
not local 1.26). Pin to statehouse's versions:

- `github.com/sweeney/identity/common v0.2.0` — `auth.JWKSVerifier` (inbound) + `spec.Converter` (OpenAPI)
- `github.com/influxdata/influxdb-client-go/v2 v2.14.0` — **query side** (`QueryAPI`)
- `gopkg.in/yaml.v3 v3.0.1`

No MQTT / no Influx-write deps.

---

## 4. Package layout

```
cmd/countinghouse/main.go        startup, SIGHUP reload, graceful shutdown
internal/config/
  config.go                      local YAML (Load/Default), InfluxConfig, IdentityConfig, RemoteConfig, House.Timezone
  device.go                      DeviceConfig (mirrors statehouse), normalise
  remote.go                      Fetcher (statehouse_devices + energy_tariffs), TokenSource iface, fail-open, per-ns status
  tariffs.go                     EnergyTariffs/Tariff structs + current-rate selection (seam for history)
internal/influx/
  client.go                      real queryClient over influxdb2 QueryAPI; Ping
  query.go                       queryClient interface + Flux builders (counter / integral)
  fake.go                        FakeQueryClient test double (canned rows keyed by device/field)
internal/energy/
  window.go                      today|week|month|custom -> [start,stop) in tz, Clock-driven
  energy.go                      route by class/energy_kwh presence -> windowed kWh per device
  cost.go                        kWh*rate*VAT, standing charge, bill assembly + reconciliation
internal/httpapi/
  server.go                      Server struct, newMux, Start(ctx), writeJSON, /healthz
  auth.go                        authMiddleware: Parse() OR ParseServiceToken()
  spec.go                        //go:embed openapi.yaml, spec.Converter, /openapi.json
  handlers.go                    device energy/cost, bill, tariffs, metrics
  openapi.yaml                   hand-maintained
  spec_test.go                   route/spec path-coverage test (hard rule)
internal/testutil/
  clock.go                       Clock / RealClock / FakeClock
  ptrs.go                        PtrF64 etc.
deploy/
  deploy.sh, countinghouse.service
Makefile                         test = go test -race -count=1 ./...
config/config.example.yaml
```

---

## 5. Influx data (verified live)

Statehouse is **write-only** to Influx; countinghouse's query layer is net-new. Shared
client (`influxdb2.NewClient(url, token)`) but `.QueryAPI(org)`.

Measurement **`device_power`** — tags `device_id`, `class`, `location`. Fields
`power_w`, `voltage_v`, `energy_kwh`, **each written as its own single-field point**
(they do NOT share rows/timestamps — query builders and the fake must reflect this).

Measurement **`house_electricity`** — tag `scope="whole_house"`; fields incl.
`gross_w`/`monitored_w`/`unmonitored_w`/`coverage` and `today_kwh`/`week_kwh`/`month_kwh`
(meter's authoritative period totals). Used for bill reconciliation. `session_*_kwh`
resets on restart — do not use.

Device inventory (from `statehouse_devices`): 16 plug devices (5 continuous, 5 cycle,
2 short_burst, 4 media) + 2 UPS (`network-ups`, `office-ups`) + `electricity_meter`.

### Query paths

Counter (16 plug + meter — have `energy_kwh`):
```flux
from(bucket: "statehouse")
  |> range(start: windowStart, stop: windowStop)
  |> filter(fn: (r) => r._measurement == "device_power" and r._field == "energy_kwh")
  |> filter(fn: (r) => r.device_id == "winefridge")
  |> increase()
  |> last()
```

Integral (2 UPS — `power_w` only):
```flux
from(bucket: "statehouse")
  |> range(start: windowStart, stop: windowStop)
  |> filter(fn: (r) => r._measurement == "device_power" and r._field == "power_w")
  |> filter(fn: (r) => r.device_id == "network-ups")
  |> integral(unit: 1h, interpolate: "linear")
  |> map(fn: (r) => ({ r with _value: r._value / 1000.0 }))   // W·h -> kWh
```

Routing rule: device has `energy_kwh` (class in {continuous,cycle,short_burst,media}_power_device
or energy_meter) → counter; class `ups_sensor` → integral. **Ignore `energy_strategy`.**

Watch-outs (tested): counter resets (`increase()`), UPS offline gaps (integral tolerant),
`coverage` outside 0–1 (don't clamp), stale devices.

---

## 6. Config

**Local YAML** (`/etc/countinghouse/config.yaml`) via `config.Load` (copy statehouse:
`token_file` fallback, `time.LoadLocation` validation):
```yaml
http:    { listen: ":8585", public_url: "https://countinghouse.swee.net" }
influx:  { url, org: "swee.net", bucket: "statehouse", token_file: /etc/countinghouse/influx-token }
identity:{ base_url: "https://id.swee.net", client_id, client_secret }
remote_config: { base_url: "https://config.swee.net" }
house:   { timezone: "Europe/London" }
```

**Remote config** via `Fetcher` (copied from statehouse: `TokenSource` iface, Bearer,
fail-open, 401→`Invalidate()`, per-namespace `Statuses()` on `/healthz`). Two namespaces:
- `statehouse_devices` — for `class`/`location`/`display_name` (drives routing + grouping).
- `energy_tariffs` — pricing (below).

`DeviceConfig` mirrors statehouse (`scheme`/`primary`/`ieee_address`/`friendly_name`/
`class`/`display_name`/`location`/`thresholds`/`energy_strategy`); we read class/location/
display_name only. We do **not** need `statehouse_classes` (routing is class-derived).

---

## 7. Tariffs & cost

`energy_tariffs` actual shape:
```json
{ "tariffs": {
    "electricity": { "unit_rate": 0.2089, "daily_standing_charge": 0.5294, "unit": "kWh", "vat_rate": 0.05 },
    "gas": { ... ignored ... } } }
```
Use `tariffs.electricity` only; ignore gas. Units are **£** (not pence). GBP.

```go
type Tariff struct {
    UnitRate            float64 `json:"unit_rate"`             // £/kWh, ex-VAT
    DailyStandingCharge float64 `json:"daily_standing_charge"` // £/day, ex-VAT
    Unit                string  `json:"unit"`
    VATRate             float64 `json:"vat_rate"`
}
type EnergyTariffs struct{ Tariffs map[string]Tariff `json:"tariffs"` }
```

Cost math:
- Per-device cost = `kWh × unit_rate × (1 + vat_rate)`
- Standing charge (total only) = `daily_standing_charge × days_in_window × (1 + vat_rate)`
- Bill total = Σ per-device costs + standing charge

`tariffs.go` exposes `tariffFor(t time.Time) Tariff` returning the single current tariff,
with a TODO/seam for effective-date history (extend schema with `effective_from`/`previous`
and split windows at boundaries when data spans a rate change).

---

## 8. Windowing

`window.go` resolves `today|week|month|custom(from,to)` to a half-open `[start, stop)`
in `house.timezone`, converted to UTC for Flux. Week starts Monday. All boundary logic
takes an injected `Clock` — **never `time.Now()` in logic** — so FakeClock drives
BST/GMT-edge and month-boundary table tests. `days_in_window` for standing charge is
computed from the resolved range in local time.

---

## 9. HTTP API

Plain `net/http.ServeMux`; handlers are methods on `Server`; routes in `newMux(s)`;
shared `writeJSON` (`SetEscapeHTML(false)`); `Start(ctx)` with 5s graceful shutdown —
all mirrored from statehouse `server.go`.

| Route | Auth | Returns |
|---|---|---|
| `GET /healthz` | public | status, version, started_at/ago, goroutines, influx_reachable, remote_config per-ns status |
| `GET /openapi.json` | public | spec via `spec.Converter`, `__PUBLIC_URL__` substitution |
| `GET /devices/{id}/energy?window=&from=&to=` | yes | `{kwh, source, window}` |
| `GET /devices/{id}/cost?window=...` | yes | `{kwh, cost, currency, tariff}` |
| `GET /bill?window=month` | yes | per-device breakdown (kwh, cost, location, display_name) + totals + reconciliation vs meter |
| `GET /tariffs` | yes | current electricity tariff |
| `GET /metrics` | yes | atomic counters: query count/errors, influx latency |

`/healthz` + `/openapi.json` are `security: []`. **`spec_test.go`** diffs `newMux` routes
(`{id}` normalised) against `openapi.yaml` paths, failing CI on drift (hard rule).
Reconciliation in `/bill`: report `monitored_kwh` (Σ devices), `meter_kwh`,
`unmonitored_kwh` remainder, `coverage` — surfaced, not hidden.

---

## 10. Auth (fix statehouse's service-token gap from day one)

Verified signatures in `identity/common v0.2.0`:
- `verifier.Parse(ctx, token) (*TokenClaims, error)` — user tokens; has `.IsActive`; rejects `typ=at+jwt`.
- `verifier.ParseServiceToken(ctx, token) (*ServiceTokenClaims, error)` — client_credentials; has `.ClientID`/`.Scope`; **successful parse = valid** (no IsActive).

Middleware (disabled when `IdentityURL == ""` for dev/tests):
```go
if c, err := verifier.Parse(ctx, token); err == nil && c.IsActive { next(); return }     // user
if _, err := verifier.ParseServiceToken(ctx, token); err == nil    { next(); return }     // service
reject(401)
```
Outbound: copy `identity.TokenSource` (client_credentials, 30s pre-expiry refresh,
`Invalidate()` on 401) for config fetches.

---

## 11. main.go

`-config` flag → slog JSON to stderr (`SetDefault`) → `config.Load` → build `Fetcher` +
`TokenSource`, `ApplyRemote` (fail-open) → build Influx **query** client → build `Server`
(set `Version`, `IdentityURL`, `PublicURL`, `RemoteConfig`) → `Start(ctx)` with
SIGINT/SIGTERM cancel + 5s shutdown → `watchSIGHUP` re-fetches remote config against a
fresh `baseCfg` copy.

---

## 12. Testing (match statehouse density)

`setup(t)` returns a `Server` wired to `FakeQueryClient` + `FakeClock`. Table/fixture
tests for: windowing across BST/GMT and month boundaries; counter reset via `increase()`;
UPS integration incl. offline gaps; tariff application + VAT; standing-charge math; bill
reconciliation incl. coverage>1 / negative; auth accepting **user and service** tokens,
rejecting bad/expired; route/spec path-coverage. `make test` = `go test -race -count=1 ./...`.
TDD for any bug fix (red → green).

---

## 13. Build order (milestones)

1. **Scaffold** — `go.mod`, `Makefile`, `testutil` (clock/ptrs), `config.Load` + structs, `config.example.yaml`.
2. **Influx query layer** — `queryClient` iface + real impl + `fake.go`; Flux builders; class→path routing. Tested (single-field-point schema, resets, gaps).
3. **Windowing** — tz-aware `[start,stop)`, days_in_window. Tested across BST/GMT/month edges.
4. **Tariffs + cost** — fetch/parse `energy_tariffs`, current-rate selection, kWh×rate×VAT, standing charge. Tested.
5. **HTTP server** — `server.go`, `/healthz`, `/openapi.json`, `spec_test.go`, auth (both token types). Tested.
6. **Handlers** — energy / cost / bill / tariffs / metrics + reconciliation. Tested.
7. **Remote Fetcher + main.go** — both namespaces, SIGHUP, graceful shutdown.
8. **Deploy** — `deploy/deploy.sh` + `countinghouse.service` (use `swee:deploy-service`). Deploy on request only.

---

## 14. Deploy-time prerequisites

- Mint countinghouse Influx **read token** scoped to `statehouse` bucket:
  `docker exec influxdb influx auth create --org swee.net --read-bucket <statehouse-bucket-id>` → `/etc/countinghouse/influx-token`.
- Register countinghouse `client_id`/`client_secret` in id.swee.net for outbound config fetches.
- Prod host `garibaldi`; deploy via `./deploy/deploy.sh sweeney@garibaldi` only when asked.
```

---

# Charting & Timeline Feature (Milestones 9–12)

Expose time-series + event data so consumers render charts (bar / stacked bar / line /
overlays) **without any knowledge of Influx or Flux**. Two tracks: quantitative energy
series (Track A) and categorical binary/event timelines (Track B). Both stay
query-on-demand / stateless. Decisions below were confirmed with the user 2026-06-11.

## A. Energy time-series

**Endpoints**
- `GET /series?window=&from=&to=&interval=&group_by=device|location|class|house` — multi-series.
- `GET /devices/{id}/series?window=&interval=` — single-device convenience.

**Response: columnar** (shared time axis + per-series value arrays — maps directly to chart libs):
```json
{ "window":"today","from":"...","to":"...","interval":"1h","group_by":"device",
  "buckets":["2026-06-11T00:00:00+01:00","2026-06-11T01:00:00+01:00","..."],
  "series":[ { "key":"winefridge","label":"Wine Fridge","location":"kitchen",
               "class":"continuous_power_device",
               "kwh":[0.05,0.04],"cost":[0.011,0.009],"avg_w":[52.1,41.8],
               "total_kwh":1.1,"total_cost":0.2413 } ] }
```

**Per-bucket metrics:** `kwh`, `cost` (kwh×tariff×VAT), `avg_w` (mean power). Aggregated
groups: kwh/cost **summed**, avg_w **summed** (power is additive).

**group_by:** device (default); location (per room); class; house → **two** series
`monitored` (sum of devices) + `meter` (whole-house `electricity_meter`) so consumers can
show total + the unmonitored gap.

**Interval:** smart default (today→1h, week→1d, month→1d, custom→by span), override via
`interval=` from {5m,15m,30m,1h,6h,1d}, hard cap ~1000 buckets → 400 asking for coarser.

**Tz/alignment:** buckets on Europe/London boundaries via Flux `aggregateWindow(location:…)`
(DST-aware). **Go owns the canonical bucket axis** (from window+interval) and maps Influx
results onto it, zero-filling gaps so every series shares identical `buckets[]` (required
for stacking). `createEmpty:true`.

**Influx (~3 queries, device-count-independent):**
1. Counter energy (plug+meter): `energy_kwh |> increase() |> aggregateWindow(every:dt,last,location) |> difference()`. Pad the range one interval earlier; drop the pad bucket so bucket 0 has a real delta. (`increase()` first = reset-safe.)
2. UPS energy: `power_w |> aggregateWindow(every:dt,mean,location)` × bucket-hours / 1000.
3. Avg power (all): `power_w |> aggregateWindow(every:dt,mean,location)`.
Cost derived in Go. Rounding via `roundTo` (kWh 3dp, cost 4dp, W 1dp).

## B. Binary / event timeline

**Source (verified live):** `device_activity` measurement — one point per state transition,
**string** fields `from`/`to` (e.g. `idle`→`active`). Intervals derived in Go by pairing
edges with a **carry-in** query (last transition before window start = opening state).
Stats derived from intervals. Binary devices have no energy (excluded from /bill already).

**Endpoints**
- `GET /devices/{id}/events?window=` — transition edges (vertical-line overlays).
- `GET /devices/{id}/intervals?window=` — on/off spans + durations + stats (shaded bands).
- `GET /events?devices=&class=&window=&group_by=device|class` — multi-device overlay.

**Events:**
```json
{ "device_id":"hot_water","window":"today",
  "events":[ {"time":"2026-06-11T05:30:01+01:00","from":"idle","to":"active","on":true},
             {"time":"2026-06-11T06:15:00+01:00","from":"active","to":"idle","on":false} ] }
```
**Intervals + stats** (`state_at_start` carry-in; `open:true` for an in-progress span at window end):
```json
{ "device_id":"hot_water","state_at_start":"idle",
  "intervals":[ {"start":"...05:30:01+01:00","end":"...06:15:00+01:00","on":true,"duration_s":2699},
                {"start":"...16:00:01+01:00","end":null,"on":true,"duration_s":1234,"open":true} ],
  "stats":{ "on_count":2,"total_on_seconds":3933,"duty":0.046 } }
```
`on` derived (`active`→true, `idle`→false); raw `from`/`to` always included so unfamiliar
vocabularies and future momentary events (doorbell ring, fire-alarm) still render as lines
(`on` omitted when not a known on/off label). Verify each new device's Influx shape when it
first appears.

**Key impl note:** `influx.Row.Value` is `float64`-only and currently DROPS string `_value`.
Track B requires extending `Row` to carry a string/text value (isolated change) + a
`BuildActivityFlux` builder. Multi-device = one query filtered to the device set/class.
Event/interval/stats logic lives in a new `internal/events` package.

## C. Device catalog (discovery)

`GET /devices` — pass-through of the `statehouse_devices` namespace so a UI can build its
device picker without knowing Influx. Per device: `id`, `display_name`, `location`, `class`,
and a derived `capabilities` hint (`energy` when PathForClass is metered, `events` for
binary/activity classes). Sourced entirely from the cached `ConfigProvider.Devices()`.

## Milestones

- **M9 — Energy series core (pure, tested):** `internal/influx` bucketed Flux builders
  (counter/UPS energy, avg power) + `FakeQuerier` bucketed-row support; `internal/energy/series.go`
  (axis generation, interval defaults/validation/cap, group_by aggregation, zero-fill, rounding).
  Tests: alignment, zero-fill, group sums, reset fixture, DST day, interval cap, house dual-series.
- **M10 — Energy series HTTP (tested):** `/series` + `/devices/{id}/series` handlers,
  `openapi.yaml` + path-coverage, wire in. Verify live.
- **M11 — Event core (pure, tested):** extend `influx.Row` for string values + `BuildActivityFlux`;
  new `internal/events` package (events, carry-in interval pairing, stats). Tests: edge pairing,
  carry-in opening state, open/in-progress span, duty/stats math, momentary/unknown-label events,
  multi-device grouping.
- **M12 — Event HTTP + catalog (tested):** `/events`, `/devices/{id}/events`,
  `/devices/{id}/intervals`, `/devices` catalog; `openapi.yaml` + path-coverage; wire in.
  Verify live against `hot_water`.

**Testing bar (all milestones):** mirror statehouse density. Fakes for every external system
(extend `FakeQuerier` to serve bucketed + string/activity rows; deterministic via `FakeClock`).
Table/fixture-driven. `make test` = `go test -race -count=1 ./...`. Path-coverage gate stays
green on every route addition (route + openapi.yaml + registeredPaths together).

---

# Future enhancements (backlog — not scheduled)

## Smooth fine-grained energy series (`source=counter|power` on `/series`)

Plug `energy_kwh` hardware counters tick in coarse **0.1 kWh** steps (discovered building
the demo, 2026-06-11). So `/series` per-bucket energy at fine intervals reads mostly 0 with
the occasional 0.1 — fine for day-scale buckets, but jagged/quantised for a low-draw device
at 15m (e.g. winefridge). The demo works around it by charting the `avg_w` power line plus a
**cumulative** kWh line.

Enhancement: add an optional `source` param to `/series`:
- `source=counter` (default) — per-bucket energy from the metered counter delta. Authoritative,
  matches the bill, but quantised at fine intervals.
- `source=power` — per-bucket energy derived from `integral(power_w)` per bucket (smooth at any
  interval). NOT authoritative: `avg_w`/∫power over-reads vs the counter (e.g. winefridge one
  day: counter 1.3 kWh vs ∫power 2.1 kWh) because devices report `power_w` more often while
  active, biasing the time-average high. Label it clearly as an estimate/shape, not billing.

The counter stays the source of truth for `/bill` and cost. This is purely a charting-quality
option. Builders/assembly already compute both signals (counter energy + mean power), so this
is mostly a routing + labelling change with tests for the divergence. See memory
`influx-device-energy-routing` for the counter-resolution / power-bias details.
