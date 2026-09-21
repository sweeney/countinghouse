"""C3 — an appliance scheduler: 'when should I run the dishwasher, and what did waiting save?'"""
import ch
ch.report("C3  deferrable-load scheduler")

up = ch.get("/prices/upcoming", hours=24)
print("cheapest runs pre-computed by /prices/upcoming:")
for r in up.get('cheapest_runs', []):
    print(f"   {r}")
ch_ = ch.get("/prices/cheapest", duration="2h")
print("/prices/cheapest?duration=2h ->", {k: ch_[k] for k in list(ch_)[:8]})

print("\nTo cost that run I need the dishwasher's typical cycle energy. What is there?")
ok, iv = ch.try_get("/devices/dishwasher/intervals", window="7d")
print("  /devices/dishwasher/intervals:", "OK" if ok else iv.body[:100])
if ok:
    print("   keys:", list(iv.keys()))
    if iv.get('intervals'):
        print("   first interval:", iv['intervals'][0])
        print("   !! duty/duration only — no kWh per interval, so a scheduler cannot")
        print("      cost a cycle without re-deriving energy from /series by timestamp.")
    print("   stats:", {k: v for k, v in iv.items() if k not in ('intervals',)})

# So: derive it by hand.
s = ch.get("/devices/dishwasher/series", window="14d", interval="30m", shape="columns")
kwh = s['series'][0]['kwh']
runs, cur = [], 0.0
for k in kwh:
    if k > 0.05: cur += k
    elif cur: runs.append(cur); cur = 0.0
if cur: runs.append(cur)
print(f"\n  hand-derived from /series: {len(runs)} cycles in 14d, mean {sum(runs)/len(runs):.3f} kWh")
cost_at = lambda p: sum(runs)/len(runs) * p / 100
print(f"  a 2h run in the cheapest window (~{ch_.get('mean_price', ch_.get('price'))}p) = £{cost_at(ch_.get('mean_price') or 0):.3f}")
peak = max(x['price'] for x in up['slots'])
print(f"  the same run at today's peak ({peak}p) = £{cost_at(peak):.3f}")
print(f"  saving by deferring: £{cost_at(peak)-cost_at(ch_.get('mean_price') or 0):.3f}")
print("\n  Everything after the first two calls is client-side. The service holds the")
print("  cycle energy AND the price; the consumer joins them by timestamp.")

ch.report("C2b  when the standing charge flips the answer")
W = dict(window="custom", **{"from":"2026-08-24T00:00:00+01:00","to":"2026-08-31T00:00:00+01:00"})
s = ch.get("/series", group_by="house", shape="columns", interval="30m", **W)
p = ch.get("/prices", **W)
P = {x['valid_from']: x['price'] for x in p['slots']}
m = next(x for x in s['series'] if x['key']=='meter')
kwh = sum(m['kwh']); actual_energy = sum(k*P[b] for b,k in zip(s['buckets'], m['kwh']))/100
sc_actual = 7 * 0.591606 * 1.05
QUOTE_RATE, QUOTE_SC = 0.2150, 0.9200   # a low-unit, high-standing-charge fixed deal
q_energy = kwh*QUOTE_RATE*1.05; q_sc = 7*QUOTE_SC*1.05
print(f"  7 days, {kwh:.1f} kWh")
print(f"  energy only : actual £{actual_energy:.2f} vs quote £{q_energy:.2f}  -> quote looks {'BETTER' if q_energy<actual_energy else 'worse'}")
print(f"  with standing: actual £{actual_energy+sc_actual:.2f} vs quote £{q_energy+q_sc:.2f} -> quote is {'better' if q_energy+q_sc<actual_energy+sc_actual else 'WORSE'}")
print("  The sign flips. This is the reporter's error #2, and it flips whenever the")
print("  standing-charge gap is a bigger share of the total than the unit-rate gap —")
print("  i.e. on short windows and low-consumption homes, which is most of them.")
