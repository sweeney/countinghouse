package config

import (
	"fmt"
	"sort"
)

// SiteRecord is one property as described in the shared `sites` namespace.
//
// The document is shared with other services and carries fields countinghouse has
// no use for — coordinates, MQTT topics, the bin scheme. Those are deliberately
// absent from this struct: unknown JSON fields are ignored, so `sites` can grow
// keys that are none of our business without touching this service.
type SiteRecord struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`

	// The namespace pointers. These are facts ABOUT the property — where its data
	// lives — as opposed to site.id in local config, which is a deployment
	// decision about which property this instance serves.
	DevicesNamespace          string `json:"devices_namespace,omitempty"`
	FloorplanNamespace        string `json:"floorplan_namespace,omitempty"`
	EnergyAgreementsNamespace string `json:"energy_agreements_namespace,omitempty"`
}

// Sites is the payload of the shared `sites` namespace.
type Sites struct {
	Sites []SiteRecord `json:"sites"`
}

// Find returns the record for a site id. Case-sensitive: ids are keys, not labels.
func (s Sites) Find(id string) (SiteRecord, bool) {
	for _, r := range s.Sites {
		if r.ID == id {
			return r, true
		}
	}
	return SiteRecord{}, false
}

// SiteNamespaces is the resolved set of namespaces this instance will read.
type SiteNamespaces struct {
	Devices    string
	Floorplan  string
	Agreements string // empty means stay on the legacy energy_tariffs document
}

// ResolveSiteNamespaces decides which namespaces to read, from the shared `sites`
// document with local config as the fallback.
//
// `sites` WINS over a disagreeing local value. That direction is the whole point:
// a stale local pointer silently overriding the correct remote one is exactly the
// drift this replaces — rename a namespace in `sites` and every other service
// follows while this one quietly keeps reading the old document. But a
// disagreement is REPORTED rather than swallowed, because an operator's edit being
// ignored without a word is its own kind of silent failure.
//
// Local config remains the fallback for the FLOORPLAN and AGREEMENTS pointers,
// which is what makes the migration safe: a site whose `sites` entry is only
// partly filled in keeps working off its own config.
//
// devices_namespace has NO local fallback. It is the pointer that decides whether
// any answer is right at all — a stale local copy would not degrade a label, it
// would bill another property's devices while the service looked entirely healthy —
// so it comes from `sites` or the instance does not start.
//
// Returns warnings separately from the error so the caller can log them and carry
// on — they are not failures.
func ResolveSiteNamespaces(local SiteConfig, sites Sites) (SiteNamespaces, []string, error) {
	if local.ID == "" {
		return SiteNamespaces{}, nil, fmt.Errorf(
			"config: site.id is required; an instance that does not know which property it " +
				"serves cannot resolve that property's namespaces, and guessing \"the only site\" " +
				"would break the moment a second one appeared")
	}

	var warns []string
	out := SiteNamespaces{
		// Devices has NO local fallback, deliberately. It is the pointer that
		// decides whether any answer is right at all, so a stale local copy would
		// not degrade a label — it would bill another property's devices while
		// looking entirely healthy. It comes from `sites` or not at all.
		Floorplan:  local.FloorplanNamespace,
		Agreements: local.EnergyAgreementsNamespace,
	}

	// An empty document is the no-remote-config case: local config stands alone.
	if len(sites.Sites) > 0 {
		rec, ok := sites.Find(local.ID)
		if !ok {
			known := make([]string, 0, len(sites.Sites))
			for _, r := range sites.Sites {
				known = append(known, r.ID)
			}
			sort.Strings(known)
			return SiteNamespaces{}, nil, fmt.Errorf(
				"config: site.id %q is not in the sites namespace (it knows %v); "+
					"refusing to serve a property nobody has described", local.ID, known)
		}
		out.Devices = rec.DevicesNamespace
		out.Floorplan = prefer(rec.FloorplanNamespace, local.FloorplanNamespace, "floorplan_namespace", &warns)
		out.Agreements = prefer(rec.EnergyAgreementsNamespace, local.EnergyAgreementsNamespace, "energy_agreements_namespace", &warns)
	}

	// The existing requirement, unchanged by where the values now come from: both
	// are required, because an unnamed floorplan degrades to ids-as-labels, which
	// is silence that reads as data.
	if out.Devices == "" {
		return SiteNamespaces{}, warns, fmt.Errorf(
			"config: site %q names no devices_namespace in the sites namespace; there is no "+
				"default and no local fallback for this one, because a stale local copy would "+
				"bill another property's devices while looking healthy. Add "+
				"devices_namespace to the site's entry in `sites`", local.ID)
	}
	if out.Floorplan == "" {
		return SiteNamespaces{}, warns, fmt.Errorf(
			"config: no floorplan_namespace for site %q, in sites or locally; without it every "+
				"floor and room reports as an id where a name belongs", local.ID)
	}
	// Agreements being empty is legal: it means stay on the legacy tariff document.
	return out, warns, nil
}

// prefer returns the remote value when it is set, warning when a local value
// disagrees and is therefore being overridden.
func prefer(remote, local, field string, warns *[]string) string {
	if remote == "" {
		return local
	}
	if local != "" && local != remote {
		*warns = append(*warns, fmt.Sprintf(
			"local %s=%q is overridden by the sites namespace (%q); remove the local value "+
				"to silence this", field, local, remote))
	}
	return remote
}

// DriftFrom reports namespace pointers that have changed in `sites` since they
// were resolved.
//
// Pointers are resolved ONCE, at startup, and deliberately not adopted on a
// SIGHUP reload. Repointing a running service at a different property's data is
// not something to do silently — the device inventory would swap underneath every
// in-flight answer — so a change is reported and left for an explicit restart,
// which is cheap.
//
// Only meaningful after a sites document has actually been fetched: an EMPTY
// document is treated as "our site is gone", not as "there is no remote config",
// because the caller already knows which of those it is and only asks in the
// former case. Guarding that here instead would silently swallow a real
// disappearance.
func (n SiteNamespaces) DriftFrom(sites Sites, siteID string) []string {
	rec, ok := sites.Find(siteID)
	if !ok {
		return []string{fmt.Sprintf(
			"site %q has disappeared from the sites namespace; still serving the namespaces "+
				"resolved at startup — restart to pick up the change", siteID)}
	}

	var out []string
	for _, c := range []struct{ field, was, now string }{
		{"devices_namespace", n.Devices, rec.DevicesNamespace},
		{"floorplan_namespace", n.Floorplan, rec.FloorplanNamespace},
		{"energy_agreements_namespace", n.Agreements, rec.EnergyAgreementsNamespace},
	} {
		if c.now != "" && c.now != c.was {
			out = append(out, fmt.Sprintf(
				"%s changed in the sites namespace from %q to %q; still reading %q — "+
					"restart to adopt it", c.field, c.was, c.now, c.was))
		}
	}
	return out
}
