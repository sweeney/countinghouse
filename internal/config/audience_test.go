package config

import (
	"strings"
	"testing"
)

// Unset means warn: the check only logs, so it can ship before anyone has read
// the warnings, and switching to enforce is a reviewed change rather than a
// root edit of the host's config file.
func TestLoadDefaultsAudienceCheckToWarn(t *testing.T) {
	cfg, err := Load(writeConfig(t, "http:\n  listen: \":8585\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTP.AudienceCheck != AudienceWarn {
		t.Errorf("audience_check = %q, want %q", cfg.HTTP.AudienceCheck, AudienceWarn)
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
