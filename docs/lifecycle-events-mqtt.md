# Pipeline lifecycle events over MQTT — parked

Status: **parked, not built.** Recorded 2026-09-11 so the ask and the reasoning
survive. Nothing in the codebase depends on this document; it is here to be picked
up later without re-deriving the thinking.

---

## The original ask

Verbatim, so it is not lost to paraphrase:

> Can we emit mqtt events from the pipeline of price fetches? fetch attempted,
> nothing new available, NEW AVAILABLE, fetched successfully, fetch rejected +
> reasons etc.

So: a stream of **lifecycle telemetry** from the collector, on the ecosystem's
existing bus, so other services and dashboards can react to what the price
pipeline is doing.

---

## Current view

**Worth doing, and cheap — but it is NOT the alerting path, and must not reuse it.**

### Why it is a separate concern from `internal/notify`

This is the substantive point, and it is the reason not to take the apparent
shortcut of adding MQTT as a second `notify.Notifier`:

| | `notify` | lifecycle events |
|---|---|---|
| Purpose | wake a human | let a machine react |
| Volume | a handful a month | several per sync, all day |
| Throttling | **essential** | **wrong** |
| Missing one | acceptable | defeats the point |

`notify.Throttle` deliberately suppresses repeats of an ongoing condition, because
the alternative is dozens of identical messages an evening and a muted channel. A
telemetry consumer wants the opposite: **every** event, including the boring
`nothing new` ones, because "the collector checked and there was nothing" is itself
the signal that it is alive.

Route lifecycle events through the notifier and the throttle eats most of them.
Remove the throttle to suit them and the alerting becomes unusable. So: two sinks,
one purpose each.

```
                       ┌──────────────────────────────┐
                       │          collector           │
                       └───────┬──────────────┬───────┘
                               │              │
             a human must know │              │ something may want to react
                               ▼              ▼
                    ┌──────────────┐   ┌──────────────────┐
                    │   Notifier   │   │    EventSink     │
                    │  throttled   │   │  every event     │
                    │  slog (+MQTT)│   │  MQTT            │
                    └──────────────┘   └──────────────────┘
```

### The events, and where each already exists

Nothing new has to be computed. `SyncResult` and `Status` already carry every one
of these — this is an emit seam plus a publisher, not new logic.

| Topic (under `house/countinghouse/prices/`) | Payload | Already available as |
|---|---|---|
| `fetch_attempted` | tariff, probe result | start of `Sync` |
| `up_to_date` | `known_to` | `SyncResult.UpToDate` |
| `published` | from, to, slot count | the fetched range |
| `stored` | inserted, unchanged, restated | `SyncResult.Stored` |
| `rejected` | count, reasons histogram | `SyncResult.Rejected` + `rejectionReasons()` |
| `day_complete` | day, slot count | `SyncResult.TomorrowComplete` |
| `day_incomplete` | day, present/expected, tail-only | `prices.DayCompleteness` |
| `fetch_failed` | status, retryable, error | the returned error + `octopus.IsRetryable` |

`house/` matches statehouse's publish prefix, so the namespace is already the
ecosystem's.

### What has to be decided first

1. **CLAUDE.md says "no MQTT".** This needs a second, narrower invariant
   amendment than the price-archive one: *outbound publish only — no subscribe, no
   ingest, no device state*. Worth writing explicitly rather than letting an MQTT
   import arrive quietly, since "no MQTT" was a deliberate boundary against
   countinghouse drifting into real-time state.
2. **A dependency and broker config.** statehouse uses paho; this would too, with
   broker URL and credentials in local config.
3. **Retained or not?** Probably not for lifecycle events — they are a stream, not
   a state. `known_to` / `complete_to` would be better as a single retained
   `status` topic, so a consumer connecting mid-day learns where things stand
   without waiting for the next sync.
4. **Fail-open, obviously.** A broker being down must never stop the archive
   filling. Same rule as the notifier: count the failure, carry on.

### Sequencing

Not a blocker for anything. The collector works without it, and the events it would
publish are already visible in the logs and on `Status`. Natural time to build it
is alongside wiring the collector into `main.go`, since both need the same config
plumbing — or after M7, when the `/prices` endpoints give dashboards a pull
interface and it becomes clearer whether a push stream is still wanted.

### The cheaper alternative, if it ever looks like too much

A single retained `house/countinghouse/prices/status` topic republished after each
sync, carrying `known_to`, `complete_to`, last error and the counters. That is one
topic, no event taxonomy to maintain, and answers "is the pipeline healthy and how
far do prices run?" — which is most of the value. The per-event stream is what you
want for *reacting*; the status topic is what you want for *watching*.
