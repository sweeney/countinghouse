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
	HTTP         HTTPConfig         `yaml:"http"`
	Influx       InfluxConfig       `yaml:"influx"`
	Identity     IdentityConfig     `yaml:"identity"`
	RemoteConfig RemoteConfigConfig `yaml:"remote_config"`
	House        HouseConfig        `yaml:"house"`
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
	return cfg, nil
}

func trimTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
