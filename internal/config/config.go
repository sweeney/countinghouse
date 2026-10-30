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
	Prices       PricesConfig       `yaml:"prices"`

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

	// The devices namespace is deliberately NOT settable here. It is the pointer
	// that decides whether any answer is right, and a stale local copy would bill
	// another property's devices while the service looked entirely healthy — so it
	// comes from the shared `sites` namespace or the instance does not start.

	// EnergyAgreementsNamespace names this site's dated-tariff namespace (conventionally
	// "energy_agreements"). It sits here, beside the other two per-property
	// pointers, because a tariff is a property of the SITE — a second property is
	// generally on a different tariff, in a different region, at different rates.
	//
	// When EMPTY the legacy `energy_tariffs` document is used instead, which is
	// what makes the migration opt-in: a deployment can take this binary with no
	// config change and behave exactly as before.
	//
	// The two are never merged. Two tariff documents disagreeing about what a kWh
	// cost has no safe resolution, so exactly one is authoritative and both
	// /healthz and GET /tariffs report which. When this IS set the namespace is
	// required: per the cold-start rule, naming one that has never been fetched
	// aborts startup rather than silently falling back to the legacy rate, because
	// falling back would price a half-hourly tariff at a flat number.
	EnergyAgreementsNamespace string `yaml:"energy_agreements_namespace"`

	// FloorplanNamespace is the config namespace holding this site's floor and room
	// records — the same document greenhouse reads, published by /floors and /rooms
	// and used to label grouped series with names instead of ids.
	//
	// REQUIRED, like DevicesNamespace. It is a document that either exists or does
	// not, and an instance that names none cannot say so: /floors and /rooms answer
	// with ids where names belong and every storey order null, which is exactly what
	// a floorplan publishing nothing would produce. The failure therefore surfaces as
	// a chart legend somebody eventually notices looks wrong, rather than at startup.
	// Naming it is one line; the alternative is a silence that reads as data.
	//
	// This is stricter than greenhouse, which treats the same namespace as optional.
	// Deliberately: both services must be given the name to serve names at all, and
	// refusing at startup is the only place the omission is visible.
	//
	// The RUNTIME remains fail-open — a namespace that is named but unfetchable keeps
	// the last-known records and degrades to unknown names rather than refusing to
	// serve. Requiring the name asserts that an operator declared where the records
	// live, not that the config service is up.
	FloorplanNamespace string `yaml:"floorplan_namespace"`
}

// UnmarshalYAML accepts either the block form or a bare id:
//
//	site: home
//	site:
//	  id: home
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

	// AudienceCheck is how incoming tokens' `aud` claim is checked against
	// "countinghouse": AudienceWarn (the default) accepts a token that does not
	// name it and logs the caller, AudienceEnforce refuses it with a 401, and
	// AudienceOff does neither. Without a check, a token minted for any sibling
	// service (mqttauth, config) reads countinghouse.
	//
	// Warn is the default so the check can ship before anyone has read the
	// warnings: every one names a caller that enforcing would lock out. Moving
	// to enforce is then a reviewed change to this default, not an edit of the
	// host's root-owned config file.
	AudienceCheck string `yaml:"audience_check"`
}

// The modes of HTTPConfig.AudienceCheck.
const (
	AudienceWarn    = "warn"
	AudienceEnforce = "enforce"
	AudienceOff     = "off"
)

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

// PricesConfig locates the half-hourly price archive.
//
// This is local bootstrap config rather than remote for an unavoidable reason: it
// is where the service keeps the data it needs in order to answer anything, so it
// cannot itself be fetched.
type PricesConfig struct {
	// DBPath is the SQLite file holding the archive. Empty DISABLES the collector
	// entirely, which is correct for a deployment whose agreements are all
	// flat-rate — it has no half-hourly prices to keep.
	//
	// It is not optional once any agreement is half-hourly: see
	// CheckArchiveRequired, which refuses that combination at startup rather than
	// letting the service look healthy while being unable to price anything after
	// the switchover.
	//
	// The parent directory must exist and be writable by the service user. The
	// file is created mode 0600, along with the -wal and -shm files SQLite keeps
	// beside it.
	DBPath string `yaml:"db_path"`

	// OctopusBaseURL is the supplier API root, defaulted by Default() and
	// overridable so a local or staging deployment can be pointed at a stand-in.
	//
	// The default lives in Default() rather than inside the octopus client, which
	// deliberately refuses to construct without a base URL — so a misconfigured
	// test cannot quietly reach the live API. The default belongs at the edge,
	// where it is visible in one place.
	OctopusBaseURL string `yaml:"octopus_base_url"`

	// Backup sends the archive to Cloudflare R2. Empty means backups are off,
	// which is correct for development and for any deployment with no archive; a
	// PARTIAL block is refused at startup rather than run as "off". The archive is
	// rebuildable from the supplier today, and the whole reason to keep it is the
	// day that stops being true — so this is the clause that makes keeping it mean
	// something.
	Backup BackupConfig `yaml:"backup"`
}

// DefaultOctopusBaseURL is the supplier's public API root. The price and standing
// charge endpoints beneath it need no authentication.
const DefaultOctopusBaseURL = "https://api.octopus.energy/v1"

// Enabled reports whether a price archive is configured, and therefore whether
// the collector should run.
func (p PricesConfig) Enabled() bool { return p.DBPath != "" }

// HouseConfig holds house-wide settings.
type HouseConfig struct {
	// Timezone names a tz database location (e.g. "Europe/London") used to
	// resolve query windows (today/week/month) to half-open ranges. Empty
	// means UTC; "Local" uses the host time zone. Load() rejects values
	// that time.LoadLocation cannot resolve (typo, missing tzdata) with a
	// clear error so operators see the diagnostic at startup.
	//
	// It also rejects a zone whose UTC offset is not ALWAYS a whole number of
	// hours (e.g. Asia/Kolkata, +05:30); see checkWholeHourZone.
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

// wholeHourCheckFrom and wholeHourCheckUntil bound the span checkWholeHourZone
// inspects: the span a /series window can be asked about AND find data in.
//
// The lower bound is the question that matters, and it is about data, not
// tzdata: a half-hour offset can only misplace energy in a window that has
// readings. This service's data begins in 2026 (statehouse's bucket holds
// nothing earlier), so 2020 is comfortably before any reading. It used to be
// 1970, which refused zones that have been whole-hour for years over offsets
// no query can reach — Asia/Singapore (+07:30 until 1982), America/Caracas
// (−04:30 until 2016), Asia/Pyongyang (+08:30 until 2018), Africa/Monrovia
// (−00:44:30 until 1972) — with an error calling them unsupported (PR #50
// review). A fixed date rather than "now minus a look-back" because config
// loading takes no clock, and because a zone's support should not change on
// its own while the service runs.
//
// The upper bound is well past any window this service will be asked about;
// beyond the last tzdata transition Go extrapolates the zone's current rule.
var (
	wholeHourCheckFrom  = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	wholeHourCheckUntil = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// checkWholeHourZone refuses a timezone whose UTC offset is not a whole number
// of hours at every point from wholeHourCheckFrom to wholeHourCheckUntil
// (issue #49).
//
// /series windows every sub-day interval on the UTC grid rather than asking
// Influx for local windows, because Influx stretches local sub-day windows
// across the autumn repeated hour. It then builds 6h buckets, and slot-priced
// 1d buckets, by folding 1h/30m UTC buckets onto local boundaries. Both are
// exact only when every local hour boundary is also a UTC hour boundary, which
// is precisely "the offset is always a whole number of hours". In a +05:30 or
// +05:45 zone, hourly buckets would be labelled at half past and every local
// 6h/day boundary would fall mid-bucket, misattributing energy between
// buckets — a wrong answer that looks like a right one. Refusing at startup is
// the loud version of that.
//
// It walks the zone's actual transitions (ZoneBounds) rather than sampling, so
// a short-lived offset cannot slip between samples. Australia/Lord_Howe, whose
// DST shift is 30 minutes, is caught on its +10:30 standard time.
//
// A zone whose clocks change AT a 6h or day boundary (America/Santiago and
// America/Havana go forward at local midnight) is whole-hour and accepted: the
// series axes resolve a skipped boundary to the transition instant, which is
// itself on the UTC hour (see energy.resolveWall).
func checkWholeHourZone(loc *time.Location) error {
	for t := wholeHourCheckFrom.In(loc); t.Before(wholeHourCheckUntil); {
		name, off := t.Zone()
		if off%3600 != 0 {
			return fmt.Errorf("house.timezone %q has a UTC offset that is not a whole number of hours "+
				"(%s, %+.2fh, from %s); /series buckets are built on the UTC hour grid and would "+
				"misplace energy across every local boundary, so this zone is not supported (issue #49)",
				loc.String(), name, float64(off)/3600, t.UTC().Format(time.RFC3339))
		}
		_, end := t.ZoneBounds()
		if end.IsZero() || !end.After(t) {
			break // no further transitions: this offset holds forever
		}
		t = end
	}
	return nil
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
		Prices: PricesConfig{
			OctopusBaseURL: DefaultOctopusBaseURL,
			// Only the hour is defaulted. Every other backup field left unset means
			// "backups off", and filling any of them in would turn a deployment that
			// never asked for backups into one that half-asks. The hour is different:
			// 0 is a legitimate midnight, so it cannot be distinguished from unset
			// later and has to be settled here.
			Backup: BackupConfig{Hour: DefaultBackupHour},
		},
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
	switch cfg.HTTP.AudienceCheck {
	case "":
		cfg.HTTP.AudienceCheck = AudienceWarn
	case AudienceWarn, AudienceEnforce, AudienceOff:
	default:
		// A misspelt mode must not quietly mean "off".
		return cfg, fmt.Errorf("http.audience_check %q: want %q, %q or %q",
			cfg.HTTP.AudienceCheck, AudienceWarn, AudienceEnforce, AudienceOff)
	}
	if cfg.Influx.Token == "" && cfg.Influx.TokenFile != "" {
		tok, err := os.ReadFile(cfg.Influx.TokenFile)
		if err != nil {
			return cfg, fmt.Errorf("read influx token: %w", err)
		}
		cfg.Influx.Token = string(trimTrailingNewline(tok))
	}
	if cfg.Prices.Backup.SecretAccessKey == "" && cfg.Prices.Backup.SecretAccessKeyFile != "" {
		sec, err := os.ReadFile(cfg.Prices.Backup.SecretAccessKeyFile)
		if err != nil {
			// The path is named; the contents never are, here or anywhere else.
			return cfg, fmt.Errorf("read r2 secret from %s: %w", cfg.Prices.Backup.SecretAccessKeyFile, err)
		}
		cfg.Prices.Backup.SecretAccessKey = string(trimTrailingNewline(sec))
		if cfg.Prices.Backup.SecretAccessKey == "" {
			return cfg, fmt.Errorf("r2 secret file %s is empty: an empty secret fails every "+
				"upload with an auth error rather than at startup", cfg.Prices.Backup.SecretAccessKeyFile)
		}
	}
	if err := cfg.Prices.Backup.Validate(cfg.Prices.DBPath); err != nil {
		return cfg, err
	}
	if cfg.House.Timezone != "" {
		loc, err := time.LoadLocation(cfg.House.Timezone)
		if err != nil {
			return cfg, fmt.Errorf("parse house.timezone %q: %w", cfg.House.Timezone, err)
		}
		if err := checkWholeHourZone(loc); err != nil {
			return cfg, err
		}
	}
	cfg.warnings = siteWarnings(cfg.Site)

	// No namespace requirement here any more, and deliberately not in the
	// base_url=="" branch either — that is the one case where NOTHING is fetched,
	// so neither pointer is ever read and requiring them would refuse a config
	// over values it then ignores. (An earlier version of this did exactly that.)
	//
	// The requirement lives in ResolveSiteNamespaces instead, which runs precisely
	// when there IS a config service and therefore when the pointers matter. An
	// instance with no config service serves empty snapshots by explicit choice,
	// and says so loudly at startup.
	return cfg, nil
}

// siteWarnings reports a half-filled site block that is legal and works.
//
// Only the mirror case is left: a namespace with no id fetches the right devices and
// serves correct numbers, and costs only the ability to say which property it serves.
// Refusing to start over that would take a working instance down to fix a label. The
// other half — an id with no namespace — is no longer a warning but an error, since
// there is nothing left for it to fall back to.
func siteWarnings(s SiteConfig) []string {
	if s.ID != "" {
		return nil
	}
	// The remaining local pointers are fallbacks. Setting one without an id is
	// still the mirror case: it would fetch the right documents while being unable
	// to say which property they describe.
	for _, c := range []struct{ field, value string }{
		{"floorplan_namespace", s.FloorplanNamespace},
		{"energy_agreements_namespace", s.EnergyAgreementsNamespace},
	} {
		if c.value != "" {
			return []string{fmt.Sprintf(
				"%s %q is set but the site has no id, so this instance cannot report "+
					"which property it serves — and without an id the sites namespace "+
					"cannot be resolved at all", c.field, c.value)}
		}
	}
	return nil
}

func trimTrailingNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
