# Per-device attribution under counter quantisation

**Status: decided — C1 (slot-price the counter deltas).** Recorded 2026-09-11 with the
options and the measurements, so C2 or C3 can be picked up later without re-deriving any
of it.

This is the decision that was open as "D1" and blocked slot-resolution costing (M6).

---

## 1. The problem

Whole-house cost under a half-hourly tariff is exact: the meter is fine-grained and is the
billing reference. **Per-device** cost is not, because plug-class devices report a
cumulative hardware counter that ticks in **0.1 kWh steps**.

Measured, `winefridge`, 2026-09-09, 30-minute buckets:

```
  reality:  ~67 W continuous, all day          what the counter reports:
  ████████████████████████████████████        ▁▁█▁▁█▁█▁▁█▁▁█▁█▁▁█▁▁█▁█▁▁█▁▁
  1.6 kWh spread evenly over 48 slots          32 slots of 0.0, 16 slots of 0.1
```

48 slots, **two distinct values**: `0` or `0.1`. The fridge draws continuously, but its day
is recorded as 16 discrete spikes landing wherever the counter happened to roll over.
`bigfridge` is identical. `kitchenkettle` manages three values (0, 0.1, 0.2). Only
`dishwasher` is finer-grained, because it draws hard enough to tick several times per slot.

The two UPSs are the opposite: `0.039` in *every* slot, perfectly smooth, because they have
no counter at all and go through the `integral(power_w)` path. That turns out to be a useful
control — see §3.

Under a flat tariff none of this mattered. Under a half-hourly tariff each spike is priced at
*some* slot's rate, and on 2026-09-09 those ranged **22.64p to 57.77p** inc VAT.

---

## 2. The options

### C1 — price each counter delta at its own slot's rate

```
cost = Σ (counter delta in slot i) × (price of slot i)
```

- **Sums exactly** to the monitored house cost: the bill adds up by construction.
- One query path, already built (`BuildCounterSeriesFlux`).
- Preserves *when* a device ran, which is the entire point of a half-hourly tariff.
- Noisy for a low-draw device on any single day.

### C2 — shape from power, magnitude from the counter

```
shape_i  = ∫power over slot i                      ← when it actually drew
scaled_i = shape_i × (counter_total / Σ shape)      ← trust the counter for how much
cost     = Σ scaled_i × price_i
```

- In principle the most accurate: fixes timing without inheriting raw ∫power's magnitude
  error, because rescaling discards the level and keeps only the shape.
- Two query paths per device, and a concept to document on the response.
- **The risk, measured.** ∫power and the counter disagree by device-specific amounts over
  one day: `winefridge` 1.54×, `bigfridge` 1.21×, `dishwasher` 0.90×, `tumbledryer` 1.43×.
  Rescaling is designed to absorb exactly that — but a bias varying 0.90–1.54 between
  devices suggests sampling irregularity, and if a plug reports power at cycle peaks more
  often than troughs then the *shape* is distorted too, which rescaling cannot fix. Power
  does report in all 48 slots for every device, so it is not simply gated on activity.
- **Worth re-measuring before building.** PR #33 changed the UPS path from mean×hours to a
  trapezoidal integral, precisely because a bucket mean weights every sample equally
  regardless of how long it stood. Some of that 1.54× is likely the same artefact, so C2 may
  be more viable than these figures suggest — but that should be measured against the
  integral path, not assumed.

### C3 — give every device the window's average price

```
cost = device kWh × window mean price
```

- Trivial, perfectly stable, zero noise.
- **Deletes the answer.** Every device reports the same effective rate, so nothing can tell
  you the dryer ran during the peak — which is the one thing a half-hourly tariff is for.

---

## 3. The measurements

### One day (2026-09-09, mean price 34.40p inc VAT)

| device | kWh | ticks | C1 effective | vs mean | reading |
|---|---|---|---|---|---|
| winefridge | 1.60 | 16 | 35.49p | +3.2% | noise |
| bigfridge | 2.10 | 21 | 35.18p | +2.3% | noise |
| dishwasher | 3.69 | 11 | 33.11p | −3.7% | signal |
| tumbledryer | 2.50 | 11 | 37.20p | +8.2% | signal |

Theoretical worst case, if every `winefridge` tick clustered in expensive slots: £0.73
against £0.42, a ±27% swing. That is a *bound*, not a behaviour — compressor cycling is
uncorrelated with price — but it is why a single day's fridge figure should not be read
closely.

### One month (August 2026, 1488 slots, mean 26.64p inc VAT, range −4.20p to 72.68p)

This is the test that mattered, because C1 rests on the claim that quantisation error is
unbiased and therefore cancels over many days.

| device | class | kWh | ticks | C1 effective | deviation |
|---|---|---|---|---|---|
| winefridge | continuous | 41.6 | 416 | 26.99p | **+1.3%** |
| bigfridge | continuous | 53.0 | 530 | 27.22p | **+2.2%** |
| bigfreezer | continuous | 40.1 | 401 | 26.94p | **+1.1%** |
| chestfreezer | continuous | 11.2 | 112 | 26.56p | **−0.3%** |
| dehumidifier | continuous | 31.2 | 299 | 27.62p | **+3.7%** |
| kitchenkettle | burst | 14.7 | 136 | 27.10p | +1.7% |
| dishwasher | **deferrable** | 47.0 | 147 | 25.20p | **−5.4%** |
| washingmachine | **deferrable** | 11.1 | 57 | 22.06p | **−17.2%** |
| tumbledryer | **deferrable** | 9.2 | 46 | 24.90p | **−6.5%** |
| network-ups | *integral, no counter* | 57.6 | 1488 | 26.65p | **+0.1%** |

```
  continuous   mean +1.61%   (−0.3 .. +3.7)
  deferrable   mean −9.69%   (−17.2 .. −5.4)
                              └─ roughly 6:1 signal to noise
```

**The UPS is the control, and it is the most reassuring number here.** It has no counter and
no quantisation, and it comes out at **+0.1%** — essentially exactly the mean. So the method
itself introduces no bias: every deviation in the other rows is either quantisation error or
a real timing effect, and not an artefact of slot-pricing.

**One honest nuance.** The continuous devices' +1.61% is systematically positive, which pure
quantisation noise would not be — a genuinely constant load consuming equally in every slot
would land exactly on the unweighted mean. The likeliest explanation is that it is partly
*real*: fridges, freezers and a dehumidifier work harder when the house is warm and occupied,
and those hours correlate with higher prices. If so, C3 would erase even that. It is small
either way, and the UPS control shows it is not coming from the machinery.

**Also worth noting for its own sake:** the deferrable loads already run 5–17% cheaper than
price-blind, without anyone trying.

---

## 4. Decision, and why

**C1.** The deciding observation is the alignment between where C1 is noisy and where it
matters:

```
                  monthly deviation   noise or signal?   can you shift it?
  fridges/freezers       +1 to +2%     mostly noise            no
  dehumidifier                 +3.7%   mixed                   partly
  deferrable loads       −5 to −17%    SIGNAL                  YES
```

C1's noise lands on the devices you cannot shift anyway, and it preserves the signal on
exactly the 3.22 kWh/day of deferrable load you can. C3 is clean precisely where cleanliness
is worthless, and blind precisely where it is not.

Supporting reasons: per-device costs sum to the monitored house cost by construction; it uses
one query path that already exists; and over a month the residual for a fridge is ~1–2%,
which is smaller than the uncertainty in the counter reading itself.

### What ships with it

- The response names its method — `"attribution": "counter_slot"` — so a consumer is never
  guessing how a number was produced.
- Each device carries an `effective_rate`, which makes a noisy fridge figure interpretable
  rather than merely odd, and incidentally gives C3's number for free without C3's blindness.
- A slot with no known price never contributes 0. It surfaces as explicit unpriced energy.

---

## 4a. The continuous devices' positive offset is real consumption, not lag

*Measured 2026-09-13, four weeks of real half-hourly energy (2026-08-16 → 09-13) against
two years of archived prices. This settles a question §2 and §4 could only speculate about.*

A reviewer proposed a second explanation for the continuous devices' systematic ~+1.6%: not
a warm house, but **phase bias**. A 0.1 kWh step on a 67 W load takes ~90 minutes to
accumulate and is only reported once the energy has been consumed, so each tick lands
systematically late — roughly half a tick interval. Prices are autocorrelated over that
scale (the evening ramp is near-monotonic), so a one-sided lag would attribute consumption
to later, dearer slots. Unlike quantisation noise, that does **not** cancel with more
months. It was a good hypothesis and it is wrong.

**Test 1 — shift the prices.** If ticks are reported late by L, pricing each bucket's
energy at the slot L earlier should pull the deviation toward zero, and the minimising L
should scale with each device's tick interval.

| device | half tick interval | L minimising \|dev\| | dev @ L=0 | dev @ best |
|---|---|---|---|---|
| network-ups *(control, no counter)* | 15 min | — | −0.05% | −0.05% |
| bigfridge | 39 min | 150 min | +1.33% | +0.08% |
| winefridge | 47 min | 180 min | +1.02% | +0.73% |
| bigfreezer | 51 min | 180 min | +1.20% | −0.08% |
| dehumidifier | 66 min | 30 min | −0.07% | +0.06% |
| basement-fridge | 133 min | 0 min | −0.42% | −0.42% |
| chestfreezer | 183 min | 90 min | +2.17% | +0.33% |

The control behaves exactly as it must — flat across every shift, because it has no counter
to lag. But the minimising lag shows **no relationship** to tick interval, and if anything
runs backwards: the shortest tick intervals want the longest shifts. The curves also wander
non-monotonically (basement-fridge −0.42 → +2.66 → +1.63) rather than showing the smooth
minimum a genuine systematic lag would leave.

**Test 2 — remove quantisation entirely.** `avg_w` is an unquantised 30-minute mean, so
C2's estimate (shape from power, magnitude from the counter) can be computed directly and
compared with C1's:

| device | C1 eff | C2 eff | C1 − C2 | window mean |
|---|---|---|---|---|
| basement-fridge | 27.65p | 28.54p | −0.90p | 27.76p |
| bigfreezer | 28.11p | 28.27p | −0.16p | 27.78p |
| bigfridge | 28.15p | 28.53p | −0.38p | 27.78p |
| chestfreezer | 28.36p | 27.79p | **+0.57p** | 27.76p |
| dehumidifier | 27.22p | 27.98p | −0.76p | 27.24p |
| winefridge | 28.06p | 27.74p | **+0.32p** | 27.78p |

This is decisive. **C2 sits above the window mean too** — 28.54, 28.27, 28.53, 27.98 against
means of ~27.76. The positive offset survives when quantisation is removed altogether, so it
cannot be a quantisation artefact of any kind, lag included. These devices really do consume
more in dearer half hours, which is what the original "partly real" reading said: compressor
duty tracks ambient temperature, and ambient temperature and the evening price peak share a
daily shape.

Two consequences, and both cut against the reviewer's conclusion that this strengthens C2:

- **C1 and C2 disagree by 0.16–0.90p/kWh on a ~28p rate — under 1%, with mixed signs**
  (four negative, two positive, mean −0.22p). C2 would not remove the offset, because the
  offset is not an error. It would cost a second query path to move the number by less than
  the noise.
- **The §4 decision stands, and on firmer ground.** Previously C1 rested on quantisation
  error being unbiased; it now rests on a measurement showing the residual is not
  quantisation at all.

Caveats: four weeks and six devices; energy from the deployed service's `/series`, prices
from the local archive; per-device means computed over each device's own observed span. A
longer run would tighten it, and the collector keeps accumulating. But the *sign* result —
that C2 shows the same offset — does not depend on precision.

## 5. When to revisit

Triggers for reconsidering, in rough order of likelihood:

1. **A finer-grained device appears.** The whole problem is the 0.1 kWh step. A plug
   reporting to 0.001 kWh makes C1 exact and the question moot for that device.
2. **Someone acts on a single day's per-device figure** and is misled. The month is fine;
   the day is noisy, and the `effective_rate` field is the mitigation rather than a fix.
3. **A high-value low-draw load appears** — something that draws little but runs constantly
   and matters financially. Quantisation error is proportionally worst there.
4. **PR #33's integral path turns out to fix the ∫power bias.** That is the single thing that
   would most change C2's cost/benefit, and it is now cheap to measure.

### The seam

C1 and C2 differ only in how per-slot per-device kWh is derived. Both then multiply by the
same price curve. So C2 is an alternative implementation behind the same interface, selected
by a request parameter (`source=counter|power`), with `attribution` on the response already
naming which ran. Nothing about choosing C1 now forecloses C2.

### Reproducing these numbers

Both tables came from live data, not fixtures:

- per-device 30m kWh from `GET /devices/{id}/series?window=custom&from=&to=&interval=30m`,
  fetched in 10-day chunks because 1488 buckets exceeds `MaxBuckets` (1000);
- prices from the Octopus product endpoint for the same window, `value_inc_vat`;
- C1 = `Σ kWh_slot × price_slot`; C3 = `total kWh × unweighted mean price`; deviation =
  `C1/C3 − 1`.

The August figures are a counterfactual — the Agile agreement only began 2026-09-10, so that
month's consumption is priced at that month's published Agile prices. That is legitimate for
the statistical question (does quantisation error cancel?) and not a claim about any bill.
