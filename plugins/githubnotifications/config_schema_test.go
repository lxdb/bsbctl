package githubnotifications

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/lxdb/bsbctl/internal/configschema"
)

func TestConfigurationSchemaMatchesRuntimeAndShippedExample(t *testing.T) {
	t.Parallel()
	schema, err := configschema.Compile(ConfigSchema)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate([]byte(`{}`)); err != nil {
		t.Fatalf("empty unconfigured default: %v", err)
	}

	exampleData, err := os.ReadFile("../../examples/github-notifications.json")
	if err != nil {
		t.Fatal(err)
	}
	var example struct {
		Config json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(exampleData, &example); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(example.Config); err != nil {
		t.Fatalf("shipped configured example: %v", err)
	}
	if _, err := DecodeConfig(example.Config); err != nil {
		t.Fatalf("runtime rejected shipped configured example: %v", err)
	}

	for name, raw := range map[string][]byte{
		"partial configured form": []byte(`{"label":"GH"}`),
		"unknown field":           []byte(`{"repositories":[],"unknown":true}`),
		"single-dot repository":   []byte(`{"repositories":[{"name":"lxdb/.","alias":"DOT"}]}`),
		"double-dot repository":   []byte(`{"repositories":[{"name":"lxdb/..","alias":"DOTS"}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := schema.Validate(raw); err == nil {
				t.Fatal("schema accepted invalid configured form")
			}
		})
	}

	// JSON Schema cannot make repository names or aliases unique by one object
	// property. The strict runtime decoder remains the relational owner.
	for name, relational := range map[string][]byte{
		"case-insensitive duplicate repository": []byte(`{"repositories":[{"name":"lxdb/bsbctl","alias":"ONE"},{"name":"LXDB/BSBCTL","alias":"TWO"}]}`),
		"duplicate alias":                       []byte(`{"repositories":[{"name":"lxdb/bsbctl","alias":"ONE"},{"name":"lxdb/busylib","alias":"ONE"}]}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := schema.Validate(relational); err != nil {
				t.Fatalf("schema should leave relational validation to runtime: %v", err)
			}
			if _, err := DecodeConfig(relational); err == nil {
				t.Fatal("runtime accepted invalid relational configuration")
			}
		})
	}
}
