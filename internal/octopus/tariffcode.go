// Package octopus is a typed, read-only client for the Octopus Energy REST API.
//
// It covers the two things countinghouse needs:
//
//   - the PUBLIC product endpoints — half-hourly unit rates and daily standing
//     charges. No authentication at all.
//   - (later) the AUTHENTICATED account endpoints, which report which tariff an
//     account is actually on. Those take an API key as the HTTP Basic username.
//
// The package is deliberately dumb about meaning: it parses what the API
// returned and hands it on. It does not validate plausibility, convert pence to
// pounds, apply VAT, or decide that a price is too extreme to be real — all of
// that belongs to internal/prices, which can then be tested without a network
// at all. The one thing this package does enforce is that nothing unvalidated
// reaches a URL path.
package octopus

import (
	"fmt"
	"strings"
)

// TariffCode is a parsed Octopus tariff code.
//
// The grammar is `{fuel}-{registers}R-{product}-{gsp}`, e.g.
// `E-1R-AGILE-24-10-01-N`: electricity, single register, product
// `AGILE-24-10-01`, grid-supply-point region `N`.
//
// Product matters because it is a SEPARATE path segment from the tariff on
// every rate endpoint, and callers generally hold only the tariff code — it is
// what the config namespace and the account's agreements carry.
type TariffCode struct {
	// Code is the input, preserved verbatim. It is what gets stored as the
	// archive's key, so it must never be a normalised or re-assembled version
	// of what the caller supplied.
	Code string

	Fuel      string // "E" electricity, "G" gas
	Registers string // "1" single-rate, "2" two-register (Economy 7 and friends)
	Product   string // "AGILE-24-10-01"
	Region    string // GSP letter, "A".."P"
}

// IsElectricity reports whether this is an electricity tariff. Countinghouse
// bills electricity only; the parser reports the fuel rather than refusing gas,
// so the decision to ignore it is visible at the call site instead of hidden
// in a parse error.
func (t TariffCode) IsElectricity() bool { return t.Fuel == "E" }

// ParseTariffCode splits a tariff code into its parts.
//
// It is strict on purpose. The product and tariff are interpolated into a
// request path, so anything that could change that path — a traversal segment,
// stray whitespace, an unexpected case — is refused rather than cleaned up.
// Silently "fixing" a malformed code risks fetching a DIFFERENT tariff's prices
// and archiving them under the key we were asked for, which is the one failure
// here that would be invisible and would corrupt money.
//
// Lowercase is refused rather than upcased for the same reason: the API treats
// codes case-sensitively, so accepting `e-1r-…` would mean silently querying
// something the caller did not name.
func ParseTariffCode(code string) (TariffCode, error) {
	if code == "" {
		return TariffCode{}, fmt.Errorf("octopus: empty tariff code")
	}
	// Reject anything that is not the conservative character set the grammar
	// uses. This is the guard that keeps the URL path safe; everything below it
	// is shape-checking, not safety.
	for _, r := range code {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return TariffCode{}, fmt.Errorf("octopus: tariff code %q contains an illegal character %q", code, r)
		}
	}

	parts := strings.Split(code, "-")
	// Minimum legal shape is fuel, register, at least one product segment, region.
	if len(parts) < 4 {
		return TariffCode{}, fmt.Errorf("octopus: tariff code %q is too short to be {fuel}-{n}R-{product}-{gsp}", code)
	}

	fuel := parts[0]
	if fuel != "E" && fuel != "G" {
		return TariffCode{}, fmt.Errorf("octopus: tariff code %q has unknown fuel %q, want E or G", code, fuel)
	}

	reg := parts[1]
	if len(reg) != 2 || reg[1] != 'R' || reg[0] < '1' || reg[0] > '9' {
		return TariffCode{}, fmt.Errorf("octopus: tariff code %q has malformed register segment %q, want e.g. 1R", code, reg)
	}

	region := parts[len(parts)-1]
	if len(region) != 1 || region[0] < 'A' || region[0] > 'P' {
		return TariffCode{}, fmt.Errorf("octopus: tariff code %q has malformed GSP region %q, want a single letter A-P", code, region)
	}

	product := strings.Join(parts[2:len(parts)-1], "-")
	if product == "" {
		return TariffCode{}, fmt.Errorf("octopus: tariff code %q has an empty product", code)
	}

	return TariffCode{
		Code:      code,
		Fuel:      fuel,
		Registers: string(reg[0]),
		Product:   product,
		Region:    region,
	}, nil
}

// NormaliseGroupID converts a grid-supply-point group id into the form tariff
// codes use.
//
// `GET /industry/grid-supply-points/?postcode=…` answers with `{"group_id":"_N"}`
// — note the leading underscore — while tariff codes spell the same region as a
// bare `N`. Feeding `_N` into a tariff code builds a path for a tariff that does
// not exist, so the two spellings are reconciled in one documented place rather
// than at each call site.
func NormaliseGroupID(groupID string) string {
	return strings.TrimPrefix(groupID, "_")
}
