package config

import (
	"strings"
	"testing"
)

// Unset means enforce: warn ran in production from 30 Oct 2026 and logged no
// caller that enforcing would lock out (#59). warn and off stay available as
// an explicit setting, for a rollback without a rebuild.
func TestLoadDefaultsAudienceCheckToEnforce(t *testing.T) {
	cfg, err := Load(writeConfig(t, "http:\n  listen: \":8585\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.AudienceCheck != AudienceEnforce {
		t.Errorf("audience_check = %q, want %q", cfg.HTTP.AudienceCheck, AudienceEnforce)
	}
}

func TestLoadAcceptsEachAudienceCheckMode(t *testing.T) {
	for _, mode := range []string{AudienceWarn, AudienceEnforce, AudienceOff} {
		cfg, err := Load(writeConfig(t, "http:\n  audience_check: "+mode+"\n"))
		if err != nil {
			t.Fatalf("%s: Load: %v", mode, err)
		}
		if cfg.HTTP.AudienceCheck != mode {
			t.Errorf("audience_check = %q, want %q", cfg.HTTP.AudienceCheck, mode)
		}
	}
}

// A misspelt mode must not quietly mean "off".
func TestLoadRefusesAnUnknownAudienceCheck(t *testing.T) {
	_, err := Load(writeConfig(t, "http:\n  audience_check: enforcing\n"))
	if err == nil || !strings.Contains(err.Error(), "audience_check") {
		t.Fatalf("Load error = %v, want one naming http.audience_check", err)
	}
}
