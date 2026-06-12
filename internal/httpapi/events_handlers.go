package httpapi

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sweeney/countinghouse/internal/config"
	"github.com/sweeney/countinghouse/internal/energy"
	"github.com/sweeney/countinghouse/internal/events"
)

// buildTimeline runs events.BuildTimeline for a device set, bumping the query
// counters (and latency) for /metrics. BuildTimeline issues two Influx queries
// (activity + carry-in); we count the whole build as one logical query so the
// per-query latency average stays comparable across endpoints.
func (s *Server) buildTimeline(r *http.Request, ids []string, win energy.Window) (map[string]events.Timeline, error) {
	start := time.Now()
	out, err := events.BuildTimeline(r.Context(), s.Influx, s.Bucket, ids, win.Start, win.Stop, s.loc())
	s.queryCount.Add(1)
	s.influxNanos.Add(int64(time.Since(start)))
	if err != nil {
		s.queryErrors.Add(1)
	}
	return out, err
}

// handleDeviceEvents serves GET /devices/{id}/events: the ordered transition
// edges for one device over the window (vertical-line overlays).
func (s *Server) handleDeviceEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.lookupDevice(w, id); !ok {
		return
	}
	win, ok := s.resolveWindowParams(w, r)
	if !ok {
		return
	}

	timelines, err := s.buildTimeline(r, []string{id}, win)
	if err != nil {
		writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
		return
	}

	// BuildTimeline always returns an entry per requested id; events may be nil
	// (no activity in the window) — normalise to an empty slice for a tidy [].
	evs := timelines[id].Events
	if evs == nil {
		evs = []events.Event{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": id,
		"window":    win.Label,
		"from":      win.Start,
		"to":        win.Stop,
		"events":    evs,
	})
}

// handleDeviceIntervals serves GET /devices/{id}/intervals: the sustained ON
// spans, carry-in opening state, and summary stats for one device (shaded
// bands).
func (s *Server) handleDeviceIntervals(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.lookupDevice(w, id); !ok {
		return
	}
	win, ok := s.resolveWindowParams(w, r)
	if !ok {
		return
	}

	timelines, err := s.buildTimeline(r, []string{id}, win)
	if err != nil {
		writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
		return
	}

	tl := timelines[id]

	writeJSON(w, http.StatusOK, map[string]any{
		"device_id":      id,
		"window":         win.Label,
		"from":           win.Start,
		"to":             win.Stop,
		"state_at_start": tl.StateAtStart,
		"intervals":      roundIntervals(tl.Intervals),
		"stats":          roundStats(tl.Stats),
	})
}

// roundIntervals returns copies of the intervals with DurationS rounded to 1 dp
// for presentation (sub-second precision is noise on minute-scale spans).
func roundIntervals(in []events.Interval) []events.Interval {
	out := make([]events.Interval, len(in))
	for i, iv := range in {
		iv.DurationS = roundTo(iv.DurationS, 1)
		out[i] = iv
	}
	return out
}

// roundStats rounds stats for presentation: seconds to 1 dp, duty to 4 dp.
func roundStats(s events.Stats) events.Stats {
	s.TotalOnSeconds = roundTo(s.TotalOnSeconds, 1)
	s.Duty = roundTo(s.Duty, 4)
	return s
}

// eventSeries is one multi-device /events series: a key (device id or class),
// a label, the class, and the merged, time-ordered events.
type eventSeries struct {
	Key    string         `json:"key"`
	Label  string         `json:"label"`
	Class  string         `json:"class"`
	Events []events.Event `json:"events"`
}

// resolveEventDevices selects the device set for /events from the devices=,
// class= and default rules, writing a 400 (and returning ok=false) on an
// unknown device in an explicit devices= list. The returned ids are sorted for
// deterministic queries/output.
func (s *Server) resolveEventDevices(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	devices := s.Config.Devices()
	q := r.URL.Query()

	var ids []string
	switch {
	case q.Get("devices") != "":
		// Explicit csv list: reject any unknown id for clarity (a typo should
		// surface, not silently yield empty overlays).
		for _, raw := range strings.Split(q.Get("devices"), ",") {
			id := strings.TrimSpace(raw)
			if id == "" {
				continue
			}
			if _, ok := devices[id]; !ok {
				writeError(w, http.StatusBadRequest, "unknown device: "+id)
				return nil, false
			}
			ids = append(ids, id)
		}
	case q.Get("class") != "":
		class := q.Get("class")
		for id, dev := range devices {
			if dev.Class == class {
				ids = append(ids, id)
			}
		}
	default:
		// Default overlay set: all event-bearing devices (binary_state_device,
		// fire_alarm, ...) — see events.IsEventClass.
		for id, dev := range devices {
			if events.IsEventClass(dev.Class) {
				ids = append(ids, id)
			}
		}
	}

	sort.Strings(ids)
	return ids, true
}

// handleEvents serves GET /events: a multi-device categorical timeline overlay.
// The device set comes from devices= (csv), class= (all of a class), or the
// default (all event-bearing devices). group_by=device (default) yields
// one series per device; group_by=class merges every member device's events
// under each class key.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = "device"
	}
	if groupBy != "device" && groupBy != "class" {
		writeError(w, http.StatusBadRequest, "invalid 'group_by' (want one of device, class)")
		return
	}

	ids, ok := s.resolveEventDevices(w, r)
	if !ok {
		return
	}

	win, ok := s.resolveWindowParams(w, r)
	if !ok {
		return
	}

	timelines, err := s.buildTimeline(r, ids, win)
	if err != nil {
		writeError(w, http.StatusBadGateway, "influx query failed: "+err.Error())
		return
	}

	devices := s.Config.Devices()
	var series []eventSeries

	switch groupBy {
	case "class":
		// Merge all member devices' events under each class key, sorted by time.
		byClass := make(map[string][]events.Event)
		for _, id := range ids {
			class := devices[id].Class
			byClass[class] = append(byClass[class], timelines[id].Events...)
		}
		classes := make([]string, 0, len(byClass))
		for class := range byClass {
			classes = append(classes, class)
		}
		sort.Strings(classes)
		for _, class := range classes {
			evs := byClass[class]
			sort.SliceStable(evs, func(i, j int) bool { return evs[i].Time.Before(evs[j].Time) })
			if evs == nil {
				evs = []events.Event{}
			}
			series = append(series, eventSeries{
				Key:    class,
				Label:  class,
				Class:  class,
				Events: evs,
			})
		}
	default: // device
		for _, id := range ids {
			dev := devices[id]
			evs := timelines[id].Events
			if evs == nil {
				evs = []events.Event{}
			}
			series = append(series, eventSeries{
				Key:    id,
				Label:  deviceLabel(id, dev),
				Class:  dev.Class,
				Events: evs,
			})
		}
	}

	if series == nil {
		series = []eventSeries{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"window":   win.Label,
		"from":     win.Start,
		"to":       win.Stop,
		"group_by": groupBy,
		"series":   series,
	})
}

// deviceLabel returns a human-readable label for a device, falling back to its
// id when no display name is configured.
func deviceLabel(id string, dev config.DeviceConfig) string {
	if dev.DisplayName != "" {
		return dev.DisplayName
	}
	return id
}

// catalogEntry is one /devices catalog row.
type catalogEntry struct {
	ID           string   `json:"id"`
	DisplayName  string   `json:"display_name"`
	Location     string   `json:"location"`
	Class        string   `json:"class"`
	Capabilities []string `json:"capabilities"`
}

// handleDevices serves GET /devices: the discovery catalog. It is a pass-through
// of the statehouse_devices snapshot enriched with a derived capabilities hint
// (energy when the class is metered, events for event-bearing classes —
// binary_state_device, fire_alarm), so a UI can build a device picker without
// knowing Influx. Sorted by id for stable output.
func (s *Server) handleDevices(w http.ResponseWriter, _ *http.Request) {
	devices := s.Config.Devices()

	ids := make([]string, 0, len(devices))
	for id := range devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]catalogEntry, 0, len(ids))
	for _, id := range ids {
		dev := devices[id]
		caps := []string{}
		if _, metered := energy.PathForClass(dev.Class); metered {
			caps = append(caps, "energy")
		}
		if events.IsEventClass(dev.Class) {
			caps = append(caps, "events")
		}
		out = append(out, catalogEntry{
			ID:           id,
			DisplayName:  dev.DisplayName,
			Location:     dev.Location,
			Class:        dev.Class,
			Capabilities: caps,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}
