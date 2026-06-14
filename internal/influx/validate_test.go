package influx

import (
	"strings"
	"testing"
)

// TestValidID locks the device-id allowlist (issue #5): the real swee.net ids
// are accepted, and anything carrying Flux-significant or non-identifier
// characters is rejected, so a config value can never embed such a sequence in
// a Flux string.
func TestValidID(t *testing.T) {
	valid := []string{
		"winefridge", "network-ups", "office-ups", "electricity_meter",
		"a.b:c_d-e", "Sensor1", "0x12AB", "z2m.0x00124b00",
	}
	for _, id := range valid {
		if !validID(id) {
			t.Errorf("validID(%q) = false, want true", id)
		}
	}

	invalid := []string{
		"",                  // empty
		`a"b`,               // double quote
		`a\b`,               // backslash
		"a$b",               // Flux string-interpolation sigil
		"a${x}",             // interpolation expression
		"a{b",               // brace
		"a b",               // whitespace
		"a\tb",              // tab
		"a\nb",              // newline
		"café",              // non-ASCII
		"a,b",               // set-literal separator
		`["x"]`,             // array literal injection
		"r.device_id == \"", // raw predicate fragment
	}
	for _, id := range invalid {
		if validID(id) {
			t.Errorf("validID(%q) = true, want false", id)
		}
	}
}

// TestDeviceSet_ExcludesHostileIDs proves the set builder fails closed: an id
// that is not a safe identifier is dropped from the emitted Flux array rather
// than interpolated into the query string.
func TestDeviceSet_ExcludesHostileIDs(t *testing.T) {
	got := deviceSet([]string{"winefridge", `evil" or r._value > 0 or "`, "network-ups"})
	if got != `["winefridge", "network-ups"]` {
		t.Errorf("deviceSet did not drop hostile id: %q", got)
	}
	if strings.Contains(got, "evil") {
		t.Errorf("hostile id leaked into set: %q", got)
	}
}

// TestSingleDeviceBuilders_FailClosedOnHostileID proves the single-device
// builders never embed an unvalidated id: a hostile id collapses the device
// predicate to `false` (match nothing) instead of interpolating the raw value.
func TestSingleDeviceBuilders_FailClosedOnHostileID(t *testing.T) {
	hostile := `evil" or r._value > 0 or "`
	for name, flux := range map[string]string{
		"counter":  BuildCounterFlux("statehouse", hostile, testStart, testStop),
		"integral": BuildIntegralFlux("statehouse", hostile, testStart, testStop),
	} {
		if strings.Contains(flux, "evil") {
			t.Errorf("%s flux leaked hostile id:\n%s", name, flux)
		}
		if !strings.Contains(flux, "filter(fn: (r) => false)") {
			t.Errorf("%s flux should fail closed with a `false` device predicate:\n%s", name, flux)
		}
		if strings.Contains(flux, `r.device_id == `) {
			t.Errorf("%s flux should not interpolate a device_id predicate for a hostile id:\n%s", name, flux)
		}
	}
}
