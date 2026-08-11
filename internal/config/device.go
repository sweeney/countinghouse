package config

import "time"

// Thresholds describes the per-class activity detection thresholds.
// Countinghouse does not use thresholds itself, but the struct is mirrored
// from statehouse so that DeviceConfig fetched from the shared
// `statehouse_devices` namespace round-trips cleanly. All fields are
// pointers so that an explicitly-set zero value is honoured.
type Thresholds struct {
	IdleBelowW           *float64       `yaml:"idle_below_w"            json:"-"`
	ActiveAboveW         *float64       `yaml:"active_above_w"          json:"-"`
	ActiveSustainedFor   *time.Duration `yaml:"active_sustained_for"    json:"-"`
	InactiveSustainedFor *time.Duration `yaml:"inactive_sustained_for"  json:"-"`
	CompressorAboveW     *float64       `yaml:"compressor_above_w"      json:"-"`
}

// DeviceConfig mirrors statehouse's device entry. Countinghouse reads
// only Class, Room, and DisplayName (to route queries and group the
// bill), but the full struct is kept so the shared `statehouse_devices`
// namespace parses without loss. The canonical identity fields are
// Scheme + Primary (and Display); the legacy `ieee_address` /
// `friendly_name` fields are Z2M shorthand that normaliseDevices folds in.
type DeviceConfig struct {
	// Canonical identity fields. Scheme names the adapter that owns the
	// device ("zigbee", "tasmota", "shelly", ...). Primary is the
	// adapter's stable identifier. Display is the human-readable name.
	Scheme  string `yaml:"scheme"   json:"scheme,omitempty"`
	Primary string `yaml:"primary"  json:"primary,omitempty"`
	Display string `yaml:"display"  json:"display,omitempty"`

	// Legacy Z2M shorthand. normaliseDevices converts these to
	// scheme=zigbee + primary=ieee_address / display=friendly_name.
	IEEEAddress  string `yaml:"ieee_address"   json:"ieee_address,omitempty"`
	FriendlyName string `yaml:"friendly_name"  json:"friendly_name,omitempty"`

	Class       string      `yaml:"class"            json:"class,omitempty"`
	DisplayName string      `yaml:"display_name"     json:"display_name,omitempty"`
	Thresholds  *Thresholds `yaml:"thresholds"       json:"thresholds,omitempty"`

	// Room is the floorplan room id this device sits in, e.g.
	// "groundfloor.kitchen". It replaces Location.
	Room string `yaml:"room" json:"room,omitempty"`

	// Covers is what this device's readings describe, when that is NOT the room
	// it sits in. Either the literal "house" or another room id; absent means it
	// covers its own room.
	//
	// The distinction is the one structural defect in the old single-field
	// scheme: central_heating, hot_water and electricity_meter each sit in one
	// room while their readings describe the whole property, so `location` was
	// recording sometimes one fact and sometimes the other. Two facts, two fields.
	Covers string `yaml:"covers" json:"covers,omitempty"`

	// Location is the free-text place the device used to declare, and is
	// DEPRECATED. It is still decoded because a devices namespace that has not
	// been republished yet still carries it.
	Location string `yaml:"location" json:"location,omitempty"`

	// EnergyStrategy is mirrored for completeness but is irrelevant to
	// countinghouse routing (routing is class/energy_kwh-derived; see
	// PLAN.md §5).
	EnergyStrategy string `yaml:"energy_strategy" json:"energy_strategy,omitempty"`
}

// normaliseDevices converts legacy ieee_address/friendly_name shorthands
// into the canonical scheme/primary/display fields. Mirrors statehouse so
// devices fetched from the remote namespace are normalised identically.
func normaliseDevices(devices map[string]DeviceConfig) {
	for id, d := range devices {
		if d.Scheme == "" && (d.IEEEAddress != "" || d.FriendlyName != "") {
			d.Scheme = "zigbee"
		}
		if d.Primary == "" && d.IEEEAddress != "" {
			d.Primary = d.IEEEAddress
		}
		if d.Display == "" && d.FriendlyName != "" {
			d.Display = d.FriendlyName
		}
		devices[id] = d
	}
}

// Place returns the room this device is grouped and billed under: its Room when the
// namespace has been migrated, otherwise its deprecated Location.
//
// Every grouping path goes through this one function, which is what makes
// group_by=room and group_by=location return identical numbers during the alias
// period instead of merely being documented to.
func (d DeviceConfig) Place() string {
	if d.Room != "" {
		return d.Room
	}
	if d.Location == CoverageHouse {
		return ""
	}
	return d.Location
}

// CoverageHouse is the sentinel meaning a device's readings describe the whole
// property rather than the room it sits in.
//
// It is also a legacy `location` value. That field carried two different facts —
// usually a place, but `house` was always a scope — which is the conflation this
// migration removes. It is also one of countinghouse's own reserved series keys, so
// keying a room series on it would collide as well as mislead.
const CoverageHouse = "house"

// CoversWholeSite reports whether this device's readings describe the whole property
// rather than the room it sits in.
func (d DeviceConfig) CoversWholeSite() bool {
	if d.Covers != "" {
		return d.Covers == CoverageHouse
	}
	return d.Room == "" && d.Location == CoverageHouse
}
