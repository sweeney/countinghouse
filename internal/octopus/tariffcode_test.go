package octopus

import "testing"

// The product code is not carried separately by the API responses we consume:
// every rate endpoint is addressed as
//
//	/products/{product}/electricity-tariffs/{tariff}/standard-unit-rates/
//
// so a caller holding only the tariff code (which is what config and the
// account's agreements carry) must be able to derive the product. These tests
// pin that derivation, including the shapes we do NOT want to accept silently —
// a wrong product code yields a 404 at best and somebody else's prices at worst.
func TestProductFromTariff(t *testing.T) {
	tests := []struct {
		name    string
		tariff  string
		product string
		region  string
		wantErr bool
	}{
		{
			// Region A throughout, deliberately: the GSP letter is derived from a
			// property's address, so the real one is not something a public repo's
			// tests should carry. Which letter it is makes no difference to parsing.
			name:    "single-register electricity, the Agile product",
			tariff:  "E-1R-AGILE-24-10-01-A",
			product: "AGILE-24-10-01",
			region:  "A",
		},
		{
			name:    "single-register electricity, a fixed product",
			tariff:  "E-1R-OE-FIX-12M-25-09-09-A",
			product: "OE-FIX-12M-25-09-09",
			region:  "A",
		},
		{
			name:    "a variable product, whose rates carry payment methods",
			tariff:  "E-1R-VAR-22-11-01-B",
			product: "VAR-22-11-01",
			region:  "B",
		},
		{
			// Economy 7 and similar are two-register. We do not bill one today,
			// but the code shape is legal and must parse rather than be rejected:
			// silently refusing a valid tariff is how a future migration breaks.
			name:    "two-register electricity parses the same way",
			tariff:  "E-2R-VAR-22-11-01-D",
			product: "VAR-22-11-01",
			region:  "D",
		},
		{
			// Gas uses the same grammar. Countinghouse bills electricity only, so
			// this is not reached today, but the parser should not be the thing
			// that decides that.
			name:    "gas parses, fuel is reported rather than assumed",
			tariff:  "G-1R-OE-FIX-18M-26-09-08-E",
			product: "OE-FIX-18M-26-09-08",
			region:  "E",
		},
		{
			name:    "every GSP letter is accepted, not just the one we bill on",
			tariff:  "E-1R-AGILE-24-10-01-C",
			product: "AGILE-24-10-01",
			region:  "C",
		},
		{name: "empty", tariff: "", wantErr: true},
		{name: "no register segment", tariff: "E-AGILE-24-10-01-A", wantErr: true},
		{name: "no region suffix", tariff: "E-1R-AGILE-24-10-01", wantErr: true},
		{name: "region is not a single letter", tariff: "E-1R-AGILE-24-10-01-AA", wantErr: true},
		{name: "region is a digit", tariff: "E-1R-AGILE-24-10-01-1", wantErr: true},
		{name: "nothing between register and region", tariff: "E-1R-N", wantErr: true},
		{
			// Anything that could change the request path must be refused: the
			// tariff code is interpolated into a URL.
			name:    "path traversal is refused",
			tariff:  "E-1R-../../secrets-N",
			wantErr: true,
		},
		{name: "whitespace is refused rather than trimmed", tariff: "E-1R-AGILE-24-10-01-A ", wantErr: true},
		{name: "lowercase is refused rather than upcased", tariff: "e-1r-agile-24-10-01-n", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseTariffCode(tc.tariff)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseTariffCode(%q) = %+v, want error", tc.tariff, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTariffCode(%q) unexpected error: %v", tc.tariff, err)
			}
			if got.Product != tc.product {
				t.Errorf("product = %q, want %q", got.Product, tc.product)
			}
			if got.Region != tc.region {
				t.Errorf("region = %q, want %q", got.Region, tc.region)
			}
			if got.Code != tc.tariff {
				t.Errorf("Code = %q, want the input %q preserved verbatim", got.Code, tc.tariff)
			}
		})
	}
}

// The fuel is reported so callers can refuse what they do not bill, rather than
// the parser deciding. Countinghouse bills electricity; gas is read and ignored.
func TestParseTariffCodeReportsFuel(t *testing.T) {
	elec, err := ParseTariffCode("E-1R-AGILE-24-10-01-A")
	if err != nil {
		t.Fatal(err)
	}
	if !elec.IsElectricity() {
		t.Error("E-1R-… should report as electricity")
	}

	gas, err := ParseTariffCode("G-1R-OE-FIX-18M-26-09-08-E")
	if err != nil {
		t.Fatal(err)
	}
	if gas.IsElectricity() {
		t.Error("G-1R-… must not report as electricity")
	}
}

// The GSP endpoint returns the region with a leading underscore ("_N") while
// tariff codes use the bare letter ("N"). Mixing the two builds a URL for a
// tariff that does not exist, so the normalisation is explicit and tested.
func TestNormaliseGroupID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"_N", "N"},
		{"N", "N"},
		{"_C", "C"},
		{"_P", "P"},
	} {
		if got := NormaliseGroupID(tc.in); got != tc.want {
			t.Errorf("NormaliseGroupID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
