package httpapi

import (
	"encoding/json"
	"testing"
)

// Which property an instance believes it serves is currently only observable by
// inference: read remote_config's keys and deduce the namespace, and the site id is
// not observable at all because nothing reads it.
//
// That is the wrong way round for the misconfiguration this feature makes possible. A
// second instance pointed at the wrong namespace serves another property's data while
// looking entirely healthy, so the config it resolved has to be legible on the endpoint
// an operator already checks — not deducible from whether the numbers look plausible.
func TestHealthReportsTheResolvedSite(t *testing.T) {
	s, _ := dataSetup(t)
	s.SiteID = "cottage"
	s.DevicesNamespace = "devices_cottage"

	var resp struct {
		Site struct {
			ID               string `json:"id"`
			DevicesNamespace string `json:"devices_namespace"`
		} `json:"site"`
	}
	if err := json.Unmarshal(doGET(t, s, "/healthz").Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Site.ID != "cottage" {
		t.Errorf("healthz site.id = %q, want cottage", resp.Site.ID)
	}
	if resp.Site.DevicesNamespace != "devices_cottage" {
		t.Errorf("healthz site.devices_namespace = %q, want devices_cottage",
			resp.Site.DevicesNamespace)
	}
}

// An instance with no site configured predates the split and is not misconfigured, so
// it says nothing rather than reporting an empty site that reads as a fault.
func TestHealthOmitsAnUnconfiguredSite(t *testing.T) {
	s, _ := dataSetup(t)

	var resp map[string]any
	if err := json.Unmarshal(doGET(t, s, "/healthz").Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp["site"]; ok {
		t.Errorf("healthz reports a site block when none is configured: %v", resp["site"])
	}
}
