"""C1 — 'Has household load shifted in response to half-hourly prices?'

Replicates the workflow in issue #36 verbatim, then pushes on it: the same join
at 30m (which the reporter used) and at 15m (which they said fails silently).
"""
import ch, collections

ch.report("C1  load-vs-price, the reporter's own join")

W = dict(window="custom", **{"from": "2026-08-12T00:00:00+01:00", "to": "2026-09-01T00:00:00+01:00"})

for interval in ("30m", "15m"):
    series = ch.get("/series", group_by="house", shape="columns", interval=interval, **W)
    prices = None
    # /prices caps at 31 days; August is exactly 31.
    ok, prices = ch.try_get("/prices", **W)
    if not ok:
        print("  /prices refused:", prices.body.strip()[:160]); break

    P = {s['valid_from']: s['price'] for s in prices['slots']}
    aligned = [P.get(b) for b in series['buckets']]
    hits = sum(1 for p in aligned if p is not None)
    print(f"\ninterval={interval}: {len(series['buckets'])} buckets, "
          f"{len(prices['slots'])} price slots, {hits} joined "
          f"({100*hits/len(series['buckets']):.0f}%)")
    if hits < len(series['buckets']):
        miss = [b for b, p in zip(series['buckets'], aligned) if p is None][:3]
        print(f"  !! {len(series['buckets'])-hits} buckets got None. e.g. {miss}")
        print("     No error, no warning, no field on the response says so.")

    # what the reporter actually wanted: load by price decile
    meter = next(s for s in series['series'] if s['key'] == 'meter')
    pairs = [(p, k) for p, k in zip(aligned, meter['kwh']) if p is not None]
    if not pairs: continue
    pairs.sort()
    n = len(pairs)
    cheap = pairs[:n//10]; dear = pairs[-n//10:]
    print(f"  cheapest decile: mean {sum(k for _,k in cheap)/len(cheap):.3f} kWh/slot "
          f"@ {sum(p for p,_ in cheap)/len(cheap):.1f}p")
    print(f"  dearest  decile: mean {sum(k for _,k in dear)/len(dear):.3f} kWh/slot "
          f"@ {sum(p for p,_ in dear)/len(dear):.1f}p")

print("\n--- the zero-kWh blind spot the reporter describes ---")
series = ch.get("/series", group_by="house", shape="columns", interval="30m", **W)
meter = next(s for s in series['series'] if s['key'] == 'meter')
zero_cost = sum(1 for c in meter['cost'] if c == 0)
print(f"  buckets where cost==0: {zero_cost}  (price unrecoverable from cost in each)")
neg = [i for i, c in enumerate(meter['cost']) if c < 0]
print(f"  buckets with NEGATIVE cost (paid to consume): {len(neg)}")
if neg:
    i = neg[0]
    print(f"    e.g. {series['buckets'][i]}  {meter['kwh'][i]:.3f} kWh, cost {meter['cost'][i]}")
    print("    a consumer summing abs(cost) or clamping at 0 is wrong here")

print("\ncalls:", dict(ch.CALLS))
