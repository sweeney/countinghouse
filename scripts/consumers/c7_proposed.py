"""The same consumer questions, rewritten against the PROPOSED API."""
import ch, mockapi
ch.report("P1  /series?prices=true — the join, server-side")
for ivl in ("30m", "15m", "1h"):
    W = dict(window="custom", **{"from":"2026-08-24T00:00:00+01:00","to":"2026-08-31T00:00:00+01:00"})
    s = mockapi.series_with_prices(group_by="house", shape="columns", interval=ivl, **W)
    meter = next(x for x in s['series'] if x['key']=='meter')
    pairs = [(p,k) for p,k in zip(s['prices'], meter['kwh']) if p is not None]
    covered = sum(k for _,k in pairs) / sum(meter['kwh'])
    print(f"  interval={ivl:4s} basis={s['price_basis']:17s} unpriced_buckets={s['unpriced_buckets']} "
          f"energy covered {100*covered:.0f}%")
print("  Consumer code for the whole analysis:")
print("     pairs = list(zip(s['prices'], meter['kwh']))            # 1 line, 1 call")
print("  vs the current two calls + dict build + key-match + silent 50% loss at 15m.")

ch.report("P2  /compare — the counterfactual, server-side")
W = dict(window="custom", **{"from":"2026-08-12T00:00:00+01:00","to":"2026-09-01T00:00:00+01:00"})
r = mockapi.compare(W, [
    {"kind":"flat","label":"Flexible Octopus (expired 2026-03-01)","unit_rate":0.2089,
     "daily_standing_charge":0.5294,"vat_rate":0.05,"available_now":False},
    {"kind":"flat","label":"renewal quote 21.5p + 92p/day","unit_rate":0.2150,
     "daily_standing_charge":0.9200,"vat_rate":0.05,"available_now":True},
    {"kind":"window_mean","label":"same kWh, flat across this window's own prices"},
])
import json; print(json.dumps(r, indent=2)[:1800])
print("\n  Each row carries delta.energy AND delta.standing separately, so the two")
print("  errors in the issue are not expressible: you cannot omit the standing")
print("  difference, and available_now:false labels the expired baseline.")
