"""C6 — a budget tracker that polls: month-to-date vs last month, and is it stale?"""
import ch, json
ch.report("C6  polling budget tracker")

mtd = ch.get("/bill", window="month")
print("month-to-date:", {k: mtd[k] for k in ('window','energy_cost','standing_charge','total','attribution','effective_rate')})

print("\nIs the config I am being billed from current?")
t = ch.get("/tariffs")
print("  /tariffs keys:", list(t.keys()), "-> no timestamp of any kind")
hz = ch.get("/healthz")
print("  /healthz keys:", list(hz.keys()))
print("  remote_config block present:", 'remote_config' in hz)
print("  prices block present:", 'prices' in hz)
print("  -> on THIS deployment /healthz carries neither, so a consumer has no")
print("     staleness signal at all. /tariffs still answers 200 with rates.")

ch.report("C6b  VAT basis: can a consumer detect a rate in the wrong basis?")
elec = t['agreements']['electricity']
fixed = [a for a in elec if a['type']=='fixed'][0]
print(f"  fixed agreement: unit_rate={fixed['unit_rate']} vat_rate={fixed['vat_rate']}")
print(f"  ex-VAT? inc-VAT? The field name says ex. Nothing validates it.")
print(f"  if it were published inc-VAT by mistake: every bill is 5% high and still")
print(f"  internally consistent. /bill, /series, /prices all agree with each other.")
zero = [a for a in elec if a['vat_rate']==0]
print(f"  the zero-VAT block (1 Oct 26 - 31 Mar 27) IS in /tariffs: {bool(zero)}")
print(f"  README mentions the zero rate: see repo check; spec does.")

ch.report("C6c  caching for a poller")
import urllib.request
op = urllib.request.build_opener(urllib.request.ProxyHandler({}))
r = op.open("http://127.0.0.1:8787/prices?window=today")
etag = r.headers.get('ETag'); print("  /prices ETag:", etag, "Cache-Control:", r.headers.get('Cache-Control'))
for p in ("/bill?window=month", "/series?window=today&group_by=house", "/tariffs"):
    rr = op.open("http://127.0.0.1:8787"+p)
    print(f"  {p:42s} ETag={rr.headers.get('ETag')} Cache-Control={rr.headers.get('Cache-Control')}")
