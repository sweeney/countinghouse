package influx

import (
	"context"
	"errors"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
)

// Config carries the runtime connection parameters for the query client.
// Bucket is not used directly by Client (the Flux builders embed it) but is
// kept here so callers have a single connection struct, mirroring statehouse.
type Config struct {
	URL    string
	Org    string
	Bucket string
	Token  string
}

// errDisabled is returned by a Client whose connection params were incomplete.
// Like statehouse's disabled-writer guard, we degrade gracefully rather than
// panicking on a missing token.
var errDisabled = errors.New("influx: client disabled (missing url/org/token)")

// Client is the production Querier, backed by influxdb-client-go's QueryAPI.
type Client struct {
	client influxdb2.Client
	qapi   api.QueryAPI
}

// New constructs a Client. If url/org/token are empty the Client is left in a
// disabled state: Query returns errDisabled and Ping returns false, so the
// rest of the service can still start (e.g. in dev or tests).
func New(cfg Config) *Client {
	if cfg.URL == "" || cfg.Org == "" || cfg.Token == "" {
		return &Client{}
	}
	c := influxdb2.NewClient(cfg.URL, cfg.Token)
	return &Client{
		client: c,
		qapi:   c.QueryAPI(cfg.Org),
	}
}

// Query runs flux against the QueryAPI and decodes the result into Rows. Tags
// are read defensively: device_id/class/location/_field may be absent on a
// given record (e.g. after aggregation drops a column), so missing keys become
// empty strings rather than errors.
func (c *Client) Query(ctx context.Context, flux string) ([]Row, error) {
	if c == nil || c.qapi == nil {
		return nil, errDisabled
	}
	res, err := c.qapi.Query(ctx, flux)
	if err != nil {
		return nil, err
	}
	defer res.Close()

	var rows []Row
	for res.Next() {
		rec := res.Record()
		row := Row{
			DeviceID: stringByKey(rec.ValueByKey("device_id")),
			Class:    stringByKey(rec.ValueByKey("class")),
			Location: stringByKey(rec.ValueByKey("location")),
			Field:    rec.Field(),
			Time:     rec.Time(),
		}
		switch v := rec.Value().(type) {
		case float64:
			row.Value = v
		case string:
			row.Text = v
		}
		rows = append(rows, row)
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

// Ping reports backend reachability. A disabled client is never reachable.
func (c *Client) Ping(ctx context.Context) bool {
	if c == nil || c.client == nil {
		return false
	}
	ok, err := c.client.Ping(ctx)
	return ok && err == nil
}

// Close releases the underlying HTTP client. Safe on a disabled Client.
func (c *Client) Close() {
	if c == nil || c.client == nil {
		return
	}
	c.client.Close()
}

// stringByKey coerces a Flux column value to a string, defaulting to "" for
// absent (nil) or non-string values.
func stringByKey(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
