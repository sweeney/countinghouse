package config

import "testing"

// The legacy free-text `location` carried two different facts. Usually a place, but
// for the electricity meter, central heating and hot water it carried `house`, which
// is a coverage scope and not a room — the conflation this migration removes. And
// `house` is one of countinghouse's own reserved series keys, so keying a room series
// on it is doubly wrong.
func TestLegacyHouseLocationIsCoverageNotARoom(t *testing.T) {
	d := DeviceConfig{Location: "house"}

	if got := d.Place(); got != "" {
		t.Errorf("Place() = %q, want empty: `house` is a scope, not a room", got)
	}
	if !d.CoversWholeSite() {
		t.Error("CoversWholeSite() = false for a device whose legacy location was `house`")
	}
}

func TestExplicitCoversStillWins(t *testing.T) {
	d := DeviceConfig{Room: "groundfloor.boiler-room", Covers: "house"}

	if got := d.Place(); got != "groundfloor.boiler-room" {
		t.Errorf("Place() = %q, want the room it sits in", got)
	}
	if !d.CoversWholeSite() {
		t.Error("CoversWholeSite() = false despite covers: house")
	}
}

func TestOrdinaryDeviceCoversItsOwnRoom(t *testing.T) {
	d := DeviceConfig{Location: "kitchen"}

	if d.Place() != "kitchen" {
		t.Errorf("Place() = %q, want kitchen", d.Place())
	}
	if d.CoversWholeSite() {
		t.Error("an ordinary device must cover only its own room")
	}
}
