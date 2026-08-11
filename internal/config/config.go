package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level service configuration loaded from local YAML.
// Countinghouse is read-side only: there is no MQTT, ingest, or adapter
// configuration here. Device inventory and tariffs come from the remote
// config service (a later milestone).
type Config struct {
	Site         SiteConfig         `yaml:"site"`
	HTTP         HTTPConfig         `yaml:"http"`
	Influx       InfluxConfig       `yaml:"influx"`
	Identity     IdentityConfig     `yaml:"identity"`
	RemoteConfig RemoteConfigConfig `yaml:"remote_config"`
	House        HouseConfig        `yaml:"house"`

	// warnings are config states that are legal but probably not what the operator
	// meant. Collected during Load, before defaults are filled in, because filling
	// them in is what makes the mistake invisible.
	warnings []string
}

// Warnings returns the suspicious-but-legal config states found during Load, for the
// caller to log at startup.
//
// They are warnings rather than errors on purpose: every one of them describes a
// config that runs correctly for the single site deployed today, and refusing to start
// would turn a second site's paperwork into an outage of the first.
func (c Config) Warnings() []string { return c.warnings }

// SiteConfig identifies the property this instance serves and where that property's
// configuration lives.
//
// It is a block rather than a bare id so adding a second property is a config edit
// rather than a code change: each site names its own devices namespace, which is what
// makes the namespaces per-site rather than one shared document.
type SiteConfig struct {
	// ID matches an entry in the `sites` namespace.
	ID string `yaml:"id"`

	// DevicesNamespace is the config namespace holding this site's devices.
	// Defaults to the shared pre-migration namespace, so a config that predates the
	// per-site split keeps reading exactly what it always read.
	DevicesNamespace string `yaml:"devices_namespace"`
}

// DefaultDevicesNamespace is the single shared namespace every service read before
// devices were split per site.
const DefaultDevicesNamespace = "statehouse_devices"

// UnmarshalYAML accepts either the block form or a bare id:
//
//	site: home
//	site:
//	  id: home
//	  devices_namespace: devices_home
func (s *SiteConfig) UnmarshalYAML(unmarshal func(any) error) error {
	var id string
	if err := unmarshal(&id); err == nil {
		s.ID = id
		return nil
	}
	type raw SiteConfig
	var r raw
	if err := unmarshal(&r); err != nil {
		return err
	}
	*s = SiteConfig(r)
	return nil
}

// HTTPConfig describes the HTTP listener.
type HTTPConfig struct {
	Listen    string `yaml:"listen"`
	PublicURL string `yaml:"public_url"`
}

// InfluxConfig describes the (read-only) connection to InfluxDB. Token
// may be supplied inline or via TokenFile; Load reads the file when
// Token is empty.
type InfluxConfig struct {
	URL       string `yaml:"url"`
	Org       string `yaml:"org"`
	Bucket    string `yaml:"bucket"`
	Token     string `yaml:"token"`
	TokenFile string `yaml:"token_file"`
}

// IdentityConfig holds credentials for the identity service used to
// obtain access tokens for service-to-service calls (outbound remote
// config fetches).
type IdentityConfig struct {
	BaseURL      string `yaml:"base_url"`
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
}

// RemoteConfigConfig holds the address of the remote config service.
type RemoteConfigConfig struct {
	BaseURL string `yaml:"base_url"`
}

// HouseConfig holds house-wide settings.
type HouseConfig struct {
	// Timezone names a tz database location (e.g. "Europe/London") used to
	// resolve query windows (today/week/month) to half-open ranges. Empty
	// means UTC; "Local" uses the host time zone. Load() rejects values
	// that time.LoadLocation cannot resolve (typo, missing tzdata) with a
	// clear error so operators see the diagnostic at startup.
	Timezone string `yaml:"timezone" json:"timezone,omitempty"`
}

// Location returns the time.Location implied by Timezone. Falls back to
// time.UTC on parse failure; production configs go through Load() which
// rejects invalid timezones up front, so this fallback only matters for
// hand-crafted HouseConfig values in tests.
func (h HouseConfig) Location() *time.Location {
	if h.Timezone == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(h.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// Default returns a config populated with safe defaults; YAML values
// override these.
func Default() Config {
	return Config{
		HTTP: HTTPConfig{Listen: ":8585"},
		Influx: InfluxConfig{
			Org:    "swee.net",
			Bucket: "statehouse",
		},
		House: HouseConfig{Timezone: "Europe/London"},
	}
}

// Load reads and parses YAML from path on top of the defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Influx.Token == "" && cfg.Influx.TokenFile != "" {
		tok, err := os.ReadFile(cfg.Influx.TokenFile)
		if err != nil {
			return cfg, fmt.Errorf("read influx token: %w", err)
		}
		cfg.Influx.Token = string(trimTrailingNewline(tok))
	}
	if cfg.House.Timezone != "" {
		if _, err := time.LoadLocation(cfg.House.Timezone); err != nil {
			return cfg, fmt.Errorf("parse house.timezone %q: %w", cfg.House.Timezone, err)
		}
	}
	cfg.warnings = siteWarnings(cfg.Site)
	if cfg.Site.DevicesNamespace == "" {
		cfg.Site.DevicesNamespace = DefaultDevicesNamespace
	}
	return cfg, nil
}

// siteWarnings reports a half-filled site block. Must run before the default is
// applied: afterwards "defaulted" and "explicitly set to the shared namespace" are the
// same value, and the distinction is the whole point.
//
// The namespace is deliberately not derived from the id. A namespace is a document
// that either exists or does not, so guessing `devices_<id>` would convert a typo in
// `id` into a successful fetch of nothing — fail-open, an empty snapshot, and every
// endpoint reporting zero devices — rather than into this complaint. Two facts, stated
// twice, checked against each other.
func siteWarnings(s SiteConfig) []string {
	switch {
	case s.ID != "" && s.DevicesNamespace == "":
		return []string{fmt.Sprintf(
			"site %q names no devices_namespace, so it reads the shared %s; "+
				"if this instance serves its own property, name its namespace explicitly",
			s.ID, DefaultDevicesNamespace)}
	case s.ID == "" && s.DevicesNamespace != "":
		return []string{fmt.Sprintf(
			"devices_namespace %q is set but the site has no id, so this instance "+
				"cannot report which property it serves", s.DevicesNamespace)}
	}
	return nil
}

func trimTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
