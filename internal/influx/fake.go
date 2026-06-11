package influx

import (
	"context"
	"strings"
	"sync"
)

// FakeQuerier is a programmable, recording Querier double for handler/logic
// tests, mirroring the spirit of statehouse's FakeWriteAPI. It is safe for
// concurrent use.
//
// Resolution order for Query:
//  1. If Err is set, it is returned.
//  2. If QueryFunc is set, it is called (full control of rows/error).
//  3. Otherwise the flux is matched against Responses: the value of the first
//     key found as a substring of the flux is returned. A convenient key is a
//     device_id, since the builders embed `r.device_id == "<id>"`.
//  4. No match → nil rows, nil error (empty result).
type FakeQuerier struct {
	mu sync.Mutex

	// Responses maps a substring of the flux to the rows to return.
	Responses map[string][]Row
	// QueryFunc, if set, overrides Responses for full control.
	QueryFunc func(flux string) ([]Row, error)
	// Err, if set, is returned from every Query call.
	Err error
	// PingOK is what Ping returns.
	PingOK bool

	// Queries records every flux string passed to Query, in order.
	Queries []string
}

// Query records the flux and returns programmed rows per the resolution order
// documented on FakeQuerier.
func (f *FakeQuerier) Query(_ context.Context, flux string) ([]Row, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.Queries = append(f.Queries, flux)

	if f.Err != nil {
		return nil, f.Err
	}
	if f.QueryFunc != nil {
		return f.QueryFunc(flux)
	}
	for key, rows := range f.Responses {
		if strings.Contains(flux, key) {
			return rows, nil
		}
	}
	return nil, nil
}

// Ping returns the configured PingOK.
func (f *FakeQuerier) Ping(_ context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.PingOK
}

// LastQuery returns the most recently recorded flux, or "" if none.
func (f *FakeQuerier) LastQuery() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Queries) == 0 {
		return ""
	}
	return f.Queries[len(f.Queries)-1]
}
