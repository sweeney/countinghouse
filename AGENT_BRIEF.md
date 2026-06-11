# Countinghouse — Agent Briefing

**Audience:** an agent tasked with designing and building `countinghouse`, a new Go
service in the swee.net ecosystem.
**Reference implementation:** `statehouse` at `/Users/sweeney/src/github.com/sweeney/statehouse`.
Treat statehouse as the canonical example of "how we build Go services here" — copy
its conventions unless this brief says otherwise.

**Your first deliverable is a design/implementation plan, not code.** Read this whole
brief, read the cited statehouse files, then produce a plan. Flag the open decisions
called out in §7.

---

## 1. Philosophy & objectives

Countinghouse turns the per-device telemetry that statehouse already writes to InfluxDB
into a **per-device breakdown of energy consumption and cost over arbitrary time
windows**. It answers questions like *"the wine fridge used 3 kWh today, 20 kWh this
week, 87 kWh this month, costing £X"* and decomposes a monthly electricity bill
device-by-device.

Core design tenets:

- **Read-side only.** Countinghouse does not ingest device readings, run MQTT, or hold
  real-time state. It is a query + accounting + pricing layer over data that already
  exists in Influx. Statehouse stays the lean real-time snapshot engine; we explicitly
  decided **not** to push energy-cost concerns into it.
- **Stateless with respect to accumulation.** It derives answers on query, not by
  maintaining running totals. The durable source of truth for energy is the device-side
  cumulative counters already in Influx — which survive restarts of *both* services. This
  is deliberate: an in-memory accumulator anywhere would reset on every redeploy, and we
  redeploy often. Countinghouse should be safe to restart at any moment with zero data loss.
- **Owns what statehouse deliberately doesn't:** tariffs, unit rates, standing charges,
  (later) time-of-use pricing, cost calculation, bill reconciliation against the meter
  total, and arbitrary-window reporting.
- **Same house metaphor / ecosystem conventions** as statehouse (`statehouse` →
  real-time state; `countinghouse` → energy + cost accounting). Same auth, same config
  service, same Go service shape, same test discipline.

### The two device groups (critical to the whole design)

Per-device windowed energy splits into two cases, because not every billable device has
its own hardware counter:

| Group | Count today | Has cumulative `energy_kwh`? | Window-kWh method |
|---|---|---|---|
| Plug-class devices (short_burst, cycle_power, continuous, media) | ~17 | **Yes** — device-side hardware counter | `increase(energy_kwh)` over the window (reset-safe) |
| **UPS** (`network-ups`, `office-ups`) | 2 | **No** — `power_w` only | `integral(power_w)` over the window |
| Meter (`electricity_meter`) | 1 | Yes (whole-house gross) | counter; use for bill reconciliation |

The UPSs are **legitimately billable** — statehouse counts their output load into
`monitored_w` on purpose (`internal/state/electricity.go:58 isPowerMonitored` includes
`ClassUPSSensor`). They are the *only* power-only class, and integration error is small
because their load is steady. Countinghouse handles both query paths; there is no
statehouse change required. If a *third* power-only device class ever appears, revisit
whether to add a tagged per-device integrator to statehouse — but not now.

---

## 2. Statehouse — what it is and its purpose

Statehouse is an event-driven smart-home state engine. Adapters (Zigbee2MQTT, Glow/SMETS2
meter, UPS, climate, boiler, intercom) receive MQTT messages and call
`engine.IngestReading()`. The engine maintains an in-memory snapshot of every device plus
a derived whole-house state (occupancy / activity / mode), and fans events out to sinks:
an MQTT publisher, an **InfluxDB writer**, and an HTTP API.

Key facts countinghouse depends on:

- It is **purely event-driven**, not polling. Writes to Influx happen per device reading.
- It already computes whole-house electricity (`gross`/`monitored`/`unmonitored` watts and
  kWh) and writes a synthetic `house_electricity` series — useful for reconciliation.
- It exposes a read API at `https://statehouse.swee.net` (e.g. `GET /state` returns the
  full device snapshot incl. `class`, `location`, `latest.power_w`, `latest.energy_kwh`).
  Countinghouse can call this for live device metadata, but for **historical** energy it
  should query Influx directly.
- Device classes: `short_burst_power_device`, `cycle_power_device`,
  `continuous_power_device`, `media_power_device`, `binary_state_device`,
  `environmental_sensor`, `ups_sensor`, `energy_meter`, `unclassified`.

Reference files: `internal/state/engine.go`, `internal/state/electricity.go`,
`internal/state/engine_electricity.go`, `internal/influx/writer.go`.

---

## 3. Data available in InfluxDB

Statehouse writes points using the **event timestamp** (not ingest time). Writes are async.
Influx client lib: `github.com/influxdata/influxdb-client-go/v2 v2.14.0` (countinghouse
uses the **query** side of the same lib).

**Connection (same instance statehouse writes to):** configured via `influx.url`,
`influx.org`, `influx.bucket`, `influx.token`. In prod: org `swee.net`, bucket
`statehouse_test`. ⚠️ The bucket was flagged for migration to a permanent name — **confirm
the live bucket/org with the user before wiring queries.**

### Measurements relevant to countinghouse

**`device_power`** — the core series. Tags: `device_id`, `class`, `location`. Fields:
- `power_w` float (instantaneous)
- `voltage_v` float
- `energy_kwh` float — **cumulative hardware counter**, present only for devices that
  report one (all plug-class devices; absent on the two UPSs)

**`house_electricity`** — synthetic whole-house aggregate. Tag: `scope="whole_house"`.
Fields incl. `gross_w`, `monitored_w`, `unmonitored_w`, `coverage`,
`today_kwh`/`week_kwh`/`month_kwh` (meter's own authoritative period totals),
`session_*_kwh` (statehouse-integrated since boot — **resets on restart, do not rely on
for history**). Use `today/week/month_kwh` and the meter counter for bill reconciliation.

Other measurements exist (`device_environment`, `device_battery`, `device_ups`,
`device_radio`, `appliance_cycle`, `device_activity`, `house_state`) but are not needed
for cost accounting. `appliance_cycle` carries per-cycle `selected_energy_kwh` /
`integrated_kwh` and may be useful later for per-cycle cost attribution.

### Query approach (the heart of countinghouse)

Counter devices — reset-safe windowed kWh:
```flux
from(bucket: "statehouse_test")
  |> range(start: windowStart, stop: windowStop)
  |> filter(fn: (r) => r._measurement == "device_power" and r._field == "energy_kwh")
  |> filter(fn: (r) => r.device_id == "winefridge")
  |> increase()          // handles device-side counter resets
  |> last()
```

UPS / power-only — integrate watts to kWh:
```flux
from(bucket: "statehouse_test")
  |> range(start: windowStart, stop: windowStop)
  |> filter(fn: (r) => r._measurement == "device_power" and r._field == "power_w")
  |> filter(fn: (r) => r.device_id == "network-ups")
  |> integral(unit: 1h, interpolate: "linear")   // W·h
  |> map(fn: (r) => ({ r with _value: r._value / 1000.0 }))  // → kWh
```

Cost = windowed kWh × tariff unit rate, plus apportioned standing charge. Reconcile the
sum of per-device kWh against `house_electricity` period totals / meter counter to surface
coverage and unmonitored remainder.

**Watch-outs:** counter resets (handled by `increase()`); device offline gaps (UPS
integration tolerates them; counters are immune); `coverage` can exceed 1 or go negative
(solar/battery export on SMETS2) — don't assume 0–1; stale devices.

---

## 4. Config: `statehouse_devices` and `energy_tariffs`

Config lives in a **remote config service at `https://config.swee.net`**, not local files.
Statehouse fetches namespaces over HTTP and merges them over local defaults.

**Endpoint pattern:** `GET {base_url}/api/v1/config/{namespace}`
e.g. `https://config.swee.net/api/v1/config/statehouse_devices`.
**Auth:** Bearer token via OAuth2 `client_credentials` (see §6). On 401, invalidate the
token and retry. Fetches are **fail-open**: on error, log a warning and keep last-known/
local values; record per-namespace status and expose it on `/healthz`.

Reference: `internal/config/remote.go` (`Fetcher.fetch`, `applyDevices`),
`internal/identity/tokensource.go`, `cmd/statehouse/main.go:52-66` (load) and SIGHUP
reload at `:239-252`.

### `statehouse_devices` (exists today)

Shape: JSON object keyed by device id → device config. Countinghouse needs this for the
device inventory and especially **`class` and `location`** (to group cost by room/class)
and `display_name`. The Go struct statehouse deserializes into
(`internal/config/config.go:247`):

```go
type DeviceConfig struct {
    Scheme  string // adapter: "zigbee","tasmota","shelly",...
    Primary string // adapter's stable id
    Display string
    // legacy z2m shorthand, normalised on load:
    IEEEAddress  string // -> scheme=zigbee, primary=ieee
    FriendlyName string // -> display
    Class       string // device class (drives which query path)
    DisplayName string
    Location    string
    Thresholds  *Thresholds
    EnergyStrategy string // "counter" | "integration" override
}
```

Countinghouse can derive each device's query path from `Class`: plug classes → counter
query; `ups_sensor` → integral query. (`EnergyStrategy` is statehouse's per-cycle hint;
countinghouse can ignore it or use it as a fallback signal.)

### `energy_tariffs` (does NOT exist yet — countinghouse defines it)

There is **no** `energy_tariffs` namespace in statehouse or, as far as we know, in the
config service. Countinghouse owns this. Plan to:

1. Define the schema and create the `energy_tariffs` namespace in `config.swee.net` (the
   building agent should confirm with the user how config namespaces get provisioned).
2. Read it via the **same fetcher pattern** as statehouse (`/api/v1/config/energy_tariffs`,
   Bearer client_credentials, fail-open, SIGHUP reload).

Proposed shape (for the plan to refine with the user — UK single-supply home):
```yaml
# energy_tariffs
currency: GBP
standing_charge_pence_per_day: 60.0
unit_rates:
  - name: standard
    pence_per_kwh: 24.5
    # optional time-of-use windows for later:
    # applies: { days: [...], from: "00:00", to: "07:00" }
effective_from: "2026-04-01"
# history of past tariffs so historical windows price correctly:
previous:
  - { pence_per_kwh: 27.0, standing_charge_pence_per_day: 53.0, effective_from: "2025-10-01", effective_to: "2026-03-31" }
```
Pricing a historical window must use the tariff(s) **in effect during that window**, so the
schema must carry effective dates / history, not just the current rate. This is a key
design point for the plan.

---

## 5. Architecture & Go conventions

Mirror statehouse. Concrete conventions:

**Module & shared deps** (`go.mod`): module `github.com/sweeney/countinghouse`, Go `1.25.x`.
Reuse the shared sweeney module:
- `github.com/sweeney/identity/common` — provides `auth.JWKSVerifier` (inbound token
  verification) and `spec.Converter` (serves `/openapi.json` from embedded YAML). **Use it
  for both auth and the OpenAPI endpoint.**
Plus `github.com/influxdata/influxdb-client-go/v2` (query side) and `gopkg.in/yaml.v3`.
No MQTT/Influx-write deps needed.

**HTTP layer** (`internal/httpapi/`): plain `net/http.ServeMux` (no chi/gorilla). Handlers
are methods on a `Server` struct; route table built in a `newMux(s)` function. Shared
`writeJSON` helper. Reference `internal/httpapi/server.go:101`.

**Health:** public `GET /healthz` returning a JSON struct: status, version, started_at/ago,
goroutines, **downstream reachability** (Influx reachable, config-service fetch status per
namespace). Model on `server.go:169`.

**OpenAPI:** hand-maintained `internal/httpapi/openapi.yaml`, `//go:embed`-ed, served at
`/openapi.json` via the shared `spec.Converter` with a `__PUBLIC_URL__` placeholder.
**Enforce route/spec parity with a path-coverage test** (`spec_test.go`) that diffs
`newMux` routes against spec paths and fails CI on drift. This is a hard project rule (it's
in statehouse's `CLAUDE.md`); replicate it. `/healthz` and `/openapi.json` are `security: []`.

**main.go startup sequence:** flag `-config` (YAML path) → `slog` JSON logger to stderr →
`config.Load` → build remote `config.Fetcher` (identity `TokenSource`) and apply → build
Influx **query** client → build HTTP `Server` → start with graceful shutdown on context
cancel (5s timeout) → SIGHUP handler re-fetches remote config. Reference
`cmd/statehouse/main.go`.

**Config:** local YAML at `/etc/countinghouse/config.yaml` for bootstrap (http listen,
influx url/org/bucket/token, identity base_url/client_id/client_secret, remote_config
base_url), overlaid by remote namespaces. Reference `internal/config/config.go`.

**Testing (take this seriously — it's a core value here):** statehouse has ~66 `_test.go`
files / ~10k lines. Patterns: `setup(t)` helpers; fake doubles for external systems
(`influx/fake.go`, `mqtt/fake.go`) — **build a fake Influx query client** so handlers are
tested without a live DB; `testutil.FakeClock` for deterministic time (essential for a
windowing/tariff service — never call `time.Now()` directly in logic, inject a clock);
JSONL fixtures for replay. `make test` runs `go test -race -count=1 ./...`. Aim for the
same density: table/fixture-driven tests over query windowing, counter-reset handling,
UPS integration, tariff selection by effective date, and bill reconciliation math.

**Logging/observability:** `log/slog` JSON to stderr, structured key-values, `SetDefault`.
A `/metrics` JSON endpoint (atomic counters: query count/errors, influx latency, cache
stats) like statehouse's — optional but cheap.

**Deployment:** `deploy/deploy.sh` cross-compiles `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`
with `-ldflags "-X main.version=$COMMIT"`, scp's a timestamp-versioned binary to the host,
symlinks active, `systemctl restart`, verifies `is-active` + clean journal, prunes old
versions. Plus a hardened `countinghouse.service` systemd unit (non-root user,
ProtectSystem, Restart=always). Model on `deploy/`. There is a `swee:deploy-service` skill
for generating this. Prod host is `garibaldi` (deploy via `./deploy/deploy.sh sweeney@garibaldi`
only when the user asks to deploy).

---

## 6. swee.net authentication regime

Identity provider: `https://id.swee.net`. Two directions:

**Inbound (validating callers of countinghouse):** use `auth.JWKSVerifier` from
`github.com/sweeney/identity/common/auth`. It fetches/caches JWKS from the issuer's
`/.well-known/jwks.json` and verifies JWT signatures locally (no per-request
introspection). Middleware pattern (`internal/httpapi/auth.go:10`):

```go
token := strings.TrimPrefix(authHeader, "Bearer ")
claims, err := verifier.Parse(ctx, token)   // user tokens
if err != nil || !claims.IsActive { 401 }
```

Build the verifier from an `IdentityURL` config value; when it's empty, disable auth
(local dev/tests). Protect all data routes; leave `/healthz` and `/openapi.json` public.
Reference `internal/httpapi/server.go:124` (`authMiddleware`).

**⚠️ Known gap to fix from the start:** statehouse's `requireAuth` calls **only**
`verifier.Parse()` (user tokens) and never `verifier.ParseServiceToken()`, so
**service-to-service callers using `client_credentials` tokens get 401**. Countinghouse is
very likely to be called by other services, so implement the middleware to try **both**:
`Parse()` first, fall back to `ParseServiceToken()`, reject only if both fail. (Confirm the
exact `ParseServiceToken` signature in the installed `identity/common` version.)

**Outbound (countinghouse calling config.swee.net, or statehouse):** use the
`client_credentials` `TokenSource` pattern (`internal/identity/tokensource.go`): POST
`grant_type=client_credentials&client_id&client_secret` to `{id_base}/oauth/token`,
cache the token, refresh ~30s before expiry, `Invalidate()` and retry on a downstream 401.
Countinghouse needs its own `client_id`/`client_secret` registered in id.swee.net (action:
confirm/provision with the user).

For local manual testing against `*.swee.net`, the user has a device-code auth helper
(`swee:sweenet-auth` skill) that yields a bearer token.

---

## 7. Open decisions to surface in the plan

1. **Influx bucket/org** — confirm the live, post-migration bucket name (memory says
   `statehouse_test`, flagged for migration). Don't hardcode blind.
2. **`energy_tariffs` schema + provisioning** — finalize the shape (esp. effective-date
   history for pricing historical windows) and how the namespace gets created in
   config.swee.net.
3. **Standing charge apportionment** — how to attribute the daily standing charge across
   devices (evenly? only in the cost total, not per-device?).
4. **Reconciliation policy** — how to present the gap between summed per-device kWh and the
   meter total (the "unmonitored" remainder) in bill breakdowns.
5. **Query-time vs. precompute** — start fully query-on-demand (simplest, restart-safe).
   Only consider a cache/rollup if Influx query latency over long windows proves too slow;
   if so, the cache must be rebuildable from Influx so the service stays stateless-by-design.
6. **Service `client_id`/`client_secret`** for countinghouse in id.swee.net.
7. **Time zone** for "today/this week/this month" window boundaries (home is UK; mind BST).

---

## 8. Suggested initial API surface (for the plan to refine)

- `GET /healthz`, `GET /openapi.json` — public
- `GET /devices/{id}/energy?window=today|week|month|custom&from=&to=` → `{ kwh, source }`
- `GET /devices/{id}/cost?window=...` → `{ kwh, cost, currency, tariff }`
- `GET /bill?window=month` → per-device breakdown + totals + reconciliation vs meter
- `GET /tariffs` → current/effective tariff(s)

Keep responses small, typed, and documented in `openapi.yaml` (path-coverage test enforced).
