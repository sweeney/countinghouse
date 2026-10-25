package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefault(t *testing.T) {
	d := Default()
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"http.listen", d.HTTP.Listen, ":8585"},
		{"influx.org", d.Influx.Org, "swee.net"},
		{"influx.bucket", d.Influx.Bucket, "statehouse"},
		{"house.timezone", d.House.Timezone, "Europe/London"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestLoadOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml", `
site:
  id: "test"
  devices_namespace: "devices_test"
  floorplan_namespace: "floorplan_test"
http:
  listen: ":9090"
  public_url: "https://countinghouse.example"
influx:
  url: "http://influx:8086"
  bucket: "custom_bucket"
  token: "inline-token"
identity:
  base_url: "https://id.example"
  client_id: "ch"
  client_secret: "shh"
remote_config:
  base_url: "https://config.example"
house:
  timezone: "America/New_York"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.Listen != ":9090" {
		t.Errorf("listen = %q", cfg.HTTP.Listen)
	}
	if cfg.HTTP.PublicURL != "https://countinghouse.example" {
		t.Errorf("public_url = %q", cfg.HTTP.PublicURL)
	}
	if cfg.Influx.Bucket != "custom_bucket" {
		t.Errorf("bucket = %q", cfg.Influx.Bucket)
	}
	// Default not overridden by YAML should survive.
	if cfg.Influx.Org != "swee.net" {
		t.Errorf("org = %q, want default swee.net", cfg.Influx.Org)
	}
	if cfg.Identity.ClientID != "ch" {
		t.Errorf("client_id = %q", cfg.Identity.ClientID)
	}
	if cfg.RemoteConfig.BaseURL != "https://config.example" {
		t.Errorf("remote_config.base_url = %q", cfg.RemoteConfig.BaseURL)
	}
	if cfg.House.Timezone != "America/New_York" {
		t.Errorf("timezone = %q", cfg.House.Timezone)
	}
}

func TestLoadTokenFileFallback(t *testing.T) {
	dir := t.TempDir()
	tokPath := writeFile(t, dir, "influx-token", "secret-token\n")
	p := writeFile(t, dir, "config.yaml", `
site:
  id: "test"
  devices_namespace: "devices_test"
  floorplan_namespace: "floorplan_test"
influx:
  url: "http://influx:8086"
  token_file: "`+tokPath+`"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Influx.Token != "secret-token" {
		t.Errorf("token = %q, want trimmed %q", cfg.Influx.Token, "secret-token")
	}
}

func TestLoadInlineTokenWinsOverFile(t *testing.T) {
	dir := t.TempDir()
	tokPath := writeFile(t, dir, "influx-token", "from-file")
	p := writeFile(t, dir, "config.yaml", `
site:
  id: "test"
  devices_namespace: "devices_test"
  floorplan_namespace: "floorplan_test"
influx:
  token: "from-inline"
  token_file: "`+tokPath+`"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Influx.Token != "from-inline" {
		t.Errorf("token = %q, want inline value", cfg.Influx.Token)
	}
}

func TestLoadMissingTokenFileErrors(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml", `
influx:
  token_file: "`+filepath.Join(dir, "does-not-exist")+`"
`)
	if _, err := Load(p); err == nil {
		t.Fatal("Load: expected error for missing token file, got nil")
	}
}

func TestLoadInvalidTimezoneErrors(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml", `
house:
  timezone: "Mars/Phobos"
`)
	if _, err := Load(p); err == nil {
		t.Fatal("Load: expected error for invalid timezone, got nil")
	}
}

func TestLoadValidTimezonePasses(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml", `
site:
  id: "test"
  devices_namespace: "devices_test"
  floorplan_namespace: "floorplan_test"
house:
  timezone: "Europe/London"
`)
	if _, err := Load(p); err != nil {
		t.Fatalf("Load: unexpected error for valid timezone: %v", err)
	}
}

// A timezone whose UTC offset is not always a whole number of hours is
// refused at load (issue #49). /series windows sub-day intervals on the UTC
// grid and builds 6h and slot-priced days by folding 1h/30m UTC buckets onto
// local boundaries; both are only right when every local hour boundary is a
// UTC hour boundary. Asia/Kolkata (+05:30) and Asia/Kathmandu (+05:45) would
// put every local boundary mid-bucket — misattributed energy that reads as
// data — so they fail loudly at startup instead.
func TestLoadRejectsNonWholeHourTimezone(t *testing.T) {
	for _, tz := range []string{"Asia/Kolkata", "Asia/Kathmandu", "Australia/Adelaide", "Australia/Lord_Howe"} {
		dir := t.TempDir()
		p := writeFile(t, dir, "config.yaml", "\nhouse:\n  timezone: \""+tz+"\"\n")
		_, err := Load(p)
		if err == nil {
			t.Errorf("Load(%s): expected error for a non-whole-hour zone, got nil", tz)
			continue
		}
		if !strings.Contains(err.Error(), tz) {
			t.Errorf("Load(%s): error should name the zone: %v", tz, err)
		}
	}
}

// Whole-hour zones, with and without DST, are accepted.
func TestLoadAcceptsWholeHourTimezones(t *testing.T) {
	for _, tz := range []string{"Europe/London", "UTC", "America/New_York", "Europe/Berlin", "Asia/Tokyo"} {
		if err := checkWholeHourZone(tz); err != nil {
			t.Errorf("checkWholeHourZone(%s): %v", tz, err)
		}
	}
}

func TestLoadMissingFileErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("Load: expected error for missing config file, got nil")
	}
}
