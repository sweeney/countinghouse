# Generates the DST fall-back fixture for countinghouse (issue #49).
import json, sys
from datetime import datetime, timedelta, timezone
Z = timezone.utc
start = datetime(2026, 10, 24, 23, 0, tzinfo=Z)
stop = datetime(2026, 10, 25, 3, 0, tzinfo=Z)
W = 400.0
base = 1000.0
readings = []
t = start
while t <= stop:
    readings.append((t, round(base + W * (t - start).total_seconds() / 3600 / 1000, 6)))
    t += timedelta(minutes=1)
in_range = [r for r in readings if r[0] < stop]
first = in_range[0][1]
m = lambda h, mi: datetime(2026, 10, 25, h, mi, tzinfo=Z)
flux_windows = [
    (start, start + timedelta(minutes=30)),
    (start + timedelta(minutes=30), m(0, 0)),
    (m(0, 0), m(0, 30)),
    (m(0, 30), m(2, 0)),
    (m(2, 0), m(2, 30)),
    (m(2, 30), stop),
]
iso = lambda d: d.strftime("%Y-%m-%dT%H:%M:%SZ")
rows = []
for ws, we in flux_windows:
    inside = [r for r in in_range if ws <= r[0] < we]
    rows.append({"device_id": "steady_plug", "field": "energy_kwh",
                 "time": iso(ws), "value": round(inside[-1][1] - first, 6)})
json.dump({
    "description": "Steady 400 W counter device across the Europe/London autumn clock change "
                   "(2026-10-25 02:00 BST -> 01:00 GMT). Synthetic load; window "
                   "layout reproduces prod Influx's location-aware aggregateWindow.",
    "window": {"from": iso(start), "to": iso(stop)},
    "interval": "30m",
    "watts": W,
    "readings": [{"time": iso(a), "energy_kwh": v} for a, v in readings],
    "flux_rows": rows,
}, sys.stdout, indent=1)
