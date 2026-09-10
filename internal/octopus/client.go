package octopus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/sweeney/countinghouse/internal/testutil"
)

const (
	// pageSize is what we ask for. The API silently CLAMPS this to 1500 rather
	// than rejecting it, so the number is a request and not a guarantee — the
	// pagination walk below never assumes it got what it asked for.
	pageSize = 1500

	// maxPages bounds the pagination walk. A month of half-hourly rates is two
	// pages and the full product history is ~24, so this is generous; it exists
	// because a server bug where `next` loops would otherwise spin forever.
	maxPages = 200

	// maxErrorBody caps how much of a failed response we retain for diagnostics.
	// An upstream error page can be megabytes, and all of it would end up in a
	// log line.
	maxErrorBody = 2048

	// maxRetryAfter clamps both our own backoff and any Retry-After the server
	// sends. Obeying an absurd or hostile value would park the collector past
	// the publication window it was woken up for.
	maxRetryAfter = 2 * time.Minute

	// baseBackoff is the first wait when the server gives us no Retry-After.
	baseBackoff = 500 * time.Millisecond

	// userAgent identifies us. The API sits behind a WAF that rejects some
	// default agents, so this is not cosmetic.
	userAgent = "countinghouse/1.0 (+https://github.com/sweeney/countinghouse)"
)

// Options configures a Client.
type Options struct {
	// BaseURL is the API root, e.g. "https://api.octopus.energy/v1". Required:
	// there is deliberately no default, so a misconfigured test cannot quietly
	// reach the live API.
	BaseURL string

	// HTTPClient is optional; a bounded-timeout client is used when nil.
	HTTPClient *http.Client

	// Clock sources the current time. Only used to turn an HTTP-date
	// Retry-After into a duration. Defaults to testutil.RealClock{}.
	Clock testutil.Clock

	// Sleep waits for d or returns ctx's error, whichever comes first. Injected
	// so retry tests are instant and deterministic. Defaults to a real wait.
	Sleep func(ctx context.Context, d time.Duration) error

	// MaxRetries is how many times a retryable failure is retried (so total
	// attempts is MaxRetries+1). Zero means no retries.
	MaxRetries int

	// Logger is optional; slog.Default() is used when nil.
	Logger *slog.Logger
}

// Client is a read-only Octopus Energy API client.
//
// It is safe for concurrent use. It performs no caching and no validation
// beyond what is needed to parse a response and keep a URL path safe — see the
// package comment for where those responsibilities live instead.
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	clock      testutil.Clock
	sleep      func(context.Context, time.Duration) error
	maxRetries int
	log        *slog.Logger
}

// New builds a Client. It fails rather than defaulting when BaseURL is missing
// or unusable: the base URL decides where every request goes, and a silent
// default would mean a test suite quietly talking to the live API.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("octopus: BaseURL is required")
	}
	u, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("octopus: parse BaseURL %q: %w", opts.BaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("octopus: BaseURL %q must be http or https, got scheme %q", opts.BaseURL, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("octopus: BaseURL %q has no host", opts.BaseURL)
	}

	c := &Client{
		baseURL:    u,
		httpClient: opts.HTTPClient,
		clock:      opts.Clock,
		sleep:      opts.Sleep,
		maxRetries: opts.MaxRetries,
		log:        opts.Logger,
	}
	if c.httpClient == nil {
		// A bounded timeout matters: without one a hung connection would hold
		// the collector's goroutine indefinitely.
		c.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if c.clock == nil {
		c.clock = testutil.RealClock{}
	}
	if c.sleep == nil {
		c.sleep = realSleep
	}
	if c.log == nil {
		c.log = slog.Default()
	}
	return c, nil
}

// realSleep waits for d, or returns early with ctx's error if it is cancelled.
func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Rate is one priced interval exactly as the API delivered it.
//
// Money stays in pence with BOTH VAT forms and full precision. inc is not
// derived from exc: the relationship is exactly x1.05 today, but storing a
// computed value would bake today's VAT rate into a permanent record, and the
// delivered inc carries more decimal places than any rounding policy we would
// pick (e.g. 62.11863).
type Rate struct {
	// ValidFrom is the inclusive start of the interval, always in UTC.
	ValidFrom time.Time

	// ValidTo is the EXCLUSIVE end. It is nil when the API sent null, which
	// means open-ended — the current standing charge, and flat-tariff rates
	// that have not yet been superseded. nil must survive as nil: coercing it
	// to a zero time would make a live rate look long expired.
	ValidTo *time.Time

	// ExcVATPence and IncVATPence are SIGNED. Agile goes negative when the grid
	// is oversupplied, and exactly 0.00 is a real price — so neither an
	// unsigned type nor a zero sentinel is usable here.
	ExcVATPence float64
	IncVATPence float64

	// PaymentMethod is "" when the API sent null (as it does for Agile).
	// On variable tariffs the SAME slot appears twice, once as DIRECT_DEBIT and
	// once as NON_DIRECT_DEBIT, at different prices — which is why this is part
	// of the archive's key and not a descriptive extra.
	PaymentMethod string
}

// APIError is a non-2xx response.
type APIError struct {
	StatusCode int
	Status     string
	// Body is the response body, truncated to maxErrorBody. It is usually the
	// only thing that distinguishes "throttled" from "tariff does not exist",
	// so it is kept rather than discarded.
	Body string
	// RetryAfter is the parsed Retry-After header, or 0 when absent.
	RetryAfter time.Duration
	// URL is the request path that failed, for logs. Query included; these
	// endpoints carry no credentials in the URL.
	URL string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("octopus: %s (%d) for %s: %s", e.Status, e.StatusCode, e.URL, e.Body)
}

// Retryable reports whether retrying this status could plausibly succeed.
// 429 and 5xx are transient; every other 4xx is a bug, a bad credential or a
// bad path, and retrying would hammer the API to no purpose.
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= 500
}

// IsRetryable reports whether err is worth retrying. Transport errors (the
// service being down, a dropped connection) are retryable; a cancelled context
// is not — that is our own shutdown, not a remote failure.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	// Anything left is a transport or decode failure. Decode failures are not
	// really retryable, but they are reported distinctly by the caller's logs
	// and a single extra attempt costs little; transport failures dominate.
	var de *decodeError
	return !errors.As(err, &de)
}

// decodeError marks a response we could not parse, so IsRetryable can tell it
// apart from a transport failure.
type decodeError struct{ err error }

func (d *decodeError) Error() string { return d.err.Error() }
func (d *decodeError) Unwrap() error { return d.err }

// page is the DRF-style envelope every list endpoint returns.
type page struct {
	Count    int     `json:"count"`
	Next     *string `json:"next"`
	Previous *string `json:"previous"`
	Results  []struct {
		ValueExcVAT   *float64 `json:"value_exc_vat"`
		ValueIncVAT   *float64 `json:"value_inc_vat"`
		ValidFrom     *string  `json:"valid_from"`
		ValidTo       *string  `json:"valid_to"`
		PaymentMethod *string  `json:"payment_method"`
	} `json:"results"`
}

// UnitRates returns every half-hourly unit rate for the tariff within
// [from, to), oldest first.
//
// A zero from or to omits that bound, which is what backfill and the horizon
// probe want. Both bounds are sent as UTC with a trailing Z: the API misreads
// local-time values across a DST changeover, which would silently fetch the
// wrong hour on exactly the two days a year hardest to debug.
func (c *Client) UnitRates(ctx context.Context, tariff TariffCode, from, to time.Time) ([]Rate, error) {
	return c.rates(ctx, tariff, "standard-unit-rates", from, to, 0)
}

// StandingCharges returns the tariff's daily standing charges, oldest first.
// These change rarely — one row per change, the newest open-ended.
func (c *Client) StandingCharges(ctx context.Context, tariff TariffCode) ([]Rate, error) {
	return c.rates(ctx, tariff, "standing-charges", time.Time{}, time.Time{}, 0)
}

// Horizon returns the end of the newest published slot for the tariff: the
// point our knowledge of prices currently runs out.
//
// This is the publication-detection signal, and it is deliberately cheap — one
// request for one row, because results come newest-first. Comparing it against
// what the archive already holds detects a new publication without downloading
// anything we have.
//
// A zero time means the product has no rates at all, which is not an error: a
// newly launched product legitimately has none yet. The caller must be able to
// tell that apart from "we have prices through X", which is why it is a zero
// time rather than an error.
func (c *Client) Horizon(ctx context.Context, tariff TariffCode) (time.Time, error) {
	rates, err := c.rates(ctx, tariff, "standard-unit-rates", time.Time{}, time.Time{}, 1)
	if err != nil {
		return time.Time{}, err
	}
	var newest time.Time
	for _, r := range rates {
		end := r.ValidFrom
		if r.ValidTo != nil {
			end = *r.ValidTo
		}
		if end.After(newest) {
			newest = end
		}
	}
	return newest, nil
}

// rates fetches and flattens a paginated rate endpoint.
//
// limit overrides the requested page size when non-zero; it exists for the
// horizon probe, which wants exactly one row. When limit is set the walk stops
// after the first page — following `next` would defeat the point of asking for
// one row.
func (c *Client) rates(ctx context.Context, tariff TariffCode, kind string, from, to time.Time, limit int) ([]Rate, error) {
	if !tariff.IsElectricity() {
		return nil, fmt.Errorf("octopus: tariff %q is not an electricity tariff", tariff.Code)
	}

	// Built from parsed, character-validated components (see ParseTariffCode),
	// so nothing unvalidated reaches the path.
	endpoint := *c.baseURL
	endpoint.Path = fmt.Sprintf("%s/products/%s/electricity-tariffs/%s/%s/",
		trimSlash(c.baseURL.Path), tariff.Product, tariff.Code, kind)

	q := url.Values{}
	size := pageSize
	if limit > 0 {
		size = limit
	}
	q.Set("page_size", strconv.Itoa(size))
	if !from.IsZero() {
		q.Set("period_from", formatUTC(from))
	}
	if !to.IsZero() {
		q.Set("period_to", formatUTC(to))
	}
	endpoint.RawQuery = q.Encode()

	var out []Rate
	next := endpoint.String()
	for pages := 0; next != ""; pages++ {
		if pages >= maxPages {
			return nil, fmt.Errorf("octopus: pagination for %s exceeded %d pages; "+
				"refusing to keep following `next` (server bug or a loop)", tariff.Code, maxPages)
		}

		p, err := c.getPage(ctx, next)
		if err != nil {
			return nil, err
		}

		for i, r := range p.Results {
			rate, err := toRate(r)
			if err != nil {
				// A slot we cannot parse is NOT skipped. A quietly dropped slot
				// becomes unpriced energy weeks later, and by then nobody can
				// tell whether the price was missing upstream or we ate it.
				return nil, fmt.Errorf("octopus: %s result %d for %s: %w", kind, i, tariff.Code, err)
			}
			out = append(out, rate)
		}

		if limit > 0 || p.Next == nil || *p.Next == "" {
			break
		}
		if err := c.checkSameOrigin(*p.Next); err != nil {
			return nil, err
		}
		next = *p.Next
	}

	// The API returns newest-first. Every consumer wants oldest-first —
	// contiguity checks, gap detection, curve rendering — so normalise here
	// once rather than at each call site. Stable so the two rows of a
	// payment-method pair keep their delivered order.
	sort.SliceStable(out, func(i, j int) bool { return out[i].ValidFrom.Before(out[j].ValidFrom) })
	return out, nil
}

// checkSameOrigin refuses a `next` link that leaves the configured API origin.
//
// `next` is a URL taken from a response body — attacker-influenced input if the
// API, DNS, or anything in front of it is ever compromised or misconfigured.
// Following it to another host would turn the collector into a request
// forwarder and could carry an Authorization header somewhere it does not
// belong, so the walk is pinned to the origin we were configured with.
func (c *Client) checkSameOrigin(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("octopus: unparseable `next` link %q: %w", raw, err)
	}
	if u.Scheme != c.baseURL.Scheme || u.Host != c.baseURL.Host {
		return fmt.Errorf("octopus: refusing to follow `next` to %s://%s; "+
			"the API is configured as %s://%s", u.Scheme, u.Host, c.baseURL.Scheme, c.baseURL.Host)
	}
	return nil
}

// getPage performs one request with retries, and decodes the envelope.
func (c *Client) getPage(ctx context.Context, rawURL string) (*page, error) {
	var lastErr error

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			wait := c.backoff(attempt, lastErr)
			c.log.DebugContext(ctx, "octopus: retrying",
				"attempt", attempt, "wait", wait, "url", rawURL, "after", lastErr)
			if err := c.sleep(ctx, wait); err != nil {
				// Cancelled mid-backoff: abandon rather than firing another
				// doomed request. Wrap so errors.Is(context.Canceled) holds.
				return nil, fmt.Errorf("octopus: abandoned retry: %w", err)
			}
		}

		p, err := c.doOnce(ctx, rawURL)
		if err == nil {
			return p, nil
		}
		lastErr = err

		// Never retry our own cancellation, and never exceed the budget.
		if !IsRetryable(err) || attempt >= c.maxRetries {
			return nil, err
		}
	}
}

// doOnce performs exactly one HTTP request and decodes it.
func (c *Client) doOnce(ctx context.Context, rawURL string) (*page, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("octopus: build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Surface context errors unwrapped enough for errors.Is to see them, so
		// shutdown is distinguishable from a fetch failure.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("octopus: request to %s: %w", rawURL, ctxErr)
		}
		return nil, fmt.Errorf("octopus: request to %s: %w", rawURL, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       string(body),
			RetryAfter: c.parseRetryAfter(resp.Header.Get("Retry-After")),
			URL:        rawURL,
		}
	}

	var p page
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, fmt.Errorf("octopus: decode response from %s: %w", rawURL, &decodeError{err})
	}
	return &p, nil
}

// backoff decides how long to wait before the given attempt.
//
// A server-supplied Retry-After wins: ignoring it is how a client earns a
// longer ban. Otherwise the wait doubles from baseBackoff. Both are clamped —
// obeying an absurd Retry-After would park the collector past the publication
// window it woke up for.
func (c *Client) backoff(attempt int, lastErr error) time.Duration {
	var apiErr *APIError
	if errors.As(lastErr, &apiErr) && apiErr.RetryAfter > 0 {
		return min(apiErr.RetryAfter, maxRetryAfter)
	}
	wait := baseBackoff << (attempt - 1)
	return min(wait, maxRetryAfter)
}

// parseRetryAfter reads both legal forms of the header: delay-seconds, and an
// HTTP date. Both appear in the wild. An unparseable or past value yields 0,
// which falls back to our own backoff curve.
func (c *Client) parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if when, err := http.ParseTime(v); err == nil {
		if d := when.Sub(c.clock.Now()); d > 0 {
			return d
		}
	}
	return 0
}

// toRate converts one wire result, insisting on the fields that make it usable.
func toRate(r struct {
	ValueExcVAT   *float64 `json:"value_exc_vat"`
	ValueIncVAT   *float64 `json:"value_inc_vat"`
	ValidFrom     *string  `json:"valid_from"`
	ValidTo       *string  `json:"valid_to"`
	PaymentMethod *string  `json:"payment_method"`
}) (Rate, error) {
	if r.ValidFrom == nil {
		return Rate{}, errors.New("missing valid_from")
	}
	from, err := time.Parse(time.RFC3339, *r.ValidFrom)
	if err != nil {
		return Rate{}, fmt.Errorf("unparseable valid_from %q: %w", *r.ValidFrom, err)
	}
	// Pointers, not zero values: a price of exactly 0.00 is real, so a missing
	// field and a zero price must not collapse into the same thing.
	if r.ValueExcVAT == nil {
		return Rate{}, errors.New("missing value_exc_vat")
	}
	if r.ValueIncVAT == nil {
		return Rate{}, errors.New("missing value_inc_vat")
	}

	rate := Rate{
		ValidFrom:   from.UTC(),
		ExcVATPence: *r.ValueExcVAT,
		IncVATPence: *r.ValueIncVAT,
	}
	if r.ValidTo != nil && *r.ValidTo != "" {
		to, err := time.Parse(time.RFC3339, *r.ValidTo)
		if err != nil {
			return Rate{}, fmt.Errorf("unparseable valid_to %q: %w", *r.ValidTo, err)
		}
		utc := to.UTC()
		rate.ValidTo = &utc
	}
	if r.PaymentMethod != nil {
		rate.PaymentMethod = *r.PaymentMethod
	}
	return rate, nil
}

// formatUTC renders a time the way the API requires: UTC, seconds precision,
// trailing Z.
func formatUTC(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// trimSlash removes a trailing slash so path joining cannot produce "//".
func trimSlash(p string) string {
	for len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	return p
}
