package energy

import (
	"context"
	"sort"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/influx"
	"github.com/sweeney/countinghouse/internal/round"
)

// group_by modes for BuildSeries / AssembleSeries.
const (
	GroupByDevice   = "device"
	GroupByLocation = "location"
	GroupByClass    = "class"
	GroupByHouse    = "house"
)

// house series keys.
const (
	houseMonitoredKey = "monitored"
	houseMeterKey     = "meter"
)

// EnergyMeterClass is the device class of the whole-house electricity meter. It
// is metered via the counter path like a plug, but it is the AUTHORITATIVE
// whole-house total, not one of the monitored devices — so it is EXCLUDED from
// device/location/class groupings and surfaced separately only under group_by=house.
//
// It is the single source of truth for the meter class name, shared by the
// energy package (grouping + routing) and the httpapi /bill handler (meter
// detection), so the two can never drift. "energy_meter" is the canonical class
// value the real statehouse_devices namespace emits (AGENT_BRIEF §3); the §1
// device table's "electricity_meter" is descriptive prose, not the class tag.
const EnergyMeterClass = "energy_meter"

// Series is one line/bar in a SeriesResponse: a labelled, location/class-tagged
// set of per-bucket arrays (all of length len(buckets)) plus window totals.
type Series struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Location string `json:"location,omitempty"`
	Class    string `json:"class,omitempty"`

	KWh  []float64 `json:"kwh"`
	Cost []float64 `json:"cost"`
	AvgW []float64 `json:"avg_w"`

	TotalKWh  float64 `json:"total_kwh"`
	TotalCost float64 `json:"total_cost"`
}

// SeriesResponse is the columnar ("wide") time-series payload (PLAN §A): a single
// shared time axis (Buckets) plus per-series value arrays that all align to it.
// This maps directly onto web charting libraries (one array per dataset) and is
// the default. For the row-oriented ("tidy"/long) alternative, see Rows().
type SeriesResponse struct {
	Window   string      `json:"window"`
	From     string      `json:"from"`
	To       string      `json:"to"`
	Interval string      `json:"interval"`
	GroupBy  string      `json:"group_by"`
	Shape    string      `json:"shape"` // "columns"
	Buckets  []time.Time `json:"buckets"`
	Series   []Series    `json:"series"`
}

// BucketStarts returns the canonical local-timezone bucket-start axis for win
// at iv: the ascending list of bucket starts from win.Start up to (but not
// including) win.Stop. This axis is the single source of truth every series
// aligns to; Influx results are demuxed onto it and gaps are zero-filled.
//
// For calendar intervals (1d) the axis steps by CALENDAR day in loc using
// time.Date(year, month, day+1, ...), so a London day that is 23h (spring
// forward) or 25h (autumn back) is still a single bucket starting at local
// midnight — DST-correct. For fixed (sub-day) intervals the axis steps by
// iv.Duration from the (local) window start.
//
// The window start is normalised into loc first so boundaries are local.
func BucketStarts(win Window, iv Interval, loc *time.Location) []time.Time {
	if loc == nil {
		loc = time.UTC
	}
	start := win.Start.In(loc)
	stop := win.Stop

	var out []time.Time
	if iv.Calendar {
		// Step by calendar day, anchored at the local midnight of start's date.
		cur := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
		for cur.Before(stop) {
			out = append(out, cur)
			cur = time.Date(cur.Year(), cur.Month(), cur.Day()+1, 0, 0, 0, 0, loc)
		}
		return out
	}

	for cur := start; cur.Before(stop); cur = cur.Add(iv.Duration) {
		out = append(out, cur)
	}
	return out
}

// bucketHours returns, for each bucket i, the real wall-clock length of that
// bucket in hours: buckets[i+1]-buckets[i] for interior buckets, and
// win.Stop-buckets[last] for the final (possibly partial) bucket. This is what
// the UPS energy conversion (mean watts × hours / 1000) must use, because a
// calendar-day bucket spanning a DST change is 23h or 25h, and a period-to-date
// final bucket ends at "now", not on a boundary.
func bucketHours(buckets []time.Time, stop time.Time) []float64 {
	hrs := make([]float64, len(buckets))
	for i := range buckets {
		var end time.Time
		if i+1 < len(buckets) {
			end = buckets[i+1]
		} else {
			end = stop
		}
		h := end.Sub(buckets[i]).Hours()
		if h < 0 {
			h = 0
		}
		hrs[i] = h
	}
	return hrs
}

// AssembleSeries is the PURE assembly step: given the canonical bucket axis, the
// device inventory, per-device per-bucket energy (kWh, already aligned to
// buckets) and mean power (W), the tariff and the group_by mode, it produces the
// grouped, zero-filled, rounded []Series.
//
// Inputs energyByDevice and powerByDevice are maps id→[]float64 aligned to
// buckets (len == len(buckets)); a missing device or a nil/short slice is
// treated as all-zero for that device. Every emitted series has arrays of
// length len(buckets).
//
// Grouping rules (PLAN §A):
//   - device (default): one series per metered device, EXCLUDING the energy
//     meter. key=id, label=DisplayName, location/class carried through.
//   - location: device kWh/cost/avgW summed per Location (meter excluded).
//   - class: summed per Class (meter excluded).
//   - house: TWO series — "monitored" = sum of ALL non-meter devices; "meter" =
//     the energy meter's own series (if present).
//
// Cost is derived per bucket as kWh × UnitRate × VAT multiplier. Aggregated
// avg_w SUMS member means (power is additive). Totals are the summed rounded
// per-bucket values. Rounding: kWh 3dp, cost 4dp, W 1dp.
func AssembleSeries(
	buckets []time.Time,
	devices map[string]config.DeviceConfig,
	energyByDevice map[string][]float64,
	powerByDevice map[string][]float64,
	tariff config.Tariff,
	groupBy string,
) []Series {
	n := len(buckets)
	get := func(m map[string][]float64, id string) []float64 {
		v := m[id]
		if len(v) >= n {
			return v[:n]
		}
		// pad short/missing to n with zeros.
		out := make([]float64, n)
		copy(out, v)
		return out
	}

	switch groupBy {
	case GroupByLocation:
		return assembleGrouped(buckets, devices, energyByDevice, powerByDevice, tariff, get, func(d config.DeviceConfig) string { return d.Location })
	case GroupByClass:
		return assembleGrouped(buckets, devices, energyByDevice, powerByDevice, tariff, get, func(d config.DeviceConfig) string { return d.Class })
	case GroupByHouse:
		return assembleHouse(buckets, devices, energyByDevice, powerByDevice, tariff, get)
	case GroupByDevice, "":
		return assembleByDevice(buckets, devices, energyByDevice, powerByDevice, tariff, get)
	default:
		return assembleByDevice(buckets, devices, energyByDevice, powerByDevice, tariff, get)
	}
}

// getter pulls a per-bucket slice for a device id, padded to the axis length.
type getter func(m map[string][]float64, id string) []float64

// assembleByDevice yields one series per metered non-meter device.
func assembleByDevice(
	buckets []time.Time,
	devices map[string]config.DeviceConfig,
	energyByDevice, powerByDevice map[string][]float64,
	tariff config.Tariff,
	get getter,
) []Series {
	ids := sortedDeviceIDs(devices)
	var out []Series
	for _, id := range ids {
		d := devices[id]
		if !isMetered(d.Class) || d.Class == EnergyMeterClass {
			continue
		}
		label := d.DisplayName
		if label == "" {
			label = id
		}
		s := buildSeries(id, label, d.Location, d.Class, buckets,
			[][]float64{get(energyByDevice, id)},
			[][]float64{get(powerByDevice, id)},
			tariff)
		out = append(out, s)
	}
	return out
}

// assembleGrouped yields one series per distinct non-empty key over metered,
// non-meter devices, summing member energy and power bucket-wise.
func assembleGrouped(
	buckets []time.Time,
	devices map[string]config.DeviceConfig,
	energyByDevice, powerByDevice map[string][]float64,
	tariff config.Tariff,
	get getter,
	keyOf func(config.DeviceConfig) string,
) []Series {
	members := map[string][]string{}
	for id, d := range devices {
		if !isMetered(d.Class) || d.Class == EnergyMeterClass {
			continue
		}
		k := keyOf(d)
		if k == "" {
			continue
		}
		members[k] = append(members[k], id)
	}

	keys := make([]string, 0, len(members))
	for k := range members {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []Series
	for _, k := range keys {
		ids := members[k]
		sort.Strings(ids)
		var es, ps [][]float64
		for _, id := range ids {
			es = append(es, get(energyByDevice, id))
			ps = append(ps, get(powerByDevice, id))
		}
		out = append(out, buildSeries(k, k, "", "", buckets, es, ps, tariff))
	}
	return out
}

// assembleHouse yields the dual house series: "monitored" (sum of all non-meter
// devices) and "meter" (the energy meter's own series), in that order. Either
// may be absent if it has no members.
func assembleHouse(
	buckets []time.Time,
	devices map[string]config.DeviceConfig,
	energyByDevice, powerByDevice map[string][]float64,
	tariff config.Tariff,
	get getter,
) []Series {
	var monEnergy, monPower [][]float64
	var meterID string
	for _, id := range sortedDeviceIDs(devices) {
		d := devices[id]
		if d.Class == EnergyMeterClass {
			meterID = id
			continue
		}
		if !isMetered(d.Class) {
			continue
		}
		monEnergy = append(monEnergy, get(energyByDevice, id))
		monPower = append(monPower, get(powerByDevice, id))
	}

	var out []Series
	if len(monEnergy) > 0 {
		out = append(out, buildSeries(houseMonitoredKey, houseMonitoredKey, "", "", buckets, monEnergy, monPower, tariff))
	}
	if meterID != "" {
		d := devices[meterID]
		out = append(out, buildSeries(houseMeterKey, houseMeterKey, d.Location, d.Class, buckets,
			[][]float64{get(energyByDevice, meterID)},
			[][]float64{get(powerByDevice, meterID)},
			tariff))
	}
	return out
}

// buildSeries sums member energy/power slices bucket-wise, derives cost, rounds
// every value, and computes totals. All member slices are assumed to be length
// len(buckets).
func buildSeries(key, label, location, class string, buckets []time.Time, energy, power [][]float64, tariff config.Tariff) Series {
	n := len(buckets)
	s := Series{
		Key:      key,
		Label:    label,
		Location: location,
		Class:    class,
		KWh:      make([]float64, n),
		Cost:     make([]float64, n),
		AvgW:     make([]float64, n),
	}
	mult := tariff.Multiplier()
	for i := 0; i < n; i++ {
		var kwh, w float64
		for _, e := range energy {
			kwh += e[i]
		}
		for _, p := range power {
			w += p[i]
		}
		cost := kwh * tariff.UnitRate * mult

		s.KWh[i] = round.To(kwh, round.KWhDP)
		s.Cost[i] = round.To(cost, round.MoneyDP)
		s.AvgW[i] = round.To(w, round.WDP)

		s.TotalKWh += s.KWh[i]
		s.TotalCost += s.Cost[i]
	}
	s.TotalKWh = round.To(s.TotalKWh, round.KWhDP)
	s.TotalCost = round.To(s.TotalCost, round.MoneyDP)
	return s
}

// isMetered reports whether a class participates in energy series at all (any
// class PathForClass routes — plug classes, energy meter, ups_sensor).
func isMetered(class string) bool {
	_, ok := PathForClass(class)
	return ok
}

// sortedDeviceIDs returns the inventory ids sorted for deterministic output.
func sortedDeviceIDs(devices map[string]config.DeviceConfig) []string {
	ids := make([]string, 0, len(devices))
	for id := range devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// BuildSeries is the orchestrator: it runs ~3 Influx queries (counter energy for
// the counter set incl. meter; UPS mean-power→energy for the ups set; mean-power
// for ALL metered devices), demuxes the bucketed rows onto the canonical axis
// (dropping pad buckets), and calls AssembleSeries.
//
// The query count is independent of device count: each builder fans out across
// a device set via contains(set: [...]). bucket is the Influx bucket name; win
// the resolved window; iv the resolved interval; groupBy the grouping mode;
// devices the inventory; tariff the pricing; loc the timezone.
func BuildSeries(
	ctx context.Context,
	q influx.Querier,
	bucket string,
	win Window,
	iv Interval,
	groupBy string,
	devices map[string]config.DeviceConfig,
	tariff config.Tariff,
	loc *time.Location,
) (SeriesResponse, error) {
	if loc == nil {
		loc = time.UTC
	}
	buckets := BucketStarts(win, iv, loc)
	idx := bucketIndex(buckets)
	hrs := bucketHours(buckets, win.Stop)
	tz := loc.String()

	// Partition the metered inventory.
	var counterIDs, upsIDs, allMeteredIDs []string
	for _, id := range sortedDeviceIDs(devices) {
		path, ok := PathForClass(devices[id].Class)
		if !ok {
			continue
		}
		allMeteredIDs = append(allMeteredIDs, id)
		switch path {
		case PathCounter:
			counterIDs = append(counterIDs, id)
		case PathIntegral:
			upsIDs = append(upsIDs, id)
		}
	}

	energyByDevice := map[string][]float64{}
	powerByDevice := map[string][]float64{}

	// Query 1: counter energy (per-bucket deltas) for the counter set + meter.
	if len(counterIDs) > 0 {
		flux := influx.BuildCounterSeriesFlux(bucket, counterIDs, win.Start, win.Stop, iv.Token, tz)
		rows, err := q.Query(ctx, flux)
		if err != nil {
			return SeriesResponse{}, err
		}
		demux(rows, idx, energyByDevice, len(buckets), func(v float64, _ int) float64 { return v })
	}

	// Query 2: UPS mean-power → energy (mean W × bucket-hours / 1000).
	if len(upsIDs) > 0 {
		flux := influx.BuildPowerMeanSeriesFlux(bucket, upsIDs, win.Start, win.Stop, iv.Token, tz)
		rows, err := q.Query(ctx, flux)
		if err != nil {
			return SeriesResponse{}, err
		}
		demux(rows, idx, energyByDevice, len(buckets), func(meanW float64, i int) float64 {
			return meanW * hrs[i] / 1000.0
		})
	}

	// Query 3: mean power for ALL metered devices (the avg_w series).
	if len(allMeteredIDs) > 0 {
		flux := influx.BuildPowerMeanSeriesFlux(bucket, allMeteredIDs, win.Start, win.Stop, iv.Token, tz)
		rows, err := q.Query(ctx, flux)
		if err != nil {
			return SeriesResponse{}, err
		}
		demux(rows, idx, powerByDevice, len(buckets), func(v float64, _ int) float64 { return v })
	}

	series := AssembleSeries(buckets, devices, energyByDevice, powerByDevice, tariff, groupBy)

	return SeriesResponse{
		Window:   win.Label,
		From:     win.Start.In(loc).Format(time.RFC3339),
		To:       win.Stop.In(loc).Format(time.RFC3339),
		Interval: iv.Token,
		GroupBy:  resolveGroupBy(groupBy),
		Shape:    ShapeColumns,
		Buckets:  buckets,
		Series:   series,
	}, nil
}

// resolveGroupBy normalises the reported group_by (empty → device).
func resolveGroupBy(groupBy string) string {
	if groupBy == "" {
		return GroupByDevice
	}
	return groupBy
}

// bucketIndex maps each canonical bucket-start (truncated to the bucket key) to
// its position. Influx returns each bucket's right-edge stop time from
// aggregateWindow, but we key on the LEFT edge; demux resolves a row's time to
// the bucket whose [start,next) it falls in via the index of exact starts, and
// falls back to the containing bucket for non-exact stamps.
func bucketIndex(buckets []time.Time) map[int64]int {
	m := make(map[int64]int, len(buckets))
	for i, b := range buckets {
		m[b.UnixNano()] = i
	}
	return m
}

// demux folds bucketed rows onto the canonical axis. Each row carries a
// DeviceID, a Time and a Value; conv maps (value, bucketIndex) → the stored
// quantity (identity for energy/power, mean→energy for UPS). Rows whose time is
// before the first bucket (pad buckets) or after the last are dropped. A row
// landing on a bucket SUMS into that bucket (aggregateWindow yields one row per
// bucket per device, so this is normally an assignment; summing is just safe).
func demux(rows []influx.Row, idx map[int64]int, dst map[string][]float64, n int, conv func(float64, int) float64) {
	if n == 0 {
		return
	}
	// Build a sorted bucket-start list once for containment fallback.
	starts := make([]int64, 0, len(idx))
	for k := range idx {
		starts = append(starts, k)
	}
	sort.Slice(starts, func(a, b int) bool { return starts[a] < starts[b] })

	for _, r := range rows {
		i := resolveBucket(r.Time, idx, starts)
		if i < 0 {
			continue // pad bucket / outside window
		}
		arr := dst[r.DeviceID]
		if arr == nil {
			arr = make([]float64, n)
			dst[r.DeviceID] = arr
		}
		arr[i] += conv(r.Value, i)
	}
}

// resolveBucket maps a row time to a canonical bucket index. It first tries an
// exact left-edge match (the common case when the row stamp equals a bucket
// start). Otherwise it locates the bucket whose start is the greatest start
// ≤ time, i.e. the containing bucket; a time before the first start (a pad
// bucket) returns -1.
//
// Influx's aggregateWindow stamps each output at the bucket's stop (right edge)
// by default; the series builders' results therefore arrive at right edges. To
// be robust to either convention we treat an exact match as the left edge and
// otherwise snap a right-edge / interior stamp back to its containing bucket by
// taking the greatest start strictly less than the stamp.
func resolveBucket(t time.Time, idx map[int64]int, starts []int64) int {
	key := t.UnixNano()
	if i, ok := idx[key]; ok {
		return i
	}
	// greatest start < key (right-edge stamp belongs to the bucket it closes).
	lo, hi := 0, len(starts)
	for lo < hi {
		mid := (lo + hi) / 2
		if starts[mid] < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	// lo is the first index with start >= key; the containing bucket is lo-1.
	if lo == 0 {
		return -1
	}
	return idx[starts[lo-1]]
}
