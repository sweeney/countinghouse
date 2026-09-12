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
	GroupByDevice = "device"
	// GroupByRoom groups by floorplan room id.
	GroupByRoom  = "room"
	GroupByClass = "class"
	// GroupByFloor groups by the floor a device DECLARES
	// (config.DeviceConfig.Floor), never one read out of the room id's
	// "<floor>.<slug>" shape: the floorplan owns that fact. Energy is additive,
	// so a floor is simply the sum of its rooms — there is no argument about
	// whether the combining statistic is meaningful, which is what made the
	// climate version of this grouping contentious (issue #19).
	GroupByFloor = "floor"
	GroupByHouse = "house"

	// GroupBySelf is the single-device assembly behind GET /devices/{id}/series.
	// It is INTERNAL: the /series handler rejects it as a group_by value, and a
	// response built with it reports group_by "device" (see resolveGroupBy),
	// because it is the device grouping — just not a FLEET one.
	//
	// The distinction is the whole-house meter. The fleet groupings exclude it
	// (see IsWholeHouseTotal): it measures the same electricity as the plugs, so
	// one series per device plus the meter double-counts the house. A request for
	// ONE device cannot double-count anything, so the exclusion has no work to do
	// and its only effect was to empty the response — 200 with "series": null for
	// the meter, which reads as "no readings" (issue #21). Self assembles exactly
	// the requested device, whatever its class.
	GroupBySelf = "self"
)

// house series keys.
const (
	// houseCoverageKey is the key the PLACE groupings (room, floor) give devices
	// whose readings describe the whole property rather than the place they sit
	// in.
	//
	// They need a key of their own. Dropping them breaks the partition that
	// withUnmonitoredCatchAll documents — houseParts counts every metered device in
	// `monitored` regardless of place, so a dropped device inflates `monitored` while
	// appearing in no series, and the parts stop summing to the meter. Attributing
	// them to the room they sit in instead would be the conflation this migration
	// removes, relocated from `location` to `room`.
	houseCoverageKey = "house"

	houseMonitoredKey   = "monitored"
	houseMeterKey       = "meter"
	houseUnmonitoredKey = "unmonitored"
)

// UnmonitoredID is the reserved device id / series key for the synthetic
// "unmonitored" (rest-of-home) quantity: the whole-house meter minus the sum of
// monitored devices. It is NOT a real device — no statehouse_devices entry may
// claim it (the handlers shadow it, see Q3 in
// docs/countinghouse-unmonitored-consumption.md). UnmonitoredClass is its class
// tag, distinguishing the synthetic series/catalogue entry from a real device.
const (
	UnmonitoredID    = "unmonitored"
	UnmonitoredClass = "unmonitored"
)

// EnergyMeterClass is the device class of the whole-house electricity meter. It
// is metered via the counter path like a plug, but it is the AUTHORITATIVE
// whole-house total, not one of the monitored devices — so it is EXCLUDED from
// device/room/class groupings and surfaced separately only under group_by=house.
//
// It is the single source of truth for the meter class name, shared by the
// energy package (grouping + routing) and the httpapi /bill handler (meter
// detection), so the two can never drift. "energy_meter" is the canonical class
// value the real statehouse_devices namespace emits (AGENT_BRIEF §3); the §1
// device table's "electricity_meter" is descriptive prose, not the class tag.
const EnergyMeterClass = "energy_meter"

// Series is one line/bar in a SeriesResponse: a labelled, room/class-tagged
// set of per-bucket arrays (all of length len(buckets)) plus window totals.
type Series struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Room  string `json:"room,omitempty"`
	Class string `json:"class,omitempty"`

	KWh  []float64 `json:"kwh"`
	Cost []float64 `json:"cost"`
	AvgW []float64 `json:"avg_w"`

	TotalKWh  float64 `json:"total_kwh"`
	TotalCost float64 `json:"total_cost"`

	// UnpricedKWh is energy in buckets where NO rate was known — a half-hourly
	// slot the archive does not hold. It is reported rather than folded into Cost
	// at zero, because charging nothing for real consumption is the quiet way a
	// bill comes out wrong. Omitted when zero, which is the normal case.
	UnpricedKWh float64 `json:"unpriced_kwh,omitempty"`
}

// SeriesResponse is the columnar ("wide") time-series payload (PLAN §A): a single
// shared time axis (Buckets) plus per-series value arrays that all align to it.
// This maps directly onto web charting libraries (one array per dataset) and is
// the default. For the row-oriented ("tidy"/long) alternative, see Rows().
type SeriesResponse struct {
	Window   string `json:"window"`
	From     string `json:"from"`
	To       string `json:"to"`
	Interval string `json:"interval"`
	GroupBy  string `json:"group_by"`
	Shape    string `json:"shape"` // "columns"

	// House decomposition confidence signals, populated only for group_by=house
	// when a meter is configured (omitted otherwise — they are meaningless for
	// device/room/class groupings). See HouseStats.
	HouseStats

	Buckets []time.Time `json:"buckets"`
	Series  []Series    `json:"series"`

	// Drift is operator-facing data-quality info (C3): the negative-residual
	// (meter < monitored) drift detected while deriving unmonitored. It is NEVER
	// serialised (json:"-") — the doc is explicit that this signal does not belong
	// in a browser; the httpapi layer turns it into a /metrics counter + WARN log.
	Drift DriftStats `json:"-"`
}

// DriftStats summarises negative-residual drift (C3): buckets where the raw
// meter − monitored fell below one counter quantum (−0.1 kWh) — beyond what the
// 0.1 kWh counter quantisation alone explains, so a real signal that a monitored
// device is over-counting, the meter is mis-scaled, or clocks are skewed. Such
// buckets are clamped to 0 in the unmonitored series, hiding the problem from the
// response, so it is surfaced out-of-band instead.
type DriftStats struct {
	ClampedBuckets   int       // count of buckets with residual < −driftQuantumKWh
	WorstResidualKWh float64   // most-negative residual seen (≤ 0; 0 when none)
	WorstAt          time.Time // bucket start of the worst residual
}

// HasDrift reports whether any bucket breached the drift threshold.
func (d DriftStats) HasDrift() bool { return d.ClampedBuckets > 0 }

// driftQuantumKWh is the per-bucket negative-residual threshold for C3: one
// device-counter quantum. Residuals between 0 and −0.1 kWh are routine
// quantisation/sampling noise (clamped silently); only a residual MORE negative
// than this is flagged as drift.
const driftQuantumKWh = 0.1

// HouseStats carries the group_by=house confidence signals so a consumer can tell
// "this much of the home is genuinely unmonitored" from "monitored is
// under-counted because a sensor dropped" without a second service call (C12/C13).
//
// "group_by=house" means the REPORTED grouping, not the one the values were
// computed from. /devices/unmonitored/series derives its series from the house
// decomposition and then reports itself as device-grouped, so it carries none of
// these — see AsSingleDevice, which clears them in the same breath as it rewrites
// GroupBy precisely so the two cannot disagree (issue #23).
//
// Coverage is monitored ÷ meter energy for the window — a pointer so a genuine
// zero (meter present, nothing monitored) is distinct from "not applicable" (nil,
// no meter / not a house grouping), which is then omitted.
//
// StaleMonitoredCount/IDs flag monitored devices that produced NO power telemetry
// in the window. A live plug/UPS reports power_w continuously even at standby, so
// zero rows means dropped/offline — and unlike energy, power presence is not
// confounded by the 0.1 kWh counter quantisation. A stale device's load silently
// shifts into unmonitored (= meter − monitored), inflating it; this is the
// monitored-side analogue of the negative-residual drift signal.
type HouseStats struct {
	Coverage            *float64 `json:"coverage,omitempty"`
	StaleMonitoredCount *int     `json:"stale_monitored_count,omitempty"`
	StaleMonitoredIDs   []string `json:"stale_monitored_ids,omitempty"`
}

// BucketStarts returns the canonical local-timezone bucket-start axis for win
// at iv: the ascending list of bucket starts from win.Start up to (but not
// including) win.Stop. This axis is the single source of truth every series
// aligns to; Influx results are demuxed onto it and gaps are zero-filled.
//
// For calendar intervals (1d) the axis steps by CALENDAR day in loc using
// time.Date(year, month, day+1, ...), so a London day that is 23h (spring
// forward) or 25h (autumn back) is still a single bucket starting at local
// midnight — DST-correct.
//
// For fixed (sub-day) intervals the axis is SNAPPED DOWN to the interval grid
// anchored at the local midnight of the start date, then stepped by
// iv.Duration. This matches Influx's aggregateWindow(every:, location:), whose
// windows are aligned to the location's grid (local midnight), NOT to
// range(start:). For today/week/month the window start is already local
// midnight, which is on every sub-day grid, so this is a no-op; it only matters
// for window=custom whose `from` is off the boundary (e.g. 14:23 with 1h snaps
// to 14:00). Snapping makes every Influx row's _start stamp exact-match a
// bucket, so the first partial window is no longer dropped and later buckets are
// not shifted.
//
// The first bucket's TIMESTAMP therefore precedes `from`, but its VALUE does not
// cover the pre-`from` slice (issues #27, #29). Both value paths hold the head to
// win.Start, symmetrically with the tail clip at win.Stop, though by different
// means: bucketHours clips the power path's bucket length, while the counter
// path's query is anchored at win.Start by increase() so its bucket 0 cannot
// describe anything earlier. Bucket 0 is a partial bucket labelled by the grid
// boundary it starts on, exactly as the last bucket is a partial bucket ending at
// win.Stop.
//
// Flux parity (anchoring): Influx's location-aware aggregateWindow anchors
// windows to local midnight in the configured location and handles DST by
// stretching/shrinking the single window that straddles the transition, NOT by
// offsetting the whole grid by the zone offset (InfluxData, "Time Zones in
// Flux": location-set results "always have time starting and stopping at
// midnight in the specified time zone"). So a 6h grid is 00/06/12/18 LOCAL in
// both GMT and BST (not 01/07/13/19), which is exactly what anchoring at local
// midnight here produces — see TestBucketStartsCustomNonAlignedCoarse, which
// exercises 6h on a BST date. (1h and finer always coincide regardless, because
// London's offset is a whole number of hours.) Caveat: for a sub-day interval
// over a custom window that CROSSES a DST transition, this fixed-duration
// stepping drifts an hour off Flux's stretched grid after the change; that is a
// pre-existing narrow edge (the 1d path has its own calendar branch) and worth
// a one-off confirmation against the live instance.
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

	// Snap start down to the interval grid anchored at the start date's local
	// midnight (Flux aligns sub-day windows to the location's grid, not to the
	// query range start).
	anchor := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
	off := start.Sub(anchor) % iv.Duration
	first := start.Add(-off)
	for cur := first; cur.Before(stop); cur = cur.Add(iv.Duration) {
		out = append(out, cur)
	}
	return out
}

// bucketHours returns, for each bucket i, the wall-clock length in hours of the
// part of that bucket that lies INSIDE the window — clipped at BOTH ends:
//
//   - bucket 0 starts at win.Start, not at its grid boundary. The axis snaps
//     that boundary DOWN below win.Start on purpose (issue #1, see BucketStarts),
//     so the first bucket's grid interval can begin before the window does; the
//     part before win.Start is not the caller's window and must not be billed.
//   - the final bucket ends at win.Stop, which for a period-to-date window is
//     "now" rather than a boundary.
//
// Either edge can therefore be partial, and a window contained entirely within
// one grid interval is partial at both. Interior buckets are their full
// wall-clock length, which is not the same as iv.Duration: a calendar-day bucket
// spanning a DST change is 23h or 25h.
//
// Two consumers need it, both of them DIVIDING energy by a duration rather than
// multiplying a rate by one (UPS energy stopped being mean × hours in issue #32
// — Influx clips those buckets itself now, since the query range is the exact
// window):
//
//   - deriveUPSPower and deriveUnmonitored, which energy-derive avg_w as
//     kwh × 1000 / hours. The head clip is load-bearing there: bucket 0's energy
//     covers only the minute inside the window, so dividing it by the full grid
//     interval would report a UPS holding a steady kilowatt as idling at 33 W.
//   - anything reasoning about how much of a bucket the window actually covers.
func bucketHours(buckets []time.Time, start, stop time.Time) []float64 {
	hrs := make([]float64, len(buckets))
	for i := range buckets {
		from := buckets[i]
		if from.Before(start) {
			from = start
		}
		to := stop
		if i+1 < len(buckets) {
			to = buckets[i+1]
		}
		if h := to.Sub(from).Hours(); h > 0 {
			hrs[i] = h
		}
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
//     meter. key=id, label=DisplayName, room/class carried through.
//   - self (internal, single-device endpoint): the device series WITHOUT the
//     whole-house exclusion — a one-device request is not a fleet and cannot
//     double-count. Identical to device for every other class.
//   - room: device kWh/cost/avgW summed per room (meter excluded).
//   - floor: the same, over the floor each device DECLARES. Energy is additive,
//     so a floor is simply the sum of its rooms.
//   - class: summed per Class (meter excluded).
//   - house: THREE series — "monitored" = sum of ALL non-meter devices;
//     "unmonitored" = clamp(meter − monitored) per bucket; "meter" = the energy
//     meter's own series (unmonitored/meter present only when a meter exists).
//
// The device/room/class unmonitored catch-all (R2) is applied by BuildSeries
// AFTER this assembly, not here.
//
// Cost is derived per bucket as kWh × UnitRate × VAT multiplier. For real-device
// series avg_w SUMS member telemetry means (power is additive); the synthetic
// unmonitored series has no telemetry and energy-derives avg_w from bucketHours
// (see deriveUnmonitored). Totals are the summed rounded per-bucket values.
// Rounding: kWh 3dp, cost 4dp, W 1dp. bucketHours is the per-bucket wall-clock
// length (only the house path consumes it; other groupings may pass nil).
//
// groupLabels maps a group key to the floorplan's display name for it, for the
// grouped place modes only (room, floor). A key with no entry, or an empty
// entry, keeps the key itself as the label: countinghouse relays the floorplan's
// names and falls back to the id rather than deriving a label from it. The KEY
// is always the id — only Label varies — so a caller matching on identity is
// unaffected by whether a name happens to be published. Per-device series take
// their label from DisplayName and ignore it entirely, and so does class
// grouping: a class is not a place and the floorplan does not name one.
func AssembleSeries(
	buckets []time.Time,
	bucketHours []float64,
	devices map[string]config.DeviceConfig,
	energyByDevice map[string][]float64,
	powerByDevice map[string][]float64,
	pricer Pricer,
	groupBy string,
	groupLabels map[string]string,
) []Series {
	get := paddedGetter(len(buckets))

	switch groupBy {
	case GroupByRoom, GroupByFloor, GroupByClass:
		return assembleGrouped(buckets, devices, energyByDevice, powerByDevice, pricer, get, groupBy, groupLabels)
	case GroupByHouse:
		return assembleHouse(buckets, bucketHours, devices, energyByDevice, powerByDevice, pricer, get)
	case GroupBySelf:
		return assembleByDevice(buckets, devices, energyByDevice, powerByDevice, pricer, get, true)
	case GroupByDevice, "":
		return assembleByDevice(buckets, devices, energyByDevice, powerByDevice, pricer, get, false)
	default:
		return assembleByDevice(buckets, devices, energyByDevice, powerByDevice, pricer, get, false)
	}
}

// getter pulls a per-bucket slice for a device id, padded to the axis length.
type getter func(m map[string][]float64, id string) []float64

// paddedGetter returns a getter that yields a device's per-bucket slice truncated
// or zero-padded to exactly n buckets, so a missing/short device reads as all-zero
// for the axis.
func paddedGetter(n int) getter {
	return func(m map[string][]float64, id string) []float64 {
		v := m[id]
		if len(v) >= n {
			return v[:n]
		}
		out := make([]float64, n)
		copy(out, v)
		return out
	}
}

// assembleByDevice yields one series per metered device. When
// includeWholeHouseTotal is false (the fleet view, group_by=device) the
// whole-house meter is left out so the series do not double-count the house;
// when true (GroupBySelf, the single-device endpoint) it is assembled like any
// other device, since one device cannot double-count.
func assembleByDevice(
	buckets []time.Time,
	devices map[string]config.DeviceConfig,
	energyByDevice, powerByDevice map[string][]float64,
	pricer Pricer,
	get getter,
	includeWholeHouseTotal bool,
) []Series {
	ids := sortedDeviceIDs(devices)
	var out []Series
	for _, id := range ids {
		d := devices[id]
		if !isMetered(d.Class) {
			continue
		}
		if IsWholeHouseTotal(d) && !includeWholeHouseTotal {
			continue
		}
		label := d.DisplayName
		if label == "" {
			label = id
		}
		s := buildSeries(id, label, d.Place(), d.Class, buckets,
			[][]float64{get(energyByDevice, id)},
			[][]float64{get(powerByDevice, id)},
			pricer)
		out = append(out, s)
	}
	return out
}

// GroupKeyFor returns the function mapping a device to its series key under
// groupBy, or nil when groupBy gives every device its own series (device/self)
// or does not group devices at all (house).
//
// One definition of "which devices share a series", used by the assembly step,
// by the rooms=/floors= filters, and by the /rooms and /floors catalogs. Two
// implementations of that question would drift, and what drifts is which
// consumption lands in which series — a silently wrong bill rather than a loud
// one. It is also what guarantees a catalog never advertises a group the
// matching filter rejects.
func GroupKeyFor(groupBy string) func(config.DeviceConfig) string {
	switch groupBy {
	case GroupByRoom:
		// Coverage is consulted before place so that a legacy `location: house` and
		// a migrated `room` + `covers: house` group identically: republishing the
		// namespace must not move energy between series.
		return func(d config.DeviceConfig) string {
			if d.CoversWholeSite() {
				return houseCoverageKey
			}
			return d.Place()
		}
	case GroupByFloor:
		// Same rule, same reason: a device whose readings describe the whole
		// property belongs to no storey, and attributing an immersion heater
		// wired house-wide to the floor its box hangs on would be exactly the
		// conflation the floorplan taxonomy removes — relocated from `location`
		// to `floor`.
		return func(d config.DeviceConfig) string {
			if d.CoversWholeSite() {
				return houseCoverageKey
			}
			return d.Floor
		}
	case GroupByClass:
		return func(d config.DeviceConfig) string { return d.Class }
	default:
		return nil
	}
}

// CountByGroupKey counts the devices in each group under groupBy: the metered,
// non-meter devices that a fleet grouping actually emits series for, keyed by
// GroupKeyFor.
//
// It backs the device_count on /rooms and /floors, and defines WHICH groups
// those catalogs list — so they list exactly what the matching filter accepts.
// Two keys are excluded, both because they are not places:
//
//   - the empty key (UNKNOWN membership: no room or no declared floor), which
//     rooms=/floors= can never match;
//   - houseCoverageKey, a coverage SCOPE and a reserved series key. Listing it
//     would advertise "house" as a room id, which the taxonomy forbids.
//
// A grouping that gives every device its own series (device) or none (house)
// has no groups to count and yields an empty map.
func CountByGroupKey(devices map[string]config.DeviceConfig, groupBy string) map[string]int {
	keyOf := GroupKeyFor(groupBy)
	counts := map[string]int{}
	if keyOf == nil {
		return counts
	}
	for _, d := range devices {
		if !isMetered(d.Class) || IsWholeHouseTotal(d) {
			continue
		}
		k := keyOf(d)
		if k == "" || k == houseCoverageKey {
			continue
		}
		counts[k]++
	}
	return counts
}

// assembleGrouped yields one series per distinct non-empty key over metered,
// non-meter devices, summing member energy and power bucket-wise. The key comes
// from GroupKeyFor, so a grouping, its filter and its catalog can never disagree
// about which devices share a series.
func assembleGrouped(
	buckets []time.Time,
	devices map[string]config.DeviceConfig,
	energyByDevice, powerByDevice map[string][]float64,
	pricer Pricer,
	get getter,
	groupBy string,
	groupLabels map[string]string,
) []Series {
	keyOf := GroupKeyFor(groupBy)
	members := map[string][]string{}
	for id, d := range devices {
		if !isMetered(d.Class) || IsWholeHouseTotal(d) {
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
		// houseCoverageKey is a coverage SCOPE, not a place, so it takes neither a
		// room nor a floorplan name — it is the one key /rooms and /floors never
		// list, and both the spec and the README tell clients to join a room
		// series' `room` to that catalog. Reporting room: "house" would hand them
		// the single value guaranteed to miss. The unmonitored catch-all already
		// reads this way for the same reason: it belongs to no place either.
		isPlace := (groupBy == GroupByRoom || groupBy == GroupByFloor) && k != houseCoverageKey

		// A series that IS a room reports that room, so a client can join it to
		// the /rooms catalog. A floor or class series belongs to no single room,
		// so `room` stays empty rather than carrying a floor id in a field named
		// room.
		room := ""
		if isPlace && groupBy == GroupByRoom {
			room = k
		}
		// The floorplan's name when it publishes one, the id otherwise. Never a
		// label derived from the id: that transform is the client-side guesswork
		// this exists to remove, and moving it here would not make it less of a
		// guess. A legend rendering `label || key` therefore shows "Room A" once
		// the floorplan names it and "floor1.room-a" until then.
		//
		// Only the PLACE groupings take names. A class is not a place (the
		// floorplan does not name one, and a key colliding with a class name must
		// not relabel it), and neither is the house scope.
		label := k
		if isPlace {
			if name := groupLabels[k]; name != "" {
				label = name
			}
		}
		out = append(out, buildSeries(k, label, room, "", buckets, es, ps, pricer))
	}
	return out
}

// assembleHouse yields the house series in order: "monitored" (sum of all
// non-meter devices), "unmonitored" (the meter minus monitored — see
// deriveUnmonitored), and "meter" (the energy meter's own series). monitored is
// absent if there are no monitored devices; unmonitored and meter are absent if
// no energy meter is configured (C6: with no meter, "unmonitored" is undefined —
// we omit it rather than report monitored as if it were the whole home). The
// ordering puts unmonitored between its two parents so a client stacking every
// series except "meter" reconstructs the whole home (R1.4).
func assembleHouse(
	buckets []time.Time,
	bucketHours []float64,
	devices map[string]config.DeviceConfig,
	energyByDevice, powerByDevice map[string][]float64,
	pricer Pricer,
	get getter,
) []Series {
	monitored, meter := houseParts(buckets, devices, energyByDevice, powerByDevice, pricer, get)

	var out []Series
	if monitored != nil {
		out = append(out, *monitored)
	}
	if meter != nil {
		out = append(out, deriveUnmonitored(buckets, bucketHours, monitored, *meter, pricer, false))
		out = append(out, *meter)
	}
	return out
}

// houseParts builds the two parents of the house decomposition: "monitored" (sum
// of all metered non-meter devices) and "meter" (the energy meter's own series).
// Either is nil when it has no members (no monitored devices / no meter). Shared
// by the house grouping and the device/room/class unmonitored catch-all (R2),
// so "unmonitored" means the identical quantity in both.
func houseParts(
	buckets []time.Time,
	devices map[string]config.DeviceConfig,
	energyByDevice, powerByDevice map[string][]float64,
	pricer Pricer,
	get getter,
) (monitored, meter *Series) {
	var monEnergy, monPower [][]float64
	meterID, _ := MeterID(devices)
	for _, id := range sortedDeviceIDs(devices) {
		d := devices[id]
		if IsWholeHouseTotal(d) {
			continue
		}
		if !isMetered(d.Class) {
			continue
		}
		monEnergy = append(monEnergy, get(energyByDevice, id))
		monPower = append(monPower, get(powerByDevice, id))
	}

	if len(monEnergy) > 0 {
		s := buildSeries(houseMonitoredKey, houseMonitoredKey, "", "", buckets, monEnergy, monPower, pricer)
		monitored = &s
	}
	if meterID != "" {
		d := devices[meterID]
		s := buildSeries(houseMeterKey, houseMeterKey, d.Place(), d.Class, buckets,
			[][]float64{get(energyByDevice, meterID)},
			[][]float64{get(powerByDevice, meterID)},
			pricer)
		meter = &s
	}
	return monitored, meter
}

// withUnmonitoredCatchAll appends the single "unmonitored" catch-all series to a
// device/room/class grouping (R2): clamp(meter − monitored) per bucket — the
// SAME quantity as the house "unmonitored" series, since the grouped series
// partition exactly the monitored devices. It is never subdivided: exactly one
// series regardless of grouping (N1), so a stacked chart of the grouping plus
// this catch-all sums to the whole-house meter (R2.4). A no-op when no meter is
// configured (C6) — there is then nothing to attribute.
func withUnmonitoredCatchAll(
	grouped []Series,
	buckets []time.Time,
	bucketHours []float64,
	devices map[string]config.DeviceConfig,
	energyByDevice, powerByDevice map[string][]float64,
	pricer Pricer,
) []Series {
	get := paddedGetter(len(buckets))
	monitored, meter := houseParts(buckets, devices, energyByDevice, powerByDevice, pricer, get)
	if meter == nil {
		return grouped
	}
	return append(grouped, deriveUnmonitored(buckets, bucketHours, monitored, *meter, pricer, false))
}

// deriveUnmonitored builds the synthetic "unmonitored" (rest-of-home) series:
// per bucket, clamp(meter − monitored). It is the energy the whole-house meter
// saw that no monitored device accounts for. A slightly-negative residual
// (device counters tick in 0.1 kWh quanta vs the meter's finer resolution, plus
// sampling skew) clamps to 0 PER BUCKET before totals are summed (C1/C2), so the
// total is Σ(clamped buckets), not a total-level subtraction. Cost is the tariff
// applied to the clamped energy (C9 — never meter_cost − monitored_cost).
//
// avg_w is ENERGY-DERIVED (C8): kwh × 1000 / bucket_hours[i]. The unmonitored
// series has NO power telemetry of its own, so its mean power must come from the
// residual energy and the bucket duration. It must NOT be meter.AvgW −
// monitored.AvgW: summed device power means are biased high vs the meter's own
// mean (the counter-vs-∫power bias), so that difference is routinely negative and
// would clamp to 0 — reporting "0 W" for a series consuming real energy (the
// avg_w=0 bug, docs/bug-unmonitored-avg-w.md) — and would disagree with the
// published kwh anyway, mirroring the C9 reasoning for cost. Using the actual
// bucket_hours keeps a partial first OR last bucket correctly scaled: BOTH edges
// are clipped to the window (issue #27), so bucket 0's residual energy is divided
// by the hours it actually covers rather than by its full grid interval.
// monitored may be nil (no monitored devices ⇒ the whole meter is unmonitored).
//
// Because meter and monitored carry already-rounded per-bucket values, the
// visible invariant monitored.kwh + unmonitored.kwh == meter.kwh holds exactly at
// display precision whenever the bucket is not clamped (R1.2).
//
// When unclamped is true (Q4 diagnostic mode) the per-bucket residual is left as
// the raw meter − monitored, NEGATIVES PRESERVED, for data-quality investigation;
// avg_w then follows the signed energy and can go negative too.
func deriveUnmonitored(buckets []time.Time, bucketHours []float64, monitored *Series, meter Series, pricer Pricer, unclamped bool) Series {
	n := len(buckets)
	s := Series{
		Key:   houseUnmonitoredKey,
		Label: houseUnmonitoredKey,
		Class: UnmonitoredClass,
		KWh:   make([]float64, n),
		Cost:  make([]float64, n),
		AvgW:  make([]float64, n),
	}

	// Per-bucket energy at full precision, for the same reason buildSeries keeps it:
	// the wire gets a rounded copy, the totals are computed from these.
	raw := make([]float64, n)
	for i := 0; i < n; i++ {
		var monKWh float64
		if monitored != nil {
			monKWh = monitored.KWh[i]
		}
		kwh := meter.KWh[i] - monKWh
		if !unclamped && kwh < 0 {
			kwh = 0
		}
		raw[i] = kwh

		var w float64
		if i < len(bucketHours) && bucketHours[i] > 0 {
			w = kwh * 1000.0 / bucketHours[i]
		}
		s.KWh[i] = round.To(kwh, round.KWhDP)
		// Priced at THIS bucket's rate, not at one rate for the window.
		if rate, known := pricer.RateAt(buckets[i]); known {
			s.Cost[i] = round.To(kwh*rate, round.MoneyDP)
		}
		s.AvgW[i] = round.To(w, round.WDP)
	}

	// Totals through the SAME function buildSeries uses, accumulating raw and
	// rounding once. This is the second producer of a Series, and when only the
	// first was fixed it kept both bugs: a month's rest-of-home cost understated by
	// a third, and missing prices rounded away to an UnpricedKWh of exactly zero —
	// which reads as "nothing was missing", the one answer worse than a wrong total.
	// It reaches the wire on /series?group_by=house, include_unmonitored=true, and
	// /devices/unmonitored/series.
	cost, unpriced := CostBuckets(buckets, raw, pricer)
	for _, kwh := range raw {
		s.TotalKWh += kwh
	}
	s.TotalKWh = round.To(s.TotalKWh, round.KWhDP)
	s.TotalCost = round.To(cost, round.MoneyDP)
	s.UnpricedKWh = round.To(unpriced, round.KWhDP)
	return s
}

// computeHouseStats derives the group_by=house confidence signals (C12 coverage,
// C13 staleness) from the assembled series and the per-device power presence. It
// returns the zero HouseStats (all nil ⇒ all omitted) when no meter series is
// present: without the whole-house total the decomposition — and any coverage or
// staleness reading over it — is undefined (C6).
func computeHouseStats(series []Series, devices map[string]config.DeviceConfig, powerByDevice map[string][]float64) HouseStats {
	var monTotal, meterTotal float64
	var haveMeter bool
	for _, s := range series {
		switch s.Key {
		case houseMonitoredKey:
			monTotal = s.TotalKWh
		case houseMeterKey:
			meterTotal = s.TotalKWh
			haveMeter = true
		}
	}
	if !haveMeter {
		return HouseStats{}
	}

	cov := 0.0
	if meterTotal != 0 {
		cov = round.To(monTotal/meterTotal, round.CovDP)
	}

	var stale []string
	for _, id := range sortedDeviceIDs(devices) {
		d := devices[id]
		if IsWholeHouseTotal(d) || !isMetered(d.Class) {
			continue
		}
		if _, ok := powerByDevice[id]; !ok {
			stale = append(stale, id)
		}
	}
	cnt := len(stale)

	return HouseStats{Coverage: &cov, StaleMonitoredCount: &cnt, StaleMonitoredIDs: stale}
}

// computeDrift scans the pre-clamp per-bucket residual (meter − monitored) for
// C3 drift: buckets more negative than one counter quantum. monitored may be nil
// (treated as zero); meter must be non-nil (no meter ⇒ no decomposition ⇒ no
// drift). Operates on the already-rounded part totals, which is ample resolution
// for a 0.1 kWh threshold.
func computeDrift(buckets []time.Time, monitored, meter *Series) DriftStats {
	var d DriftStats
	if meter == nil {
		return d
	}
	for i := range buckets {
		var mon float64
		if monitored != nil {
			mon = monitored.KWh[i]
		}
		resid := meter.KWh[i] - mon
		if resid < -driftQuantumKWh {
			d.ClampedBuckets++
			if resid < d.WorstResidualKWh {
				d.WorstResidualKWh = resid
				d.WorstAt = buckets[i]
			}
		}
	}
	return d
}

// rebuildUnmonitoredUnclamped replaces the clamped "unmonitored" series in place
// with its UNCLAMPED form (raw meter − monitored, negatives preserved) for the Q4
// diagnostic mode. No-op when there is no unmonitored series or no meter. The
// negative buckets deliberately break the monitored+unmonitored==meter
// presentation — that is the point of the diagnostic.
func rebuildUnmonitoredUnclamped(series []Series, buckets []time.Time, bucketHours []float64, devices map[string]config.DeviceConfig, energyByDevice, powerByDevice map[string][]float64, pricer Pricer) []Series {
	idx := -1
	for i, s := range series {
		if s.Key == houseUnmonitoredKey {
			idx = i
			break
		}
	}
	if idx < 0 {
		return series
	}
	monitored, meter := houseParts(buckets, devices, energyByDevice, powerByDevice, pricer, paddedGetter(len(buckets)))
	if meter == nil {
		return series
	}
	series[idx] = deriveUnmonitored(buckets, bucketHours, monitored, *meter, pricer, true)
	return series
}

// AsSingleDevice reshapes a GROUPED build into the single-device response form:
// only the named series, group_by=device, and none of the house-only confidence
// signals. It is what lets /devices/unmonitored/series satisfy R3.1 — the same
// response schema as a real device's, so a client plots it with no branching —
// even though unmonitored can only be DERIVED from the house grouping.
//
// All three parts are one decision, which is why they are one call (issue #23).
// The third is the one easy to miss: HouseStats is EMBEDDED in SeriesResponse,
// and BuildSeries populates it under exactly the predicate that reports GroupBy
// as "house". Rewriting GroupBy alone therefore left a body announcing itself as
// device-grouped while still carrying coverage and staleness — two facts about
// the same response, disagreeing. Clearing the stats here means they cannot.
//
// Dropping them loses nothing: they describe the whole-house decomposition, not
// this series, and GET /series?group_by=house still carries them beside the same
// unmonitored values for a consumer who wants both.
//
// Drift is deliberately kept. It is never serialised (json:"-") and is the
// operator-facing C3 signal the handler turns into a metric — a property of the
// computation that produced these numbers, not of the shape they are sent in.
func (r SeriesResponse) AsSingleDevice(key string) SeriesResponse {
	r.Series = OnlySeries(r.Series, key)
	r.GroupBy = GroupByDevice
	r.HouseStats = HouseStats{}
	return r
}

// OnlySeries returns the sub-slice of series whose Key == key (preserving order),
// or an empty slice if none match. Used to extract a single named series (e.g.
// "unmonitored") from a grouped response for the single-device endpoint shape.
func OnlySeries(series []Series, key string) []Series {
	out := make([]Series, 0, 1)
	for _, s := range series {
		if s.Key == key {
			out = append(out, s)
		}
	}
	return out
}

// MeterID returns the id of the whole-house energy meter in the inventory (the
// device IsWholeHouseTotal identifies — that predicate's doc carries the
// definition, so this one cannot drift from it) and whether one exists. It is the
// single place "is there a meter, and which device is it?" is answered, shared by
// the house grouping, the bill reconciliation, the device catalogue, and the
// synthetic unmonitored series. The first id in sorted order wins (there is only
// ever one meter; sorting just makes the choice deterministic).
func MeterID(devices map[string]config.DeviceConfig) (string, bool) {
	for _, id := range sortedDeviceIDs(devices) {
		if IsWholeHouseTotal(devices[id]) {
			return id, true
		}
	}
	return "", false
}

// buildSeries sums member energy/power slices bucket-wise, derives cost, rounds
// every value, and computes totals. All member slices are assumed to be length
// len(buckets).
func buildSeries(key, label, place, class string, buckets []time.Time, energy, power [][]float64, pricer Pricer) Series {
	n := len(buckets)
	s := Series{
		Key:   key,
		Label: label,
		Room:  place,
		Class: class,
		KWh:   make([]float64, n),
		Cost:  make([]float64, n),
		AvgW:  make([]float64, n),
	}

	// Per-bucket energy at full precision. The wire gets a rounded copy; the totals
	// are computed from THESE values, not from the rounded ones — see below.
	raw := make([]float64, n)
	for i := 0; i < n; i++ {
		var kwh, w float64
		for _, e := range energy {
			kwh += e[i]
		}
		for _, p := range power {
			w += p[i]
		}
		raw[i] = kwh

		var cost float64
		if rate, known := pricer.RateAt(buckets[i]); known {
			cost = kwh * rate
		}

		s.KWh[i] = round.To(kwh, round.KWhDP)
		s.Cost[i] = round.To(cost, round.MoneyDP)
		s.AvgW[i] = round.To(w, round.WDP)
	}

	// Totals accumulate RAW and round exactly once.
	//
	// Summing the rounded per-bucket values instead was harmless while the totals
	// only fed a chart, and stopped being harmless when /bill started reading them:
	// a month at half-hourly resolution is 1488 buckets, and 1488 roundings of up to
	// half a hundredth of a penny can drift a bill by several pence in one
	// direction. Pence on a bill is the kind of wrong that is small and completely
	// indefensible. The same argument applies to the energy total, and with more
	// force to UnpricedKWh, where rounding each part away produced a total of zero
	// — which reads as "nothing was missing".
	//
	// CostBuckets is the SINGLE definition of pricing a bucket axis, shared with the
	// cost path, so /series and /bill cannot disagree about what a window cost.
	cost, unpriced := CostBuckets(buckets, raw, pricer)
	for _, kwh := range raw {
		s.TotalKWh += kwh
	}
	s.TotalKWh = round.To(s.TotalKWh, round.KWhDP)
	s.TotalCost = round.To(cost, round.MoneyDP)
	s.UnpricedKWh = round.To(unpriced, round.KWhDP)
	return s
}

// isMetered reports whether a class participates in energy series at all (any
// class PathForClass routes — plug classes, energy meter, ups_sensor). It is the
// SAME predicate the /devices/{id}/series handler gates on, so a class the
// handler admits is a class assembly can build — the two must never diverge, or
// the endpoint answers 200 with no series (issue #21).
func isMetered(class string) bool {
	_, ok := PathForClass(class)
	return ok
}

// IsWholeHouseTotal reports whether a device's readings ARE the authoritative
// whole-house total rather than one contributor to it. Such a device is excluded
// from the fleet groupings (device/room/class) and from `monitored`, and surfaced
// on its own as the house "meter" series; including it alongside the plugs it
// already measures would double-count the house.
//
// It is the single place that distinction is decided — shared by assembleByDevice,
// assembleGrouped, houseParts, computeHouseStats, MeterID and the /bill handler —
// so those can never drift apart. Drift between two such filters is what produced
// issue #21: assembly excluded the meter from a grouping the handler had already
// decided to serve, and the endpoint answered 200 with no series at all.
//
// It keys on CLASS, deliberately, not on `covers: house`. The two say different
// things: `covers` answers "which place do these readings describe?" — it exists
// so a whole-property device is not attributed to the room it sits in (it groups
// under houseCoverageKey instead) — while class answers "is this the meter?".
// A whole-property device that is not the meter, an immersion heater wired
// house-wide say, is still a real load the meter sees, so it belongs in
// `monitored` and in a grouped series; that partition is pinned by
// TestGroupedSeriesPartitionTheMonitoredDevices. Keying the exclusion on
// `covers` would drop it from both and inflate `unmonitored` by its consumption.
// It would also make a namespace that omits `covers` on the meter double-count
// the entire house, a far worse failure than the one being fixed.
func IsWholeHouseTotal(d config.DeviceConfig) bool {
	return d.Class == EnergyMeterClass
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

// deriveUPSPower fills avg_w for the UPS set from the energy query 2 just
// produced: kWh × 1000 / bucket_hours, which is the bucket's TIME-WEIGHTED mean
// power (issue #32).
//
// A UPS is the one metered class with no counter, so its energy is an estimate
// of an integral rather than a reading. Taking avg_w from that same integral is
// the C8 argument applied where it belongs a second time: the published avg_w
// and the published kwh are then two views of ONE number and cannot contradict
// each other. The alternative — a separate mean(power_w) query — reintroduces
// exactly the estimator gap this change closes, one field lower down: a bucket
// whose samples cluster in its first minutes would report a healthy sample mean
// beside an energy figure that disagrees with it.
//
// For the steady loads a UPS carries, sampled regularly, the time-weighted mean
// and the sample mean coincide to well inside avg_w's 1dp — so this is not a
// visible change in the ordinary case, only in the uneven one it exists for.
//
// Devices with NO energy rows are skipped deliberately, leaving them absent from
// powerByDevice: a UPS that reported nothing all window must stay visible to C13
// staleness rather than being handed a manufactured row of zeroes.
func deriveUPSPower(upsIDs []string, bucketHours []float64, energyByDevice, powerByDevice map[string][]float64) {
	for _, id := range upsIDs {
		kwh := energyByDevice[id]
		if kwh == nil {
			continue // silent all window — leave it stale, do not invent 0 W
		}
		w := make([]float64, len(kwh))
		for i := range kwh {
			if i < len(bucketHours) && bucketHours[i] > 0 {
				w[i] = kwh[i] * 1000.0 / bucketHours[i]
			}
		}
		powerByDevice[id] = w
	}
}

// BuildSeries is the orchestrator: it runs ~3 Influx queries (counter energy for
// the counter set incl. meter; per-bucket power integral for the ups set;
// mean-power for the counter set), demuxes the bucketed rows onto the canonical axis
// and calls AssembleSeries. The query count is three regardless of window
// alignment or device count.
//
// The query count is independent of device count: each builder fans out across
// a device set via contains(set: [...]). bucket is the Influx bucket name; win
// the resolved window; iv the resolved interval; groupBy the grouping mode;
// devices the inventory; tariff the pricing; groupLabels the floorplan display
// names for grouped series keys; loc the timezone.
func BuildSeries(
	ctx context.Context,
	q influx.Querier,
	bucket string,
	win Window,
	iv Interval,
	groupBy string,
	includeUnmonitored bool,
	unclamped bool,
	devices map[string]config.DeviceConfig,
	pricer Pricer,
	groupLabels map[string]string,
	loc *time.Location,
) (SeriesResponse, error) {
	if loc == nil {
		loc = time.UTC
	}
	buckets := BucketStarts(win, iv, loc)
	idx := bucketIndex(buckets)
	hrs := bucketHours(buckets, win.Start, win.Stop)
	tz := loc.String()

	// Partition the metered inventory.
	var counterIDs, upsIDs []string
	for _, id := range sortedDeviceIDs(devices) {
		path, ok := PathForClass(devices[id].Class)
		if !ok {
			continue
		}
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
		// The counter series returns each bucket's CLOSING RUNNING TOTAL measured
		// from the window start, not a per-bucket delta: the differencing happens
		// here so a reading gap is explicit rather than smeared (issue #29).
		demuxCounterTotals(rows, idx, energyByDevice, len(buckets))
	}

	// Query 2: UPS energy, by integrating power_w over each bucket — the same
	// reduction /devices/{id}/energy applies to the whole window (issue #32).
	// The rows are already kWh, so the fold is the identity.
	if len(upsIDs) > 0 {
		flux := influx.BuildPowerIntegralSeriesFlux(bucket, upsIDs, win.Start, win.Stop, iv.Token, tz)
		rows, err := q.Query(ctx, flux)
		if err != nil {
			return SeriesResponse{}, err
		}
		demux(rows, idx, energyByDevice, len(buckets), func(kwh float64, _ int) float64 { return kwh })
		deriveUPSPower(upsIDs, hrs, energyByDevice, powerByDevice)
	}

	// Query 3: mean power for the COUNTER devices (the avg_w series). A UPS is
	// absent by design: its avg_w is energy-derived from query 2 instead, so it
	// cannot disagree with its own kwh — see deriveUPSPower.
	if len(counterIDs) > 0 {
		flux := influx.BuildPowerMeanSeriesFlux(bucket, counterIDs, win.Start, win.Stop, iv.Token, tz)
		rows, err := q.Query(ctx, flux)
		if err != nil {
			return SeriesResponse{}, err
		}
		demux(rows, idx, powerByDevice, len(buckets), func(v float64, _ int) float64 { return v })
	}

	series := AssembleSeries(buckets, hrs, devices, energyByDevice, powerByDevice, pricer, groupBy, groupLabels)

	// R2: opt the single unmonitored catch-all into a device/room/class
	// grouping so the parts sum to the whole house. group_by=house already carries
	// it, so the flag is a no-op there (and on any future grouping it is ignored
	// rather than double-adding).
	if includeUnmonitored && resolveGroupBy(groupBy) != GroupByHouse {
		series = withUnmonitoredCatchAll(series, buckets, hrs, devices, energyByDevice, powerByDevice, pricer)
	}

	// C3 drift detection runs whenever the decomposition is produced (house, or a
	// catch-all was added) — before any unclamping, on the true residual.
	var drift DriftStats
	if resolveGroupBy(groupBy) == GroupByHouse || includeUnmonitored {
		monitored, meter := houseParts(buckets, devices, energyByDevice, powerByDevice, pricer, paddedGetter(len(buckets)))
		drift = computeDrift(buckets, monitored, meter)
	}

	// Q4: replace the clamped unmonitored series with its raw (signed) form.
	if unclamped {
		series = rebuildUnmonitoredUnclamped(series, buckets, hrs, devices, energyByDevice, powerByDevice, pricer)
	}

	// An assembly that produced nothing marshals as "series": null, and a consumer
	// reading a full bucket axis beside a null cannot tell "nothing matched this
	// grouping" from "this device reported no data" — the ambiguity issue #21 was
	// filed over. An empty grouping is an empty list; absence is not a value.
	if series == nil {
		series = []Series{}
	}

	resp := SeriesResponse{
		Window:   win.Label,
		From:     win.Start.In(loc).Format(time.RFC3339),
		To:       win.Stop.In(loc).Format(time.RFC3339),
		Interval: iv.Token,
		GroupBy:  resolveGroupBy(groupBy),
		Shape:    ShapeColumns,
		Buckets:  buckets,
		Series:   series,
	}
	// Coverage + staleness are house-only confidence signals (C12/C13).
	if resolveGroupBy(groupBy) == GroupByHouse {
		resp.HouseStats = computeHouseStats(series, devices, powerByDevice)
	}
	resp.Drift = drift
	return resp, nil
}

// resolveGroupBy normalises the reported group_by (empty or the internal
// GroupBySelf → device).
func resolveGroupBy(groupBy string) string {
	// GroupBySelf is an internal assembly mode, not a wire value: a single-device
	// response is a device-grouped response carrying one device, and clients
	// switch on group_by to pick a renderer.
	if groupBy == "" || groupBy == GroupBySelf {
		return GroupByDevice
	}
	return groupBy
}

// bucketIndex maps each canonical bucket-start (truncated to the bucket key) to
// its position. Influx returns each bucket's right-edge stop time from
// aggregateWindow, but we key on the LEFT edge; demux resolves a row's time to
// the bucket whose [start,next) it falls in via the index of exact starts, and
// falls back to the containing bucket for non-exact stamps — which is not a rare
// path: aggregateWindow truncates its first window to the range, so an off-grid
// `from` yields a first row stamped at `from` itself rather than at the grid
// boundary the axis uses.
func bucketIndex(buckets []time.Time) map[int64]int {
	m := make(map[int64]int, len(buckets))
	for i, b := range buckets {
		m[b.UnixNano()] = i
	}
	return m
}

// demux folds bucketed rows onto the canonical axis. Each row carries a
// DeviceID, a Time and a Value; conv maps (value, bucketIndex) → the stored
// quantity; both of its callers pass the identity, since the UPS energy query now
// returns kWh directly rather than a mean to scale. Rows whose time falls
// outside the axis are dropped. A row landing on a bucket SUMS into that bucket
// (aggregateWindow yields one row per bucket per device, so this is normally an
// assignment; summing is just safe).
//
// A NULL row is dropped, not folded: createEmpty: true fills unreported buckets
// with nulls that decode to 0.0, and folding those would publish "0 W" for a
// device that said nothing (issue #32). Dropping them leaves the bucket at the
// slice's zero either way, but it also leaves a device that reported NOTHING
// with no entry in dst at all — which is how C13 staleness tells a silent device
// from a genuinely idle one.
//
// This serves the two POWER queries only. The counter series carries running
// totals rather than per-bucket quantities and is folded by demuxCounterTotals.
func demux(rows []influx.Row, idx map[int64]int, dst map[string][]float64, n int, conv func(float64, int) float64) {
	if n == 0 {
		return
	}
	starts := sortedBucketStarts(idx)

	for _, r := range rows {
		if r.Null {
			continue // absent bucket, not a reading of zero
		}
		i := resolveBucket(r.Time, idx, starts)
		if i < 0 {
			continue // outside the window
		}
		arr := dst[r.DeviceID]
		if arr == nil {
			arr = make([]float64, n)
			dst[r.DeviceID] = arr
		}
		arr[i] += conv(r.Value, i)
	}
}

// sortedBucketStarts returns the bucket-start keys ascending, for
// resolveBucket's containment fallback.
func sortedBucketStarts(idx map[int64]int) []int64 {
	starts := make([]int64, 0, len(idx))
	for k := range idx {
		starts = append(starts, k)
	}
	sort.Slice(starts, func(a, b int) bool { return starts[a] < starts[b] })
	return starts
}

// demuxCounterTotals folds the counter series' per-bucket CLOSING RUNNING TOTALS
// onto the canonical axis and differences them here, in Go, rather than in Flux.
//
// Each row is "energy this device had accumulated by the end of this bucket,
// measured from the window start" (see influx.BuildCounterSeriesFlux). A bucket
// is worth the rise since the last bucket that CLOSED — not since the previous
// bucket index — so the running total is carried across buckets the device did
// not report in. Those buckets are worth 0, and the next real reading picks up
// everything that accrued meanwhile.
//
// That carry is the whole point (issue #29). Flux's difference() could not do it:
// it needs a prior window to subtract against, which the old design bought by
// padding the range before the window — anchoring the series at a reading taken
// BEFORE `from`, and, when the pad was empty, spending a real in-window bucket as
// the seed instead. Anchoring at `from` makes the series' total identical to
// BuildCounterFlux's reduction over the same range, so /series and
// /devices/{id}/energy agree by definition.
//
// A device with no rows at all gets no entry, which AssembleSeries reads as
// all-zero — correct for a device that reported nothing inside the window.
//
// Null rows are dropped before any of that, for a sharper reason than on the
// power path — see the guard below.
//
// Note the deliberate asymmetry with DeviceWindowKWh, which SUMS the rows it gets
// ("disjoint accumulations, so they add"). Both are right for the one table per
// device that regroupByDevice guarantees. They differ in how they would fail if
// that guarantee lapsed — as it did when the location→site migration fragmented
// every series mid-window (issue #17): summing running totals would multiply the
// answer, so last-wins is the safer reading here, while summing is the safer one
// there. Neither should be "harmonised" into the other without restoring the
// guarantee first.
func demuxCounterTotals(rows []influx.Row, idx map[int64]int, dst map[string][]float64, n int) {
	if n == 0 {
		return
	}
	starts := sortedBucketStarts(idx)

	// The closing total per (device, bucket). aggregateWindow yields one row per
	// bucket per device, but resolve defensively: the LAST row to land in a
	// bucket is the one that closes it.
	type bucketClose struct {
		at  time.Time
		val float64
	}
	closes := make(map[string]map[int]bucketClose)
	for _, r := range rows {
		if r.Null {
			// A null is an absent bucket, and absent is what the carry-forward
			// below already handles correctly. Folding it would be worse here
			// than on the power path: a null reads as a running total of ZERO,
			// so the anchor resets and the next real bucket re-bills everything
			// accrued since the window opened — issue #29's double-count, in the
			// path #29 rewrote. BuildCounterSeriesFlux says createEmpty: false
			// so none should arrive; this does not depend on that staying true.
			continue
		}
		i := resolveBucket(r.Time, idx, starts)
		if i < 0 {
			continue // outside the window
		}
		per := closes[r.DeviceID]
		if per == nil {
			per = make(map[int]bucketClose)
			closes[r.DeviceID] = per
		}
		if prev, seen := per[i]; !seen || !r.Time.Before(prev.at) {
			per[i] = bucketClose{at: r.Time, val: r.Value}
		}
	}

	for id, per := range closes {
		arr := dst[id]
		if arr == nil {
			arr = make([]float64, n)
			dst[id] = arr
		}
		var running float64
		for i := 0; i < n; i++ {
			c, closed := per[i]
			if !closed {
				continue // no reading: carry `running` forward, this bucket is 0
			}
			// increase() is monotonic, so a fall should be impossible; clamp
			// rather than publish negative energy, and still advance the anchor.
			if d := c.val - running; d > 0 {
				arr[i] += d
			}
			running = c.val
		}
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
