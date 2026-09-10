# Countinghouse — Claude guidance

Countinghouse is a read-side energy **cost/accounting** service for the swee.net home.
It turns the per-device telemetry statehouse writes to InfluxDB into per-device kWh and
cost over arbitrary windows. **Full design brief: `AGENT_BRIEF.md` — read it first.**
Sibling/reference service: `../statehouse` (mirror its conventions).

## Core invariants (don't violate without discussion)

- **Read-side with respect to the home.** No MQTT, no device ingest, no real-time state.
  Query Influx + apply tariffs. **One exception, and only one:** the external price archive —
  idempotent writes of immutable, externally-sourced facts (Octopus half-hourly spot prices)
  to a store that holds nothing else. Countinghouse still never ingests device telemetry.
  See `docs/octopus-price-data-model.md` §1–§1a for why this lives here rather than in a
  separate service.
- **Stateless w.r.t. accumulation.** Derive answers on query; never maintain running energy
  totals in memory/disk. The durable truth is the device-side counters in Influx, so the
  service must survive restart with zero data loss. Any cache must be rebuildable from Influx.
  **The price archive is not a cache** and this clause does not cover it: it is rebuildable
  from Octopus, not from Influx, and the reason we keep it is the day that stops being true.
  It is primary durable state, which is exactly why it gets an enforced key, a restatement
  log and a backup — not a retention policy.
- **Two query paths by device class:** plug classes → `increase(energy_kwh)` (reset-safe);
  `ups_sensor` → `integral(power_w)`. See AGENT_BRIEF §3.

## House rules

- **gofmt:** run `gofmt -w` on changed Go files before committing. CI enforces it.
- **OpenAPI:** when adding/removing an HTTP endpoint, update `internal/httpapi/openapi.yaml`.
  A path-coverage test (`internal/httpapi/spec_test.go`) fails CI if routes and spec drift.
- **Docs stay in sync:** any change to endpoints, request/response shapes, config, or behaviour
  must update BOTH `internal/httpapi/openapi.yaml` AND `README.md` in the same change. Treat
  out-of-date docs as a bug.
- **TDD for bug fixes:** write a failing test reproducing the bug, confirm red, then fix to green.
- **Tests matter here:** match statehouse's density. Use fake doubles (fake Influx query
  client) and an injected clock — never call `time.Now()` in logic. `make test` = `go test -race -count=1 ./...`.
- **Issues:** close via `Closes #N` in the commit message, not `gh issue close`.
- **Deploy:** only when the user asks. `./deploy/deploy.sh sweeney@garibaldi` (SSH+systemctl
  on garibaldi). Locally: build the binary; no tmux/systemctl.

## Config & auth (see AGENT_BRIEF §4, §6)

- Config is remote at `config.swee.net` (`GET /api/v1/config/{namespace}`), not local files.
  Namespaces: the site's devices namespace (named by `site.devices_namespace` in local
  config — `devices_home` here; the old shared `statehouse_devices` was deleted upstream
  and there is no default, so a config naming none refuses to start), `energy_tariffs`
  (countinghouse defines it), and the floorplan namespace (`site.floorplan_namespace` —
  `floorplan_home` here) shared with greenhouse, behind `/floors`, `/rooms` and grouped
  series labels. Both site namespaces are REQUIRED — a config naming either none refuses
  to start, since an unnamed floorplan degrades to ids-as-labels, which is silence that
  reads as data. **Boot needs truth, running keeps the last truth:** a namespace that has
  never been fetched aborts startup (`Fetcher.Cold()` → `requireWarmSnapshots`); every
  later failure, SIGHUP included, is fail-open and merely degrades `/healthz`.
- Auth via `github.com/sweeney/identity/common`: JWKS verify inbound, `client_credentials`
  `TokenSource` outbound. **Accept service tokens** (`ParseServiceToken`) as well as user
  tokens — statehouse's gap of rejecting service tokens must not be inherited.
