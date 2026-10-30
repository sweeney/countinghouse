package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// audienceSetup is authSetup with an audience mode and a captured log.
func audienceSetup(t *testing.T, mode string) (h http.Handler, logs *bytes.Buffer, mint func(typ string, aud any) string) {
	t.Helper()
	priv := genTestKey(t)
	kid := "testkey"
	fakeID := fakeJWKSServer(t, &priv.PublicKey, kid)
	s := setup(t)
	s.IdentityURL = fakeID.URL
	s.AudienceCheck = mode
	logs = &bytes.Buffer{}
	s.Logger = slog.New(slog.NewTextHandler(logs, nil))
	h = s.authMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mint = func(typ string, aud any) string {
		c := jwt.MapClaims{"iss": fakeID.URL, "exp": time.Now().Add(15 * time.Minute).Unix()}
		if typ == "at+jwt" {
			c["sub"], c["client_id"], c["scope"] = "svc-abc", "energy-tariff-screen", "energy.read"
		} else {
			c["sub"], c["act"] = "user-abc", true
		}
		if aud != nil {
			c["aud"] = aud
		}
		return signToken(t, priv, kid, typ, c)
	}
	return h, logs, mint
}

const audienceWarning = "token would be rejected by audience enforcement"

func TestAudienceWarnAcceptsAWrongAudienceAndLogsIt(t *testing.T) {
	for _, typ := range []string{"JWT", "at+jwt"} {
		for _, aud := range []any{nil, []string{"mqttauth"}} {
			h, logs, mint := audienceSetup(t, "warn")
			if w := doAuth(h, mint(typ, aud)); w.Code != http.StatusOK {
				t.Errorf("%s aud=%v: want 200 in warn mode, got %d", typ, aud, w.Code)
			}
			if !strings.Contains(logs.String(), audienceWarning) || !strings.Contains(logs.String(), Audience) {
				t.Errorf("%s aud=%v: want a warning naming %q, log was:\n%s", typ, aud, Audience, logs)
			}
		}
	}
}

func TestAudienceWarnIsQuietForTheRightAudience(t *testing.T) {
	h, logs, mint := audienceSetup(t, "warn")
	if w := doAuth(h, mint("JWT", []string{"config", Audience, "statehouse"})); w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if strings.Contains(logs.String(), audienceWarning) {
		t.Errorf("no warning wanted for a token naming %s, log was:\n%s", Audience, logs)
	}
}

func TestAudienceEnforceRefusesAWrongOrMissingAudience(t *testing.T) {
	for _, typ := range []string{"JWT", "at+jwt"} {
		for _, aud := range []any{nil, []string{"mqttauth"}} {
			h, _, mint := audienceSetup(t, "enforce")
			if w := doAuth(h, mint(typ, aud)); w.Code != http.StatusUnauthorized {
				t.Errorf("%s aud=%v: want 401 when enforcing, got %d", typ, aud, w.Code)
			}
		}
	}
}

func TestAudienceEnforceAcceptsTheRightAudience(t *testing.T) {
	for _, typ := range []string{"JWT", "at+jwt"} {
		h, _, mint := audienceSetup(t, "enforce")
		if w := doAuth(h, mint(typ, []string{Audience})); w.Code != http.StatusOK {
			t.Errorf("%s: want 200, got %d", typ, w.Code)
		}
	}
}

func TestAudienceOffNeitherRefusesNorLogs(t *testing.T) {
	h, logs, mint := audienceSetup(t, "off")
	if w := doAuth(h, mint("JWT", []string{"mqttauth"})); w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if strings.Contains(logs.String(), audienceWarning) {
		t.Errorf("off must not log, log was:\n%s", logs)
	}
}

// A mode the config layer would have refused is a programming error here.
func TestAudienceUnknownModePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("want a panic for an unknown audience mode")
		}
	}()
	audienceSetup(t, "enforcing")
}
