# Countinghouse — Claude guidance

Countinghouse is a read-side energy **cost/accounting** service for the swee.net home.
It turns the per-device telemetry statehouse writes to InfluxDB into per-device kWh and
cost over arbitrary windows. **Full design brief: `AGENT_BRIEF.md` — read it first.**
Sibling/reference service: `../statehouse` (mirror its conventions).

## Core invariants (don't violate without discussion)

- **Read-side only.** No MQTT, no device ingest, no real-time state. Query Influx + apply tariffs.
- **Stateless w.r.t. accumulation.** Derive answers on query; never maintain running energy
  totals in memory/disk. The durable truth is the device-side counters in Influx, so the
  service must survive restart with zero data loss. Any cache must be rebuildable from Influx.
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
  Namespaces: `statehouse_devices` (exists), `energy_tariffs` (countinghouse defines it).
- Auth via `github.com/sweeney/identity/common`: JWKS verify inbound, `client_credentials`
  `TokenSource` outbound. **Accept service tokens** (`ParseServiceToken`) as well as user
  tokens — statehouse's gap of rejecting service tokens must not be inherited.
