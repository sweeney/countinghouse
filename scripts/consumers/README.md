# Consumer scripts

Research tooling for issue #36, not part of the service.

These talk to the fixture served by `TestConsumerHarness`
(`internal/httpapi/consumer_harness_test.go`) over HTTP and to nothing else — no
repo access, no Influx, no Flux. That constraint is the point: issue #36 is
feedback from someone holding only the API, and the only honest way to weigh it
is to work under the same limit.

```sh
CH_HARNESS=1 CH_PORT=8787 CH_SECONDS=600 \
  go test ./internal/httpapi -run TestConsumerHarness -count=1 -v &

cd scripts/consumers && python3 c1_shift.py
```

| Script | The end-user question it is trying to answer |
|---|---|
| `c1_shift.py` | Has household load shifted in response to half-hourly prices? |
| `c2_counterfactual.py` | Is the half-hourly tariff beating the fixed alternative? |
| `c3_scheduler.py` | When should I run the dishwasher, and what did deferring save? |
| `c4_split.py` | Split the actual bill across rooms; how often is it cheap by season? |
| `c6_budget.py` | Am I on track this month, and is the config I am billed from current? |
| `c7_proposed.py` | The same questions against the PROPOSED API (`mockapi.py`). |

`mockapi.py` mocks the proposed `/series?prices=true` and `/compare` shapes by
deriving them from the live API, so a proposal can be judged by rewriting a real
consumer against it rather than by reading a schema. Every figure it prints comes
from the harness; none is invented.

The fixture's own properties — a bill that reconciles, a meter above monitored, a
complete priced curve, negative prices in range and a load shift that is actually
detectable — are asserted by `TestConsumerFixtureIsPlausible`, which runs in CI.
