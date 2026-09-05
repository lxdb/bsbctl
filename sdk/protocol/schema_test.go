package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type schemaFixture struct {
	Name       string          `json:"name"`
	Definition string          `json:"definition"`
	Value      json.RawMessage `json:"value"`
}

func TestProtocolV1SchemaFixtures(t *testing.T) {
	root := filepath.Join("..", "..", "docs", "protocol", "v1")
	schemaData, err := os.ReadFile(filepath.Join(root, "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaData))
	if err != nil {
		t.Fatalf("decode schema: %v", err)
	}

	covered := make(map[string]bool)
	for _, set := range []struct {
		name  string
		valid bool
	}{
		{"positive", true},
		{"negative", false},
	} {
		fixtureData, err := os.ReadFile(filepath.Join(root, "fixtures", set.name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var fixtures []schemaFixture
		if err := DecodeStrict(fixtureData, &fixtures); err != nil {
			t.Fatalf("decode %s fixtures: %v", set.name, err)
		}
		for _, fixture := range fixtures {
			t.Run(set.name+"/"+fixture.Name, func(t *testing.T) {
				compiler := jsonschema.NewCompiler()
				compiler.DefaultDraft(jsonschema.Draft2020)
				if err := compiler.AddResource("schema.json", document); err != nil {
					t.Fatal(err)
				}
				reference := fmt.Sprintf(`{"$schema":"https://json-schema.org/draft/2020-12/schema","$ref":"schema.json#/$defs/%s"}`, fixture.Definition)
				refDocument, err := jsonschema.UnmarshalJSON(bytes.NewBufferString(reference))
				if err != nil {
					t.Fatal(err)
				}
				if err := compiler.AddResource("fixture-schema.json", refDocument); err != nil {
					t.Fatal(err)
				}
				compiled, err := compiler.Compile("fixture-schema.json")
				if err != nil {
					t.Fatal(err)
				}
				value, err := jsonschema.UnmarshalJSON(bytes.NewReader(fixture.Value))
				if err != nil {
					t.Fatalf("decode fixture value: %v", err)
				}
				schemaErr := compiled.Validate(value)
				goErr := validateFixtureWithGo(fixture.Definition, fixture.Value)
				if set.valid && schemaErr != nil {
					t.Fatalf("valid schema fixture: %v", schemaErr)
				}
				if set.valid && goErr != nil {
					t.Fatalf("valid Go fixture: %v", goErr)
				}
				if !set.valid && schemaErr == nil {
					t.Fatal("invalid fixture passed schema validation")
				}
				if !set.valid && goErr == nil {
					t.Fatal("invalid fixture passed Go validation")
				}
				covered[set.name+":"+fixture.Definition] = true
			})
		}
	}

	methodShapes := []string{
		"InitializeRequest", "InitializeResult", "ReplaceInstancesRequest", "SessionStartRequest", "SessionEndRequest",
		"SessionInputRequest", "SessionInputResult", "OperationRequest", "OperationResult", "PublishRequest", "WithdrawRequest", "CheckpointRequest", "CompleteSessionRequest",
		"SessionExecutionRequest",
		"HealthResult", "LogNotification", "MetricNotification", "ErrorData",
	}
	for _, definition := range methodShapes {
		for _, set := range []string{"positive", "negative"} {
			if !covered[set+":"+definition] {
				t.Errorf("%s fixtures do not cover %s", set, definition)
			}
		}
	}
}

func validateFixtureWithGo(definition string, data []byte) error {
	decodeAndValidate := func(target any, validate func() error) error {
		if err := DecodeStrict(data, target); err != nil {
			return err
		}
		return validate()
	}

	switch definition {
	case "InitializeRequest":
		var value InitializeRequest
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "InitializeResult":
		var value InitializeResult
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "ReplaceInstancesRequest":
		var value ReplaceInstancesRequest
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "SessionStartRequest":
		var value SessionStartRequest
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "SessionEndRequest":
		var value SessionEndRequest
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "SessionInputRequest":
		var value SessionInputRequest
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "SessionInputResult":
		var value SessionInputResult
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "OperationRequest":
		var value OperationRequest
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "OperationResult":
		var value OperationResult
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "PublishRequest":
		var value PublishRequest
		fixtureNow := time.Date(2026, 8, 28, 17, 59, 0, 0, time.UTC)
		return decodeAndValidate(&value, func() error { return value.Observation.Validate(fixtureNow) })
	case "WithdrawRequest":
		var value WithdrawRequest
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "CheckpointRequest":
		var value CheckpointRequest
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "CompleteSessionRequest":
		var value CompleteSessionRequest
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "SessionExecutionRequest":
		var value SessionExecutionRequest
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "HealthResult":
		var value HealthResult
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "LogNotification":
		var value LogNotification
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "MetricNotification":
		var value MetricNotification
		return decodeAndValidate(&value, func() error { return value.Validate() })
	case "ErrorData":
		var value ErrorData
		return decodeAndValidate(&value, func() error { return value.Validate() })
	default:
		return fmt.Errorf("no Go validator for schema definition %q", definition)
	}
}
