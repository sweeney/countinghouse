package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// Whole-hour zones, with and without DST, are accepted — including zones that
// are whole-hour now but had a half-hour offset before any data this service
// can hold (PR #50 review): Singapore (+07:30 until 1982), Caracas (−04:30
// until 2016), Pyongyang (+08:30 until 2018), Monrovia (−00:44:30 until
// 1972). And zones whose clocks change at local midnight (Santiago, Havana),
// whose skipped boundaries the series axes resolve to the transition.
func TestLoadAcceptsWholeHourTimezones(t *testing.T) {
	for _, tz := range []string{"Europe/London", "UTC", "America/New_York", "Europe/Berlin", "Asia/Tokyo",
		"Asia/Singapore", "Asia/Kuala_Lumpur", "America/Caracas", "Asia/Pyongyang", "Africa/Monrovia",
		"America/Santiago", "America/Havana"} {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			t.Fatalf("LoadLocation(%s): %v", tz, err)
		}
		if err := checkWholeHourZone(loc); err != nil {
			t.Errorf("checkWholeHourZone(%s): %v", tz, err)
		}
	}
}

// The check itself, on synthetic zones so it does not depend on host tzdata:
// any offset with a sub-hour part is refused, a whole-hour one is not.
func TestCheckWholeHourZoneSynthetic(t *testing.T) {
	for _, tc := range []struct {
		off  int
		want bool // accepted
	}{
		{0, true}, {3600, true}, {-5 * 3600, true}, {14 * 3600, true},
		{5*3600 + 1800, false}, {5*3600 + 2700, false}, {-(3*3600 + 1800), false}, {60, false},
	} {
		err := checkWholeHourZone(time.FixedZone("X", tc.off))
		if got := err == nil; got != tc.want {
			t.Errorf("offset %+ds: accepted = %v, want %v (err: %v)", tc.off, got, tc.want, err)
		}
	}
}

func TestLoadMissingFileErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("Load: expected error for missing config file, got nil")
	}
}
