package httpapi

import "time"

// PriceHealth is one tariff's price-archive health, as reported on /healthz and
// /metrics.
//
// This is an httpapi-local type rather than the collector's own Status, for the
// same reason ConfigProvider takes an interface: the HTTP layer should not import
// the collector, and a test should be able to produce any health state without
// constructing one. main.go adapts between them.
type PriceHealth struct {
	TariffCode string `json:"tariff_code"`

	// KnownTo is the end of the newest slot held. CompleteTo is the end of the
	// newest FULLY POPULATED local day.
	//
	// Both are reported because they answer different questions, and a monitor
	// reading only the first would be misled. A publication can advance KnownTo
	// across a whole day while leaving that day short of slots — observed in
	// practice — so "prices are arriving" and "we can bill this far" are not the
	// same statement.
	KnownTo    time.Time `json:"known_to,omitempty"`
	CompleteTo time.Time `json:"complete_to,omitempty"`

	// LastAttempt moves on every sync; LastSuccess only on one that worked. Equal
	// values mean healthy; a LastSuccess lagging behind means we are failing now.
	LastAttempt time.Time `json:"last_attempt,omitempty"`
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastError   string    `json:"last_error,omitempty"`

	// Cumulative counters since start, for /metrics.
	Syncs    int `json:"syncs"`
	Failures int `json:"failures"`
	Inserted int `json:"inserted"`
	Restated int `json:"restated"`
	Rejected int `json:"rejected"`
	Warnings int `json:"warnings"`
}

// PricesProvider supplies the price-archive health behind /healthz and /metrics.
//
// Server.Prices may be nil, and normally IS on a deployment whose tariff
// agreements are all flat-rate: it runs no collector, so there is no archive to
// report on. The block is then omitted entirely rather than rendered empty, since
// a zeroed block would read as a broken archive rather than as no archive.
type PricesProvider interface {
	PriceHealth() []PriceHealth
}

// priceVerdict reports whether the archive is in good order, and why not when it
// is not.
//
// Two conditions degrade, and neither can flap:
//
//   - a fetch error. Same treatment as a failing config namespace: the archive
//     still holds everything it held before, so past windows still price, but
//     somebody should know we are not keeping up.
//   - CompleteTo at or behind now, which means TODAY cannot be priced in full.
//     In the healthy state CompleteTo is the end of today or tomorrow and so
//     always ahead of now, which is what keeps this from oscillating. An archive
//     that has never been filled has a zero CompleteTo and is caught by the same
//     test.
//
// Neither is "unavailable": the data routes still answer, and historical windows
// are unaffected. Influx being unreachable is the only hard failure.
func priceVerdict(health []PriceHealth, now time.Time) (degraded bool, reason string) {
	for _, h := range health {
		if h.LastError != "" {
			return true, "price fetch failing for " + h.TariffCode
		}
		if h.CompleteTo.IsZero() {
			return true, "no complete day of prices held for " + h.TariffCode
		}
		if !h.CompleteTo.After(now) {
			return true, "prices for today are incomplete for " + h.TariffCode
		}
	}
	return false, ""
}
