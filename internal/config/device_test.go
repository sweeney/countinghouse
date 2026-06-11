package config

import "testing"

func TestNormaliseDevicesLegacyShorthand(t *testing.T) {
	devices := map[string]DeviceConfig{
		"winefridge": {
			IEEEAddress:  "0x00124b00",
			FriendlyName: "Wine Fridge",
			Class:        "continuous_power_device",
		},
	}
	normaliseDevices(devices)
	d := devices["winefridge"]
	if d.Scheme != "zigbee" {
		t.Errorf("scheme = %q, want zigbee", d.Scheme)
	}
	if d.Primary != "0x00124b00" {
		t.Errorf("primary = %q, want ieee address", d.Primary)
	}
	if d.Display != "Wine Fridge" {
		t.Errorf("display = %q, want friendly name", d.Display)
	}
}

func TestNormaliseDevicesDoesNotOverrideCanonical(t *testing.T) {
	devices := map[string]DeviceConfig{
		"plug": {
			Scheme:       "tasmota",
			Primary:      "tasmota-123",
			Display:      "Canonical Name",
			IEEEAddress:  "0xdead",
			FriendlyName: "Legacy Name",
		},
	}
	normaliseDevices(devices)
	d := devices["plug"]
	if d.Scheme != "tasmota" {
		t.Errorf("scheme = %q, want tasmota preserved", d.Scheme)
	}
	if d.Primary != "tasmota-123" {
		t.Errorf("primary = %q, want canonical preserved", d.Primary)
	}
	if d.Display != "Canonical Name" {
		t.Errorf("display = %q, want canonical preserved", d.Display)
	}
}

func TestNormaliseDevicesNoLegacyFields(t *testing.T) {
	devices := map[string]DeviceConfig{
		"meter": {Class: "energy_meter"},
	}
	normaliseDevices(devices)
	d := devices["meter"]
	if d.Scheme != "" {
		t.Errorf("scheme = %q, want empty (no legacy fields to imply zigbee)", d.Scheme)
	}
}
