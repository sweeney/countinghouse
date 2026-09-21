"""C2 — 'Is the half-hourly tariff actually beating the fixed alternative?'

Builds, by hand and from the API only, the four counterfactuals issue #36 lists.
Counts the calls, the lines, and the places a consumer can silently get it wrong.
"""
import ch

ch.report("C2  tariff counterfactuals, hand-rolled")
FROM, TO = "2026-08-12T00:00:00+01:00", "2026-09-01T00:00:00+01:00"
W = dict(window="custom", **{"from": FROM, "to": TO})
DAYS = 20

bill = ch.get("/bill", **W)
series = ch.get("/series", group_by="house", shape="columns", interval="30m", **W)
prices = ch.get("/prices", **W)
tariffs = ch.get("/tariffs")

meter = next(s for s in series['series'] if s['key'] == 'meter')
house_kwh = sum(meter['kwh'])
P = [ {s['valid_from']: s['price'] for s in prices['slots']}[b] for b in series['buckets'] ]

# ACTUAL. Note: /bill's own total is monitored-only, so the whole-house actual
# has to be rebuilt from /series. Friction point #5, felt.
actual_energy = sum(k*p for k, p in zip(meter['kwh'], P)) / 100
print(f"whole-house energy               {house_kwh:8.1f} kWh")
print(f"/bill energy_cost (monitored)    £{bill['energy_cost']:8.2f}   <- NOT the household's energy bill")
print(f"/bill total                      £{bill['total']:8.2f}   <- nor this")
print(f"whole-house actual (rebuilt)     £{actual_energy:8.2f}")
print(f"  ratio bill/actual: {bill['energy_cost']/actual_energy:.2f} — quoting /bill.total understates by "
      f"{100*(1-bill['total']/(actual_energy+bill['standing_charge'])):.0f}%")

sc = bill['standing_charge']
print(f"standing charge (from /bill)     £{sc:8.2f}")
actual_total = actual_energy + sc

# (a) same kWh, spread flat across the window's own prices
mean_price = sum(P)/len(P)
flat_same = house_kwh * mean_price / 100
print(f"\n(a) same kWh at the window's mean price   £{flat_same:7.2f}  "
      f"(shift benefit £{flat_same-actual_energy:+.2f})")
# NOTE: unweighted mean of slots is not the same as "spread flat" — you want the
# time-weighted mean, which here is the same only because slots are equal length.

# (b) a named fixed tariff, standing charge included
elec = tariffs['agreements']['electricity']
print("\n(b) /tariffs gives me these agreements:")
for a in elec:
    print(f"      {a.get('from','-')[:10]}..{a.get('to','open')[:10]}  {a['name']:20s} "
          f"{a['type']:8s} unit={a.get('unit_rate','-')} sc={a['daily_standing_charge']} vat={a['vat_rate']}")
fixed = [a for a in elec if a['type'] == 'fixed']
pick = fixed[0]
print(f"    picking the only fixed one: {pick['name']} — but it ENDED {pick['to'][:10]}.")
print("    Nothing in the response says it is no longer buyable. This is error #1 from the issue.")
cf_energy = house_kwh * pick['unit_rate'] * (1 + pick['vat_rate'])
cf_sc = DAYS * pick['daily_standing_charge'] * (1 + pick['vat_rate'])
print(f"    energy £{cf_energy:.2f} + standing £{cf_sc:.2f} = £{cf_energy+cf_sc:.2f}")
print(f"    vs actual £{actual_total:.2f}  → half-hourly {'wins' if actual_total < cf_energy+cf_sc else 'loses'} "
      f"by £{abs(actual_total-(cf_energy+cf_sc)):.2f} over {DAYS} days")
print(f"    if I had forgotten the standing-charge DIFFERENCE (error #2): I'd have compared")
print(f"      £{actual_energy:.2f} vs £{cf_energy:.2f} = £{cf_energy-actual_energy:+.2f}, "
      f"instead of £{(cf_energy+cf_sc)-actual_total:+.2f}. "
      f"Sign flips: {(cf_energy-actual_energy > 0) != ((cf_energy+cf_sc)-actual_total > 0)}")

# (c) the un-automated shape on this period's prices
pre = dict(window="custom", **{"from":"2026-06-10T00:00:00+01:00","to":"2026-06-30T00:00:00+01:00"})
old = ch.get("/series", group_by="house", shape="columns", interval="30m", **pre)
old_meter = next(s for s in old['series'] if s['key'] == 'meter')
# align by half-hour-of-week; 20 days is not a whole number of weeks, so this is
# already an approximation the consumer has to invent and document.
prof = {}
for b, k in zip(old['buckets'], old_meter['kwh']):
    hh = (int(b[11:13])*2 + int(b[14:16])//30)
    prof.setdefault(hh, []).append(k)
shape = {hh: sum(v)/len(v) for hh, v in prof.items()}
scale = house_kwh / (sum(shape[(int(b[11:13])*2+int(b[14:16])//30)] for b in series['buckets']) or 1)
cf_shape = sum(shape[(int(b[11:13])*2+int(b[14:16])//30)]*scale*p for b, p in zip(series['buckets'], P))/100
print(f"\n(c) old (pre-automation) shape on these prices  £{cf_shape:7.2f}  "
      f"(shift saved £{cf_shape-actual_energy:+.2f})")

# (d) a rate off a renewal letter
LETTER_RATE, LETTER_SC, LETTER_VAT = 0.2450, 0.6120, 0.05
d_energy = house_kwh * LETTER_RATE * (1+LETTER_VAT)
d_sc = DAYS * LETTER_SC * (1+LETTER_VAT)
print(f"(d) renewal quote 24.5p + 61.2p/day        £{d_energy+d_sc:7.2f}  "
      f"({'worse' if d_energy+d_sc > actual_total else 'better'} than actual by £{abs(d_energy+d_sc-actual_total):.2f})")

print(f"\ncalls: {dict(ch.CALLS)}  |  hand-written assumptions: mean-price basis, VAT basis,")
print("day count for the standing charge, week alignment for the shape, and which tariff is buyable.")
