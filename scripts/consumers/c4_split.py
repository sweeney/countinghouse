"""C4 — 'Split last month's actual bill between the rooms.'  A cost-allocation consumer."""
import ch
ch.report("C4  allocate the real bill across rooms")
W = dict(window="custom", **{"from":"2026-08-12T00:00:00+01:00","to":"2026-09-01T00:00:00+01:00"})

r = ch.get("/series", group_by="room", shape="columns", interval="1h", include_unmonitored="true", **W)
print("series returned:", [(s['key'], s['label'], round(s['total_cost'],2)) for s in r['series']])
tot = sum(s['total_cost'] for s in r['series'])
print(f"sum of room costs incl. catch-all: £{tot:.2f}")

h = ch.get("/series", group_by="house", shape="columns", interval="1h", **W)
meter = next(s for s in h['series'] if s['key']=='meter')
print(f"house meter cost:                 £{meter['total_cost']:.2f}   (match: {abs(tot-meter['total_cost'])<0.01})")

bill = ch.get("/bill", **W)
print(f"\nthe bill a landlord must actually divide = energy + standing:")
print(f"  whole-house energy £{meter['total_cost']:.2f} + standing £{bill['standing_charge']:.2f} = £{meter['total_cost']+bill['standing_charge']:.2f}")
print(f"  /bill.total says   £{bill['total']:.2f}  <- would short the landlord £{meter['total_cost']+bill['standing_charge']-bill['total']:.2f}")
print("\n  Allocating the standing charge is a policy choice (/bill deliberately refuses to).")
print("  But there is no endpoint that returns 'the number to divide' at all: it is")
print("  /series?group_by=house + /bill.standing_charge, added by the client.")

ch.report("C5  seasonality: how often is it cheap in winter vs summer?")
for label, frm, to in [
    ("2 years before the switch", "2024-03-01T00:00:00Z", "2024-03-20T00:00:00Z"),
    ("under the old FIXED deal",  "2025-12-01T00:00:00Z", "2025-12-20T00:00:00Z"),
    ("spanning the switch",       "2026-02-20T00:00:00Z", "2026-03-10T00:00:00Z"),
    ("on the half-hourly deal",   "2026-06-01T00:00:00Z", "2026-06-20T00:00:00Z"),
]:
    ok, resp = ch.try_get("/prices", window="custom", **{"from":frm,"to":to})
    if ok:
        print(f"  {label:28s} 200  half_hourly={resp['half_hourly']} slots={len(resp['slots'])}"
              + (f" flat_price={resp.get('flat_price')}" if not resp['half_hourly'] else ""))
    else:
        print(f"  {label:28s} {resp.status}  {resp.body.strip()[:110]}")
ok, st = ch.try_get("/prices/stats", window="custom", **{"from":"2025-12-01T00:00:00Z","to":"2025-12-20T00:00:00Z"})
print(f"  /prices/stats, same pre-switch window: {'200' if ok else st.status} "
      + ("" if ok else st.body.strip()[:110]))
print("\n  The archive holds a curve keyed by tariff_code. The CODE is in every price")
print("  response and in /tariffs. There is still no way to ask for it by code.")

ch.report("C5b  what /prices/stats can and cannot roll up")
st = ch.get("/prices/stats", window="custom", **{"from":"2026-06-01T00:00:00+01:00","to":"2026-09-01T00:00:00+01:00"})
print("top-level keys:", list(st.keys()))
print("one day:", st['days'][0])
print(f"days returned: {len(st['days'])} — a 'cheap slots per month' answer is {len(st['days'])} rows of client-side grouping")
