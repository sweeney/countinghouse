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
// only Class, Location, and DisplayName (to route queries and group the
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
	Location    string      `yaml:"location"         json:"location,omitempty"`
	Thresholds  *Thresholds `yaml:"thresholds"       json:"thresholds,omitempty"`

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
