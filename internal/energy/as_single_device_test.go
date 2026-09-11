package energy

import (
	"testing"
	"time"
)

// AsSingleDevice is the reshape behind /devices/unmonitored/series. Issue #23 was
// that it was not one operation: the handler rewrote GroupBy and filtered the
// series but left the embedded HouseStats, so a device-grouped body carried the
// house-only confidence signals.
func TestAsSingleDeviceClearsHouseOnlyFields(t *testing.T) {
	cov := 0.1
	stale := 1
	resp := SeriesResponse{
		GroupBy: GroupByHouse,
		HouseStats: HouseStats{
			Coverage:            &cov,
			StaleMonitoredCount: &stale,
			StaleMonitoredIDs:   []string{"network-ups"},
		},
		Series: []Series{
			{Key: houseMonitoredKey, TotalKWh: 0.15},
			{Key: UnmonitoredID, TotalKWh: 0.35},
			{Key: houseMeterKey, TotalKWh: 0.5},
		},
	}

	got := resp.AsSingleDevice(UnmonitoredID)

	if got.GroupBy != GroupByDevice {
		t.Errorf("group_by = %q, want %q", got.GroupBy, GroupByDevice)
	}
	if len(got.Series) != 1 || got.Series[0].Key != UnmonitoredID {
		t.Fatalf("series = %+v, want only %q", got.Series, UnmonitoredID)
	}
	if got.Coverage != nil {
		t.Errorf("coverage = %v, want nil — it describes the house decomposition, not this series", *got.Coverage)
	}
	if got.StaleMonitoredCount != nil {
		t.Errorf("stale_monitored_count = %v, want nil", *got.StaleMonitoredCount)
	}
	if got.StaleMonitoredIDs != nil {
		t.Errorf("stale_monitored_ids = %v, want nil", got.StaleMonitoredIDs)
	}

	// The source response is untouched: the reshape returns a copy, so a caller
	// can still record drift or serve the house grouping from the same build.
	if resp.Coverage == nil || len(resp.Series) != 3 {
		t.Errorf("the house response was mutated: %+v", resp)
	}
}

// Drift is NEVER serialised and is the operator-facing C3 signal the handler
// turns into a metric — a property of the computation, not of the response shape.
// Reshaping to a device view must not drop it.
func TestAsSingleDeviceKeepsDrift(t *testing.T) {
	at := time.Date(2026, 6, 11, 3, 0, 0, 0, time.UTC)
	resp := SeriesResponse{
		GroupBy: GroupByHouse,
		Series:  []Series{{Key: UnmonitoredID}},
		Drift:   DriftStats{ClampedBuckets: 2, WorstResidualKWh: -0.4, WorstAt: at},
	}

	got := resp.AsSingleDevice(UnmonitoredID)
	if !got.Drift.HasDrift() || got.Drift.ClampedBuckets != 2 || got.Drift.WorstResidualKWh != -0.4 || !got.Drift.WorstAt.Equal(at) {
		t.Errorf("drift = %+v, want it carried through the reshape", got.Drift)
	}
}

// A key that is not present yields no series rather than a nil slice, which is
// what lets the handler's single-series guard report the inconsistency as a 500
// instead of serving 200 with a full bucket axis and no data (issue #21).
func TestAsSingleDeviceMissingKeyYieldsEmptyNotNil(t *testing.T) {
	resp := SeriesResponse{GroupBy: GroupByHouse, Series: []Series{{Key: houseMeterKey}}}
	got := resp.AsSingleDevice(UnmonitoredID)
	if got.Series == nil {
		t.Fatal("series = nil; an empty grouping is an empty list, not absence")
	}
	if len(got.Series) != 0 {
		t.Errorf("series = %+v, want none", got.Series)
	}
}
