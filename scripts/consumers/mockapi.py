"""A mock of the PROPOSED countinghouse API, computed from the real one.

Nothing here invents numbers: every figure is derived from the live harness, so
a consumer rewritten against this proposal produces the same answers as the
hand-rolled version — which is the only way to tell whether the proposal is
actually sufficient.
"""
import ch, datetime as dt

SLOT_MIN = 30

def _slot_key(ts_iso):
    """The half-hour slot an RFC3339 bucket label falls inside."""
    t = dt.datetime.fromisoformat(ts_iso)
    return t.replace(minute=(t.minute // SLOT_MIN) * SLOT_MIN, second=0, microsecond=0)

def series_with_prices(**params):
    """PROPOSAL 1: GET /series?...&prices=true

    Adds `prices[]` aligned to `buckets[]`, `price_unit`, `price_basis` and
    `unpriced_buckets`. The server can always answer this: a bucket either sits
    inside one slot (interval <= 30m) or spans several (interval > 30m), and the
    honest number for the second case is the energy-weighted mean rate the
    costing already used.
    """
    s = ch.get("/series", **params)
    w = {k: params[k] for k in ("window", "from", "to") if k in params}
    p = ch.get("/prices", **w)
    if not p["half_hourly"]:
        s["prices"] = [p["flat_price"]] * len(s["buckets"])
        s["price_basis"] = "flat"
        s["price_unit"] = "p/kWh"
        s["unpriced_buckets"] = 0
        return s
    by_slot = {dt.datetime.fromisoformat(x["valid_from"]): x for x in p["slots"]}
    ivl = s["interval"]
    mins = {"5m": 5, "15m": 15, "30m": 30, "1h": 60, "6h": 360, "1d": 1440}[ivl]
    out, unpriced = [], 0
    for b in s["buckets"]:
        start = dt.datetime.fromisoformat(b)
        if mins <= SLOT_MIN:                      # bucket sits inside one slot
            sl = by_slot.get(_slot_key(b))
            out.append(sl["price"] if sl else None)
            s["price_basis"] = "slot"
        else:                                      # bucket spans n slots
            n = mins // SLOT_MIN
            ps = [by_slot.get(start + dt.timedelta(minutes=SLOT_MIN * i)) for i in range(n)]
            ps = [x["price"] for x in ps if x]
            out.append(round(sum(ps) / len(ps), 3) if ps else None)
            s["price_basis"] = "mean_over_bucket"
        if out[-1] is None:
            unpriced += 1
    s["prices"], s["price_unit"], s["unpriced_buckets"] = out, "p/kWh", unpriced
    s["tariff_code"] = p.get("tariff_code")
    return s

def compare(window_params, alternatives, scope="household"):
    """PROPOSAL 2: GET /compare?window=…&alt=…

    Returns the window's ACTUAL cost and each alternative's, both decomposed
    into energy + standing so a consumer cannot compare one and forget the
    other, plus whether the alternative is something that can actually be bought
    today.
    """
    s = ch.get("/series", group_by="house", shape="columns", interval="30m", **window_params)
    p = ch.get("/prices", **window_params)
    bill = ch.get("/bill", **window_params)
    by = {x["valid_from"]: x["price"] for x in p["slots"]}
    meter = next(x for x in s["series"] if x["key"] == "meter")
    monitored = next(x for x in s["series"] if x["key"] == "monitored")
    kwh = sum(meter["kwh"]) if scope == "household" else sum(monitored["kwh"])
    if scope == "household":
        energy = sum(k * by[b] for b, k in zip(s["buckets"], meter["kwh"])) / 100
    else:
        energy = bill["energy_cost"]
    days = (dt.datetime.fromisoformat(s["to"]) - dt.datetime.fromisoformat(s["from"])).total_seconds() / 86400
    standing = bill["standing_charge"]

    actual = {"label": "actual", "tariff_code": p.get("tariff_code"), "kwh": round(kwh, 3),
              "energy_cost": round(energy, 4), "standing_charge": round(standing, 4),
              "total": round(energy + standing, 4)}
    out = {"window": s["window"], "from": s["from"], "to": s["to"], "scope": scope,
           "currency": "GBP", "days": round(days, 4), "actual": actual, "alternatives": []}

    for a in alternatives:
        if a["kind"] == "flat":
            e = kwh * a["unit_rate"] * (1 + a["vat_rate"])
            sc = days * a["daily_standing_charge"] * (1 + a["vat_rate"])
        elif a["kind"] == "window_mean":
            mean = sum(by[b] for b in s["buckets"]) / len(s["buckets"])
            e, sc = kwh * mean / 100, standing
        else:
            raise ValueError(a["kind"])
        row = {"label": a["label"], "kind": a["kind"],
               "available_now": a.get("available_now", None),
               "kwh": round(kwh, 3),
               "energy_cost": round(e, 4), "standing_charge": round(sc, 4),
               "total": round(e + sc, 4),
               "delta": {"energy": round(actual["energy_cost"] - e, 4),
                         "standing": round(actual["standing_charge"] - sc, 4),
                         "total": round(actual["total"] - (e + sc), 4)},
               "verdict": "actual_cheaper" if actual["total"] < e + sc else "alternative_cheaper"}
        out["alternatives"].append(row)
    return out
