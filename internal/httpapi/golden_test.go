package httpapi

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the golden alias snapshots")

// These snapshots pinned the deprecated `location` spelling until it was removed in
// step 11. The alias case was deleted deliberately, and its golden file with it.
func TestGoldenDeprecatedAliasResponses(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"group-by-room", "/series?window=today&interval=1h&group_by=room"},
		{"bill-breakdown", "/bill?window=today"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// seriesSetup, not dataSetup: dataSetup's fake keys on single-device flux
			// substrings, so the multi-device series queries demuxed to nothing and
			// every snapshot was all zeros — pinning shape but not one computed value.
			s := seriesSetup(t,
				map[string]float64{"winefridge": 0.05, "network-ups": 0.02},
				map[string]float64{"winefridge": 52.0, "network-ups": 100.0})
			s.Config = fakeConfig{devices: roomDevices(), tariffs: testTariffs()}

			got := doGET(t, s, tc.path).Body.Bytes()

			var pretty any
			if err := json.Unmarshal(got, &pretty); err != nil {
				t.Fatalf("response is not JSON: %v", err)
			}
			formatted, err := json.MarshalIndent(pretty, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			formatted = append(formatted, '\n')

			path := filepath.Join("testdata", tc.name+".golden.json")
			if *updateGolden {
				if err := os.WriteFile(path, formatted, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v\nrun: go test ./internal/httpapi -update-golden", err)
			}
			if string(formatted) != string(want) {
				t.Errorf("%s drifted from its golden snapshot.\nIf this is a deliberate "+
					"alias removal, regenerate with -update-golden.\n got: %s\nwant: %s",
					tc.path, formatted, want)
			}
		})
	}
}
