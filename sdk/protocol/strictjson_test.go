package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestDecodeRejectsAmbiguousOrUnexpectedJSON(t *testing.T) {
	t.Parallel()

	type document struct {
		Name   string         `json:"name"`
		Nested map[string]any `json:"nested"`
	}

	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "duplicate top-level name", raw: `{"name":"first","name":"second"}`},
		{name: "duplicate nested name", raw: `{"name":"value","nested":{"count":1,"count":2}}`},
		{name: "unknown field", raw: `{"name":"value","unexpected":true}`},
		{name: "second value", raw: `{"name":"value"} {"name":"second"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var decoded document
			if err := DecodeStrict(json.RawMessage(test.raw), &decoded); err == nil {
				t.Fatalf("Decode(%q) succeeded, want rejection", test.raw)
			}
		})
	}
}

func TestDecodePreservesNumbersInUntypedValues(t *testing.T) {
	t.Parallel()

	var decoded struct {
		Value any `json:"value"`
	}
	if err := DecodeStrict(json.RawMessage(`{"value":9007199254740993}`), &decoded); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	value, ok := decoded.Value.(json.Number)
	if !ok || value.String() != "9007199254740993" {
		t.Fatalf("value = %#v, want exact json.Number", decoded.Value)
	}
}

func TestDecodeAcceptsOneStrictValue(t *testing.T) {
	t.Parallel()

	var decoded struct {
		Name   string         `json:"name"`
		Nested map[string]any `json:"nested"`
	}
	if err := DecodeStrict(json.RawMessage(`{"name":"value","nested":{"count":2}}`), &decoded); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if decoded.Name != "value" || decoded.Nested["count"] != json.Number("2") {
		t.Fatalf("decoded = %#v", decoded)
	}
}

func TestStrictJSONRejectsCaseAliases(t *testing.T) {
	for _, input := range []string{`{"ID":"main","generation":1}`, `{"id":"main","Generation":1}`, `{"id":"first","ID":"second","generation":1}`} {
		var result InstanceRef
		if err := DecodeStrict([]byte(input), &result); err == nil {
			t.Errorf("case alias accepted: %s -> %+v", input, result)
		}
	}
	for _, input := range []string{`{"instances":[{"id":"main","GENERATION":1,"config":{}}]}`, `{"Instances":[]}`} {
		var result ReplaceInstancesRequest
		if err := DecodeStrict([]byte(input), &result); err == nil {
			t.Errorf("nested alias accepted: %s", input)
		}
	}
	var metric MetricNotification
	if err := DecodeStrict([]byte(`{"Name":"requests","value":0,"unit":"count"}`), &metric); err == nil {
		t.Fatal("custom metric wire decoder accepted a case alias")
	}
}

func TestStrictJSONPreservesOpaquePayloadsAndTypedMapValues(t *testing.T) {
	type value struct {
		Label string `json:"label,omitzero"`
	}
	type record struct {
		Values  map[string]value `json:"values"`
		Raw     json.RawMessage  `json:"raw"`
		At      time.Time        `json:"at"`
		Number  any              `json:"number"`
		Ignored string           `json:"-"`
	}
	input := []byte(`{"values":{"A":{"label":"upper"},"a":{}},"raw":{"X":1,"x":2},"at":"2026-09-05T00:00:00Z","number":9007199254740993}`)
	var got record
	if err := DecodeStrict(input, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Values) != 2 || got.Values["A"].Label != "upper" || string(got.Raw) != `{"X":1,"x":2}` || got.Number != json.Number("9007199254740993") || got.At.IsZero() {
		t.Fatalf("opaque data or exact numbers changed: %+v", got)
	}
	for _, input := range []string{`{"values":{"A":{"Label":"alias"}}}`, `{"raw":{"x":1,"x":2}}`, `{"Ignored":"not a wire field"}`} {
		if err := DecodeStrict([]byte(input), &got); err == nil {
			t.Errorf("invalid nested input accepted: %s", input)
		}
	}
}

type strictPromoted struct {
	Value string `json:"value"`
}
type strictUnexported struct {
	Other int `json:"other"`
}
type strictTagged struct {
	Value string `json:"Value"`
}
type strictPlain struct{ Value string }

func TestStrictJSONMatchesPromotedFieldDominance(t *testing.T) {
	for _, test := range []struct {
		name, raw string
		target    any
	}{
		{"promoted", `{"value":"x","other":2}`, &struct {
			strictPromoted
			strictUnexported
		}{}},
		{"named embedding", `{"nested":{"value":"x"}}`, &struct {
			strictPromoted `json:"nested"`
		}{}},
		{"outer dominates", `{"value":7}`, &struct {
			strictPromoted
			Value int `json:"value"`
		}{}},
		{"tag dominates", `{"Value":"x"}`, &struct {
			strictTagged
			strictPlain
		}{}},
		{"omitted field accepted", `{"Value":""}`, &struct {
			Value string `json:",omitempty"`
		}{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := reflect.New(reflect.TypeOf(test.target).Elem()).Interface()
			if err := json.Unmarshal([]byte(test.raw), want); err != nil {
				t.Fatal(err)
			}
			if err := DecodeStrict([]byte(test.raw), test.target); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(test.target, want) {
				t.Fatalf("strict decode differs from canonical field dominance: got %+v want %+v", test.target, want)
			}
		})
	}
	type duplicate struct {
		Value string
	}
	var ambiguous struct {
		strictPlain
		duplicate
	}
	if err := DecodeStrict([]byte(`{"Value":"ambiguous"}`), &ambiguous); err == nil {
		t.Fatal("ambiguous promoted field accepted")
	}
}
