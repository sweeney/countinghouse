package config

import "github.com/sweeney/common/devices"

// DeviceConfig is one device in the site's devices namespace: the shared
// schema in common/devices, whose Place and CoversWholeSite every grouping and
// billing path goes through, so they agree by construction. Countinghouse
// reads Class, Room, Floor, Covers and DisplayName to route queries and group
// the bill.
type DeviceConfig = devices.Device

// Thresholds are statehouse's activity thresholds, part of the shared schema.
// Countinghouse does not use them.
type Thresholds = devices.Thresholds

// CoverageHouse is the sentinel meaning a device's readings describe the whole
// property rather than the room it sits in. It is also one of countinghouse's
// own reserved series keys, so keying a room series on it would collide as
// well as mislead.
const CoverageHouse = devices.CoverageHouse
