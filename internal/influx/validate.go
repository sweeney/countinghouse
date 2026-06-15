package influx

import (
	"fmt"
	"regexp"
)

// deviceIDRe is the conservative allowlist of characters permitted in a device
// id embedded into Flux. It matches the real swee.net ids (winefridge,
// network-ups, office-ups, electricity_meter, z2m.0x… style ids) while
// excluding everything Flux-significant: quotes, backslashes, the `$`/`{}`
// string-interpolation syntax, whitespace, commas and any non-ASCII byte.
var deviceIDRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// validID reports whether id is a safe device identifier to embed in a Flux
// query string. It is the trust-boundary check for the influx layer: device ids
// originate from the statehouse_devices remote-config snapshot, and `%q` alone
// guarantees a valid *Go* literal, not a Flux one. Validating here fails closed
// so a malformed or hostile id can never alter query semantics or produce a
// malformed query, even if a future caller forwards an id that skipped the
// inventory check.
func validID(id string) bool {
	return deviceIDRe.MatchString(id)
}

// deviceIDPredicate renders the single-device Flux filter predicate for id,
// failing closed: a valid id yields `r.device_id == "id"`; an invalid id yields
// the constant `false` so the query matches nothing rather than embedding an
// unvalidated value.
func deviceIDPredicate(id string) string {
	if !validID(id) {
		return "false"
	}
	return fmt.Sprintf("r.device_id == %q", id)
}
