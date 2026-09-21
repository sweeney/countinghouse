package httpapi

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"
)

// ---------------------------------------------------------------------------
// The consumer-level half of issue #36.
//
// The harness exists because the findings in #36 were only reachable from
// OUTSIDE the service — a unit test shares the server's assumptions, and every
// one of them passed while the API was doing the thing the report described.
// The series that answered #36 was then verified almost entirely by unit tests,
// which left the harness guarding the fixture and nothing else.
//
// These are the assertions that need the consumer's vantage point: the ones about
// what a response CARRIES, checked the way a caller would check it, against a
// fixture built to produce the awkward conditions on purpose (a clamped residual,
// a stale namespace, a statutory zero-VAT block, an unaligned window).
//
// Two of them would have caught real defects. The price-clip bug was found this
// way and nowhere else.
// ---------------------------------------------------------------------------

// The R2.4 promise, checked the way the README now tells a consumer to check it:
// on the per-bucket arrays, not on total_kwh.
//
// Both halves matter. The arrays must balance EXACTLY, because that is the
// documented identity. The totals must be close but are allowed not to balance,
// because each series' total accumulates raw values and rounds once — and a
// consumer who reaches for the totals first (most will) needs the gap to be
// small enough to read as rounding rather than as a hole.
func TestConsumerClampIdentityHoldsOnTheArrays(t *testing.T) {
	fx := chBuild(t)

	w := doGET(t, fx.Server, "/series?window=7d&interval=30m&group_by=house")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var r struct {
		Series []struct {
			Key      string    `json:"key"`
			KWh      []float64 `json:"kwh"`
			TotalKWh float64   `json:"total_kwh"`
		} `json:"series"`
		Clamp *struct {
			KWh     float64 `json:"kwh"`
			Buckets int     `json:"buckets"`
		} `json:"clamp"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}

	sum := map[string]float64{}
	totals := map[string]float64{}
	for _, s := range r.Series {
		for _, v := range s.KWh {
			sum[s.Key] += v
		}
		totals[s.Key] = s.TotalKWh
	}
	for _, k := range []string{"monitored", "unmonitored", "meter"} {
		if _, ok := sum[k]; !ok {
			t.Fatalf("no %q series: %s", k, w.Body.String())
		}
	}

	// The fixture clamps on purpose, so the block must be present. Its absence
	// would mean either the clamp stopped firing or the block stopped being sent,
	// and from out here those are the same bytes.
	if r.Clamp == nil {
		t.Fatal("no clamp block, but the fixture is built to produce a clamped residual")
	}

	parts := sum["monitored"] + sum["unmonitored"]
	meter := sum["meter"] + r.Clamp.KWh
	if math.Abs(parts-meter) > 1e-6 {
		t.Errorf("on the ARRAYS: monitored+unmonitored = %.6f, meter+clamp = %.6f, diff %.6f — "+
			"this identity is documented as exact", parts, meter, parts-meter)
	}

	// On the totals it is allowed to differ, by rounding and nothing else. The
	// bound is loose against the axis (a few hundred buckets) and tight against
	// the clamp, so a real regression in the decomposition still fails here.
	tParts := totals["monitored"] + totals["unmonitored"]
	tMeter := totals["meter"] + r.Clamp.KWh
	if d := math.Abs(tParts - tMeter); d > 0.5 {
		t.Errorf("on the TOTALS: diff %.6f is too large to be per-bucket rounding", d)
	} else {
		t.Logf("totals differ by %.6f kWh (per-bucket rounding); arrays balance exactly", d)
	}
}

// Staleness has to be visible in the response a consumer already has, not one
// /healthz call away. The fixture holds one namespace that FAILED its last fetch,
// because a fixture where everything succeeded can only exercise the happy half.
func TestConsumerSeesFreshnessAtThePointOfUse(t *testing.T) {
	fx := chBuild(t)

	for _, path := range []string{"/tariffs", "/floors", "/rooms"} {
		w := doGET(t, fx.Server, path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		m := decode(t, w)
		if _, ok := m["fetched_at"]; !ok {
			t.Errorf("%s: no fetched_at, so a consumer cannot tell how old this is", path)
		}
		stale, ok := m["stale"].(bool)
		if !ok {
			t.Errorf("%s: no stale flag", path)
			continue
		}
		// These three read namespaces the fixture fetched successfully.
		if stale {
			t.Errorf("%s: stale = true, but its namespace fetched cleanly", path)
		}
	}

	// And the failed namespace is visible somewhere a consumer will meet it,
	// with the error text — which stays on /healthz rather than riding every
	// response.
	h := decode(t, doGET(t, fx.Server, "/healthz"))
	ns, ok := h["remote_config"].(map[string]any)
	if !ok {
		t.Fatalf("/healthz carries no remote_config block: %v", h)
	}
	dev, ok := ns["devices_harness"].(map[string]any)
	if !ok {
		t.Fatalf("/healthz does not report the devices namespace: %v", ns)
	}
	if dev["ok"] != false {
		t.Errorf("devices namespace ok = %v, want false: its last fetch failed", dev["ok"])
	}
	if dev["error"] == nil || dev["error"] == "" {
		t.Error("a failed fetch must carry its error text on /healthz")
	}
	// Fail-open: a stale namespace degrades health, it does not refuse service.
	// Every route above answered 200 while this is true, which is the behaviour
	// the point-of-use signal exists to make visible.
	if h["status"] != "degraded" {
		t.Errorf("status = %v, want degraded: a namespace is serving a stale snapshot", h["status"])
	}
}

// The config gate must be SILENT on plausible config. A health signal that is
// noisy on correct documents is one operators learn to ignore, which costs more
// than not having it.
//
// The fixture is the right place to assert this: it carries a statutory
// ZERO-VAT block, which is exactly the shape a naive band would flag — a VAT
// rate of 0 among neighbours at 0.05.
func TestConsumerHealthzIsQuietOnPlausibleConfig(t *testing.T) {
	fx := chBuild(t)

	h := decode(t, doGET(t, fx.Server, "/healthz"))
	if warnings, present := h["config_warnings"]; present {
		t.Errorf("config_warnings on a plausible document (the zero-VAT block is "+
			"statutory, not a typo): %v", warnings)
	}
	// Quiet about the CONTENT of the config, which is a different axis from
	// whether a fetch succeeded. This fixture is `degraded` for a stale
	// namespace, and the gate must not add to that on rates it has no quarrel
	// with — the two signals answer different questions and conflating them is
	// how an operator learns to ignore both.
	if _, present := h["config_warnings"]; present {
		t.Error("the gate contributed a warning to an already-degraded health report")
	}
}

// prices[] on a window whose start falls inside the first bucket.
//
// This is the shape a consumer paging historical periods issues, and the one
// that found the clip bug: the axis is built on calendar boundaries, so the
// first bucket is labelled before the window begins. Its kwh and cost describe
// only the covered part, and its price must too — otherwise a fully priced
// bucket reports null, and unpriced_buckets says the window is incomplete when
// it is not.
func TestConsumerPricesAnUnalignedWindow(t *testing.T) {
	fx := chBuild(t)

	// A day inside the fixture's priced range, entered an hour after the local
	// day boundary so the first calendar bucket is clipped.
	from := fx.From.Add(48 * 60 * 60 * 1e9).UTC()
	to := from.Add(48 * 60 * 60 * 1e9)
	path := "/series?window=custom" +
		"&from=" + from.Format("2006-01-02T15:04:05Z") +
		"&to=" + to.Format("2006-01-02T15:04:05Z") +
		"&interval=1d&group_by=house&prices=true"

	w := doGET(t, fx.Server, path)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var r struct {
		Buckets         []string   `json:"buckets"`
		Prices          []*float64 `json:"prices"`
		UnpricedBuckets *int       `json:"unpriced_buckets"`
		PriceUnit       string     `json:"price_unit"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(r.Prices) != len(r.Buckets) {
		t.Fatalf("len(prices)=%d, len(buckets)=%d — the one contract a consumer zips against",
			len(r.Prices), len(r.Buckets))
	}
	if r.PriceUnit == "" {
		t.Error("no price_unit, so the array's scale is a guess")
	}
	if r.UnpricedBuckets == nil {
		t.Fatal("no unpriced_buckets, so 0 cannot assert completeness")
	}
	if *r.UnpricedBuckets != 0 {
		t.Errorf("unpriced_buckets = %d over a window the archive fully covers", *r.UnpricedBuckets)
	}
	for i, p := range r.Prices {
		if p == nil {
			t.Errorf("prices[%d] (%s) is null, but the archive covers it", i, r.Buckets[i])
		}
	}
}

// The window-derived routes say how long an answer may be reused and carry no
// ETag, and a consumer polling them needs both facts — the second as much as the
// first, since the price routes DO tag and the difference is not guessable.
func TestConsumerWindowRoutesAreCacheableButNotTagged(t *testing.T) {
	fx := chBuild(t)

	for _, path := range []string{
		"/series?window=7d&group_by=house",
		"/devices/unmonitored/series?window=7d",
		"/bill?window=7d",
	} {
		w := doGET(t, fx.Server, path)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") == "" {
			t.Errorf("%s: no Cache-Control, so a poller has no guidance", path)
		}
		if et := w.Header().Get("ETag"); et != "" {
			t.Errorf("%s: ETag %q — there is no honest strong validator over Influx", path, et)
		}
	}

	// The price routes DO tag, and that asymmetry is the point rather than an
	// inconsistency. Asserting it here keeps the two from quietly converging.
	p := doGET(t, fx.Server, "/prices?window=today")
	if p.Code == http.StatusOK && p.Header().Get("ETag") == "" {
		t.Error("/prices lost its ETag: it has a semantic fingerprint and should use it")
	}
}
