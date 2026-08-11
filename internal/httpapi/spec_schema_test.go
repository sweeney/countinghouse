package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// spec_test.go checks that routes and spec paths agree. Nothing checked that the
// schemas agree with what the handlers actually send, and they had drifted: the
// catalog schema listed the deprecated `location` as required and omitted `room`
// entirely, so a generated client would have treated the field the migration
// introduces as optional and the one it retires as guaranteed.
func TestDeviceCatalogRequiredFieldsMatchTheResponse(t *testing.T) {
	spec, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var required string
	for _, line := range strings.Split(string(spec), "\n") {
		if strings.Contains(line, "required: [id, display_name") {
			required = line
		}
	}
	if required == "" {
		t.Fatal("no DeviceCatalogEntry required list found in the spec")
	}

	s, _ := dataSetup(t)
	s.Config = fakeConfig{devices: roomDevices(), tariffs: testTariffs()}
	w := doGET(t, s, "/devices")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}

	var resp struct {
		Devices []map[string]json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Devices) == 0 {
		t.Fatal("no devices in the catalog")
	}

	for _, field := range []string{"id", "display_name", "class", "capabilities"} {
		if !strings.Contains(required, field) {
			t.Errorf("spec omits %q from required, but every response carries it", field)
		}
	}
	// The field the migration introduces must be required; the one it retires must
	// only be required while the handler still sends it.
	_, sendsRoom := resp.Devices[0]["room"]
	_, sendsLocation := resp.Devices[0]["location"]

	if sendsRoom && !strings.Contains(required, "room") {
		t.Error("the response carries `room` but the spec does not require it")
	}
	if !sendsLocation && strings.Contains(required, "location") {
		t.Error("the spec requires `location` but the response no longer carries it")
	}
}
