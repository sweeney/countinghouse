package octopus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test harness
//
// Every test in this package runs against an httptest server. Nothing here
// ever reaches api.octopus.energy: a test suite that depended on a third
// party's uptime would be useless exactly when we needed it, and we could not
// provoke a 429 or a 503 on demand. The fixtures under testdata/ are real
// recorded responses, so the shapes are honest even though the transport is not.
// ---------------------------------------------------------------------------

// agileN is the tariff countinghouse actually bills on. Parsed once so the
// tests exercise the same path-building the collector will.
func agileN(t *testing.T) TariffCode {
	t.Helper()
	tc, err := ParseTariffCode("E-1R-AGILE-24-10-01-N")
	if err != nil {
		t.Fatalf("fixture tariff code should parse: %v", err)
	}
	return tc
}

// fixture reads a recorded API response from testdata/.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// recordedRequest captures what the client actually sent, so tests can assert
// on the request as well as the response handling.
type recordedRequest struct {
	Path   string
	Query  url.Values
	Header http.Header
}

// testServer is an httptest server with a programmable handler and a record of
// every request it received.
type testServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
}

func (ts *testServer) got() []recordedRequest {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]recordedRequest, len(ts.requests))
	copy(out, ts.requests)
	return out
}

// newTestServer starts a server whose handler is called for every request, with
// the zero-based request index so a handler can behave differently on a retry.
func newTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, attempt int)) *testServer {
	t.Helper()
	ts := &testServer{}
	ts.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		attempt := len(ts.requests)
		ts.requests = append(ts.requests, recordedRequest{
			Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone(),
		})
		ts.mu.Unlock()
		handler(w, r, attempt)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// serveFixture is the common case: always answer 200 with one recorded file.
func serveFixture(t *testing.T, name string) *testServer {
	t.Helper()
	body := fixture(t, name)
	return newTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body) //nolint:errcheck
	})
}

// fakeSleeper records the backoff durations it was asked for and returns
// immediately. Production code must never block a test: if a retry test takes
// real seconds, nobody will run it.
type fakeSleeper struct {
	mu     sync.Mutex
	slept  []time.Duration
	failAt int  // return ctx error on this call index (-1 = never)
	ctxErr bool // whether failAt has been reached
}

func newFakeSleeper() *fakeSleeper { return &fakeSleeper{failAt: -1} }

func (f *fakeSleeper) Sleep(ctx context.Context, d time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAt >= 0 && len(f.slept) == f.failAt {
		f.ctxErr = true
		return context.Canceled
	}
	f.slept = append(f.slept, d)
	return nil
}

func (f *fakeSleeper) durations() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Duration, len(f.slept))
	copy(out, f.slept)
	return out
}

// newClient builds a client pointed at ts with a non-sleeping sleeper.
func newClient(t *testing.T, ts *testServer, sleeper *fakeSleeper) *Client {
	t.Helper()
	if sleeper == nil {
		sleeper = newFakeSleeper()
	}
	c, err := New(Options{BaseURL: ts.URL, Sleep: sleeper.Sleep})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

// The base URL is the one knob that decides where credentials and queries go,
// so a malformed one must fail at construction rather than at the first
// request — and certainly not default silently to the live API during a test.
func TestNewValidatesBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		baseURL string
		wantErr bool
	}{
		{name: "https is fine", baseURL: "https://api.octopus.energy/v1"},
		{name: "http is fine for a local test server", baseURL: "http://127.0.0.1:8080"},
		{name: "a trailing slash is tolerated", baseURL: "https://api.octopus.energy/v1/"},
		{name: "empty is refused", baseURL: "", wantErr: true},
		{name: "no scheme is refused", baseURL: "api.octopus.energy", wantErr: true},
		{name: "a non-http scheme is refused", baseURL: "file:///etc/passwd", wantErr: true},
		{name: "unparseable is refused", baseURL: "http://[::1", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Options{BaseURL: tc.baseURL})
			if tc.wantErr && err == nil {
				t.Errorf("New(%q) = nil error, want error", tc.baseURL)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("New(%q) unexpected error: %v", tc.baseURL, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Parsing real recorded responses
// ---------------------------------------------------------------------------

func TestUnitRatesParsesRecordedResponse(t *testing.T) {
	ts := serveFixture(t, "unit_rates_simple.json")
	c := newClient(t, ts, nil)

	rates, err := c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("UnitRates: %v", err)
	}
	if len(rates) != 4 {
		t.Fatalf("got %d rates, want 4", len(rates))
	}

	// The API returns newest-first. Every consumer (contiguity checks, curve
	// rendering, gap detection) wants oldest-first, so the client re-sorts and
	// that is part of its contract rather than an accident of the response.
	for i := 1; i < len(rates); i++ {
		if !rates[i].ValidFrom.After(rates[i-1].ValidFrom) {
			t.Errorf("rates not ascending by ValidFrom: [%d]=%s then [%d]=%s",
				i-1, rates[i-1].ValidFrom, i, rates[i].ValidFrom)
		}
	}

	// Slots are half-open and exactly 30 minutes on this product.
	for i, r := range rates {
		if r.ValidTo == nil {
			t.Fatalf("rate %d has a nil ValidTo; Agile slots are bounded", i)
		}
		if d := r.ValidTo.Sub(r.ValidFrom); d != 30*time.Minute {
			t.Errorf("rate %d spans %v, want 30m", i, d)
		}
		if r.ValidFrom.Location() != time.UTC {
			t.Errorf("rate %d ValidFrom is in %v, want UTC", i, r.ValidFrom.Location())
		}
	}

	// Money is carried through in pence, both VAT forms, unrounded. Deriving
	// inc from exc would bake today's VAT rate into a permanent record.
	var wire struct {
		Results []struct {
			ValueExcVAT float64 `json:"value_exc_vat"`
			ValueIncVAT float64 `json:"value_inc_vat"`
			ValidFrom   string  `json:"valid_from"`
		} `json:"results"`
	}
	if err := json.Unmarshal(fixture(t, "unit_rates_simple.json"), &wire); err != nil {
		t.Fatal(err)
	}
	want := map[string][2]float64{}
	for _, w := range wire.Results {
		want[w.ValidFrom] = [2]float64{w.ValueExcVAT, w.ValueIncVAT}
	}
	for _, r := range rates {
		key := r.ValidFrom.Format("2006-01-02T15:04:05Z")
		w, ok := want[key]
		if !ok {
			t.Fatalf("parsed a slot %s that is not in the fixture", key)
		}
		if r.ExcVATPence != w[0] || r.IncVATPence != w[1] {
			t.Errorf("slot %s = (%v, %v), want (%v, %v) exactly as delivered",
				key, r.ExcVATPence, r.IncVATPence, w[0], w[1])
		}
	}
}

// Negative prices are the whole point of Agile: when the grid is oversupplied
// you are paid to consume. A client that clamped, dropped or abs()'d them would
// destroy the most valuable slots in the archive.
func TestUnitRatesPreservesNegativePrices(t *testing.T) {
	ts := serveFixture(t, "unit_rates_negative.json")
	c := newClient(t, ts, nil)

	rates, err := c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("UnitRates: %v", err)
	}

	var negatives int
	for _, r := range rates {
		if r.ExcVATPence < 0 {
			negatives++
			// VAT on a negative price makes it MORE negative. Verified against
			// the live API: inc is exactly exc x 1.05 across every slot.
			if r.IncVATPence >= r.ExcVATPence {
				t.Errorf("slot %s: inc %v should be more negative than exc %v",
					r.ValidFrom, r.IncVATPence, r.ExcVATPence)
			}
		}
	}
	if negatives == 0 {
		t.Fatal("the negative-price fixture contains no negative slots; it is not testing what it claims")
	}
	t.Logf("preserved %d negative slots out of %d", negatives, len(rates))
}

// A local day is 46, 48 or 50 half-hours depending on the DST changeover. The
// client must not assume 48 anywhere, and must not fabricate or drop slots to
// reach it.
func TestUnitRatesHandlesDSTDayLengths(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		want    int
		spanned time.Duration
	}{
		{name: "spring forward, 23h local day", file: "unit_rates_dst_spring_46.json", want: 46, spanned: 23 * time.Hour},
		{name: "autumn back, 25h local day", file: "unit_rates_dst_autumn_50.json", want: 50, spanned: 25 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := serveFixture(t, tc.file)
			c := newClient(t, ts, nil)

			rates, err := c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("UnitRates: %v", err)
			}
			if len(rates) != tc.want {
				t.Fatalf("got %d slots, want %d", len(rates), tc.want)
			}

			// Contiguous: each slot begins exactly where the last ended. This is
			// what makes the set a usable price curve rather than a bag of rows.
			for i := 1; i < len(rates); i++ {
				if !rates[i].ValidFrom.Equal(*rates[i-1].ValidTo) {
					t.Errorf("gap or overlap between slot %d (ends %s) and %d (starts %s)",
						i-1, rates[i-1].ValidTo, i, rates[i].ValidFrom)
				}
			}
			// The real elapsed span is the DST-shortened or -lengthened day,
			// which is the point: the axis is real time, not calendar hours.
			if got := rates[len(rates)-1].ValidTo.Sub(rates[0].ValidFrom); got != tc.spanned {
				t.Errorf("span = %v, want %v", got, tc.spanned)
			}
		})
	}
}

// Standing charges are the same shape as unit rates but open-ended: the current
// row has valid_to null. nil must survive as nil — coercing it to a zero time
// would make the rate look long expired.
func TestStandingChargesOpenEndedValidTo(t *testing.T) {
	ts := serveFixture(t, "standing_charges.json")
	c := newClient(t, ts, nil)

	rates, err := c.StandingCharges(context.Background(), agileN(t))
	if err != nil {
		t.Fatalf("StandingCharges: %v", err)
	}
	if len(rates) == 0 {
		t.Fatal("no standing charges parsed")
	}
	last := rates[len(rates)-1]
	if last.ValidTo != nil {
		t.Errorf("newest standing charge ValidTo = %v, want nil (open-ended)", last.ValidTo)
	}
	if last.ExcVATPence <= 0 {
		t.Errorf("standing charge %v should be positive", last.ExcVATPence)
	}

	// Path check: standing charges live under a different final segment.
	got := ts.got()
	if len(got) != 1 {
		t.Fatalf("made %d requests, want 1", len(got))
	}
	wantPath := "/products/AGILE-24-10-01/electricity-tariffs/E-1R-AGILE-24-10-01-N/standing-charges/"
	if got[0].Path != wantPath {
		t.Errorf("path = %q, want %q", got[0].Path, wantPath)
	}
}

// payment_method is part of the archive key because it has to be: on variable
// tariffs the SAME slot appears twice, once per payment method, with different
// prices. A client that dropped the field would make those rows collide.
func TestUnitRatesCarriesPaymentMethod(t *testing.T) {
	ts := serveFixture(t, "unit_rates_payment_methods.json")
	c, err := New(Options{BaseURL: ts.URL, Sleep: newFakeSleeper().Sleep})
	if err != nil {
		t.Fatal(err)
	}
	varN, err := ParseTariffCode("E-1R-VAR-22-11-01-N")
	if err != nil {
		t.Fatal(err)
	}

	rates, err := c.UnitRates(context.Background(), varN, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("UnitRates: %v", err)
	}

	byStart := map[time.Time][]Rate{}
	methods := map[string]bool{}
	for _, r := range rates {
		byStart[r.ValidFrom] = append(byStart[r.ValidFrom], r)
		methods[r.PaymentMethod] = true
	}
	if !methods["DIRECT_DEBIT"] || !methods["NON_DIRECT_DEBIT"] {
		t.Fatalf("expected both payment methods, got %v", methods)
	}

	var collided bool
	for start, rs := range byStart {
		if len(rs) < 2 {
			continue
		}
		collided = true
		if rs[0].PaymentMethod == rs[1].PaymentMethod {
			t.Errorf("slot %s has two rows with the same payment method", start)
		}
		if rs[0].ExcVATPence == rs[1].ExcVATPence {
			t.Errorf("slot %s: the two payment methods priced identically (%v); "+
				"the fixture is not demonstrating the collision", start, rs[0].ExcVATPence)
		}
	}
	if !collided {
		t.Fatal("no slot appeared twice; this fixture is meant to prove valid_from alone is not unique")
	}
}

// Agile slots have payment_method null. That must normalise to "" rather than
// staying a nil pointer, so the archive key is a comparable value.
func TestUnitRatesNullPaymentMethodBecomesEmpty(t *testing.T) {
	ts := serveFixture(t, "unit_rates_simple.json")
	c := newClient(t, ts, nil)

	rates, err := c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rates {
		if r.PaymentMethod != "" {
			t.Errorf("slot %s payment method = %q, want \"\"", r.ValidFrom, r.PaymentMethod)
		}
	}
}

// ---------------------------------------------------------------------------
// Request shape
// ---------------------------------------------------------------------------

// period_from/period_to must always go out as UTC with a trailing Z. The API
// misreads local-time values across the DST changeover, which would silently
// fetch the wrong hour on exactly the two days a year that are hardest to debug.
func TestUnitRatesSendsUTCPeriodParams(t *testing.T) {
	ts := serveFixture(t, "unit_rates_simple.json")
	c := newClient(t, ts, nil)

	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	// Deliberately hand it BST-local times; they must be converted, not formatted.
	from := time.Date(2026, 9, 11, 1, 0, 0, 0, london) // 00:00Z
	to := time.Date(2026, 9, 12, 1, 0, 0, 0, london)   // 2026-09-12T00:00Z

	if _, err := c.UnitRates(context.Background(), agileN(t), from, to); err != nil {
		t.Fatalf("UnitRates: %v", err)
	}

	got := ts.got()
	if len(got) != 1 {
		t.Fatalf("made %d requests, want 1", len(got))
	}
	q := got[0].Query
	if q.Get("period_from") != "2026-09-11T00:00:00Z" {
		t.Errorf("period_from = %q, want 2026-09-11T00:00:00Z", q.Get("period_from"))
	}
	if q.Get("period_to") != "2026-09-12T00:00:00Z" {
		t.Errorf("period_to = %q, want 2026-09-12T00:00:00Z", q.Get("period_to"))
	}
	wantPath := "/products/AGILE-24-10-01/electricity-tariffs/E-1R-AGILE-24-10-01-N/standard-unit-rates/"
	if got[0].Path != wantPath {
		t.Errorf("path = %q, want %q", got[0].Path, wantPath)
	}
	if ua := got[0].Header.Get("User-Agent"); ua == "" {
		t.Error("no User-Agent sent; the API's WAF rejects some default agents")
	}
}

// A zero from/to means "no bound" — used by the horizon probe and by backfill.
// Sending an empty period_from= would be a different request entirely.
func TestUnitRatesOmitsZeroPeriodParams(t *testing.T) {
	ts := serveFixture(t, "unit_rates_simple.json")
	c := newClient(t, ts, nil)

	if _, err := c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	q := ts.got()[0].Query
	if _, ok := q["period_from"]; ok {
		t.Errorf("period_from should be absent, got %q", q.Get("period_from"))
	}
	if _, ok := q["period_to"]; ok {
		t.Errorf("period_to should be absent, got %q", q.Get("period_to"))
	}
}

// ---------------------------------------------------------------------------
// The horizon probe — the publication-detection signal
// ---------------------------------------------------------------------------

// Detecting a new publication must not cost a full day's download. Results are
// newest-first, so page_size=1 yields the newest slot's valid_to in ~344 bytes.
func TestHorizonAsksForOneRowOnly(t *testing.T) {
	ts := serveFixture(t, "unit_rates_horizon.json")
	c := newClient(t, ts, nil)

	horizon, err := c.Horizon(context.Background(), agileN(t))
	if err != nil {
		t.Fatalf("Horizon: %v", err)
	}

	got := ts.got()
	if len(got) != 1 {
		t.Fatalf("Horizon made %d requests, want exactly 1", len(got))
	}
	if ps := got[0].Query.Get("page_size"); ps != "1" {
		t.Errorf("page_size = %q, want 1 — the probe must stay cheap", ps)
	}

	// The fixture's single row is the newest slot; its valid_to is the horizon.
	var wire struct {
		Results []struct {
			ValidTo string `json:"valid_to"`
		} `json:"results"`
	}
	if err := json.Unmarshal(fixture(t, "unit_rates_horizon.json"), &wire); err != nil {
		t.Fatal(err)
	}
	want, err := time.Parse(time.RFC3339, wire.Results[0].ValidTo)
	if err != nil {
		t.Fatal(err)
	}
	if !horizon.Equal(want) {
		t.Errorf("horizon = %s, want %s", horizon, want)
	}
}

// An empty result set is not an error — a brand-new product legitimately has no
// rates yet — but it must be distinguishable from "we have prices through X".
func TestHorizonEmptyResultsReportsNoHorizon(t *testing.T) {
	ts := newTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Write([]byte(`{"count":0,"next":null,"previous":null,"results":[]}`)) //nolint:errcheck
	})
	c := newClient(t, ts, nil)

	horizon, err := c.Horizon(context.Background(), agileN(t))
	if err != nil {
		t.Fatalf("an empty product should not be an error: %v", err)
	}
	if !horizon.IsZero() {
		t.Errorf("horizon = %s, want the zero time to mean 'nothing published'", horizon)
	}
}

// ---------------------------------------------------------------------------
// Pagination
// ---------------------------------------------------------------------------

// page_size is silently CLAMPED to 1500 by the API, not rejected: asking for
// 2000 returns 200 with 1500 rows and a next link. So the client must follow
// next and must never assume it received what it asked for.
func TestUnitRatesFollowsPagination(t *testing.T) {
	var ts *testServer
	ts = newTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprintf(w, `{"count":3,"next":null,"previous":null,"results":[
				{"value_exc_vat":10.0,"value_inc_vat":10.5,
				 "valid_from":"2026-09-10T00:00:00Z","valid_to":"2026-09-10T00:30:00Z","payment_method":null}]}`)
			return
		}
		fmt.Fprintf(w, `{"count":3,"next":%q,"previous":null,"results":[
			{"value_exc_vat":30.0,"value_inc_vat":31.5,
			 "valid_from":"2026-09-10T01:00:00Z","valid_to":"2026-09-10T01:30:00Z","payment_method":null},
			{"value_exc_vat":20.0,"value_inc_vat":21.0,
			 "valid_from":"2026-09-10T00:30:00Z","valid_to":"2026-09-10T01:00:00Z","payment_method":null}]}`,
			ts.URL+"/next-page/?page=2")
	})
	c := newClient(t, ts, nil)

	rates, err := c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("UnitRates: %v", err)
	}
	if len(rates) != 3 {
		t.Fatalf("got %d rates across pages, want 3", len(rates))
	}
	// Still globally sorted, not just per page.
	for i := 1; i < len(rates); i++ {
		if !rates[i].ValidFrom.After(rates[i-1].ValidFrom) {
			t.Errorf("pages not merged in order at %d: %s then %s",
				i, rates[i-1].ValidFrom, rates[i].ValidFrom)
		}
	}
	if n := len(ts.got()); n != 2 {
		t.Errorf("made %d requests, want 2 (one per page)", n)
	}
}

// `next` is a URL from the response body, i.e. attacker-influenced input if the
// API or anything in front of it is ever compromised or misconfigured. Following
// it to another host would turn our collector into a request forwarder and could
// leak an Authorization header. It must be refused.
func TestUnitRatesRefusesCrossHostNextLink(t *testing.T) {
	evil := newTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		t.Error("client followed `next` to a different host")
		w.Write([]byte(`{"count":0,"next":null,"previous":null,"results":[]}`)) //nolint:errcheck
	})

	ts := newTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		fmt.Fprintf(w, `{"count":2,"next":%q,"previous":null,"results":[
			{"value_exc_vat":10.0,"value_inc_vat":10.5,
			 "valid_from":"2026-09-10T00:00:00Z","valid_to":"2026-09-10T00:30:00Z","payment_method":null}]}`,
			evil.URL+"/v1/rates/")
	})
	c := newClient(t, ts, nil)

	_, err := c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{})
	if err == nil {
		t.Fatal("expected an error when `next` points at another host")
	}
	if len(evil.got()) != 0 {
		t.Errorf("the foreign host received %d requests, want 0", len(evil.got()))
	}
}

// A `next` that loops back to the same page would spin forever on a server bug.
// The client bounds the walk instead of trusting the server to terminate it.
func TestUnitRatesBoundsPaginationLoop(t *testing.T) {
	var ts *testServer
	ts = newTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		// Always claims there is another page, and always the same one.
		fmt.Fprintf(w, `{"count":99999,"next":%q,"previous":null,"results":[
			{"value_exc_vat":10.0,"value_inc_vat":10.5,
			 "valid_from":"2026-09-10T00:00:00Z","valid_to":"2026-09-10T00:30:00Z","payment_method":null}]}`,
			ts.URL+"/loop/?page=2")
	})
	c := newClient(t, ts, nil)

	_, err := c.UnitRates(context.Background(), agileN(t), time.Time{}, time.Time{})
	if err == nil {
		t.Fatal("expected an error when pagination does not terminate")
	}
	if n := len(ts.got()); n > maxPages+1 {
		t.Errorf("made %d requests; the walk should be bounded near %d", n, maxPages)
	}
	t.Logf("bounded after %d requests", len(ts.got()))
}
