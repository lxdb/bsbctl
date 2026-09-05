package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestBuildPlanGroupsEnabledInstancesAndResolvesSecretReferences(t *testing.T) {
	t.Parallel()
	document := config.Document{
		Version: config.CurrentVersion, Generation: 9,
		Plugins: map[string]config.Plugin{
			"plugin": {
				ID: "plugin", Version: "1", Executable: "/plugin",
				ProtocolVersion: protocol.Version,
				ExecutionModes:  []protocol.ExecutionMode{protocol.ExecutionModeResident},
				Channels:        []protocol.Channel{{ID: "main"}},
			},
		},
		Apps: map[string]config.App{
			"enabled": {
				ID: "enabled", PluginID: "plugin", Generation: 3, Enabled: true, Config: json.RawMessage(`{"threshold":80}`),
				Secrets:  map[string]string{"token": "keychain://bsbctl/app/token"},
				Policies: map[string]presentation.PolicyConfig{"main": {Policy: presentation.PolicyWhenRelevant}},
			},
			"disabled": {
				ID: "disabled", PluginID: "plugin", Generation: 3, Enabled: false, Config: json.RawMessage(`{}`),
				Policies: map[string]presentation.PolicyConfig{"main": {Policy: presentation.PolicyWhenRelevant}},
			},
		},
	}
	resolver := SecretResolverFunc(func(_ context.Context, reference string) (string, error) {
		if reference != "keychain://bsbctl/app/token" {
			t.Fatalf("Resolve reference = %q", reference)
		}
		return "resolved-secret", nil
	})

	plan, err := BuildPlan(context.Background(), document, resolver)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	specs, generations := plan.Specs, plan.Generations
	if len(specs) != 1 || len(specs[0].Instances) != 1 {
		t.Fatalf("specs = %#v", specs)
	}
	if specs[0].ProtocolVersion != protocol.Version {
		t.Fatalf("spec protocol version = %q", specs[0].ProtocolVersion)
	}
	instance := specs[0].Instances[0]
	if instance.ID != "enabled" || instance.Generation != 3 || instance.Secrets["token"] != "resolved-secret" {
		t.Fatalf("instance = %#v", instance)
	}
	if generation, ok := generations.Lookup("plugin", "enabled"); !ok || generation != 3 {
		t.Fatalf("generation = %d, %v", generation, ok)
	}
	if _, ok := generations.Lookup("plugin", "disabled"); ok {
		t.Fatal("disabled instance was registered")
	}
}

func TestBuildPlanPreservesRotationSchedulingPolicy(t *testing.T) {
	document := serviceDocument(true)
	app := document.Apps["ball8"]
	app.Policies["answer"] = presentation.PolicyConfig{
		Policy: presentation.PolicyRotation, RotationIntervalMS: 30_000, RotationJitterPercent: 17,
	}
	document.Apps["ball8"] = app
	plan, err := BuildPlan(context.Background(), document, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := plan.Specs[0].Instances[0].Policies["answer"]
	if got.RotationIntervalMS != 30_000 || got.RotationJitterPercent != 17 {
		t.Fatalf("rotation policy = %#v", got)
	}
}

func TestBuildPlanContainsReadyAppsAndFaultContainsSecretFailure(t *testing.T) {
	t.Parallel()
	document := config.Document{
		Version: config.CurrentVersion, Generation: 4,
		Plugins: map[string]config.Plugin{"plugin": {
			ID: "plugin", Version: "1", Executable: "/plugin",
			ProtocolVersion: protocol.Version,
			ExecutionModes:  []protocol.ExecutionMode{protocol.ExecutionModeResident},
			Channels:        []protocol.Channel{{ID: "main"}},
		}},
		Apps: map[string]config.App{
			"ready":   appWithSecret("ready", "plugin", "keychain://bsbctl/ready/token"),
			"pending": appWithSecret("pending", "plugin", "keychain://bsbctl/pending/token"),
		},
	}
	resolver := SecretResolverFunc(func(_ context.Context, reference string) (string, error) {
		if reference == "keychain://bsbctl/pending/token" {
			return "", errors.New("account customer@example.test at secret.invalid was denied")
		}
		return "ready-value", nil
	})

	plan, err := BuildPlan(context.Background(), document, resolver)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Specs) != 1 || len(plan.Specs[0].Instances) != 1 || plan.Specs[0].Instances[0].ID != "ready" {
		t.Fatalf("specs = %#v, want only ready instance", plan.Specs)
	}
	if _, ok := plan.Generations.Lookup("plugin", "pending"); ok {
		t.Fatal("pending generation was admitted")
	}
	if got := readinessByID(plan.Readiness)["pending"]; got.Phase != AppSecretPending || got.LastErrorCode != secretUnavailableCode {
		t.Fatalf("pending readiness = %#v", got)
	}
}

func TestBuildPlanRejectsStructurallyInvalidDocumentBeforeResolving(t *testing.T) {
	t.Parallel()
	document := serviceDocument(true)
	document.Apps["ball8"] = config.App{ID: "ball8", PluginID: "missing", Enabled: true}
	called := false
	_, err := BuildPlan(context.Background(), document, SecretResolverFunc(func(context.Context, string) (string, error) {
		called = true
		return "", nil
	}))
	if err == nil {
		t.Fatal("BuildPlan accepted invalid document")
	}
	if called {
		t.Fatal("resolver called before structural validation")
	}
}

func TestGenerationsUsesStructuredTupleKeys(t *testing.T) {
	values := Generations{values: map[generationKey]uint64{
		{pluginID: "plugin/a", instanceID: "b"}: 1,
		{pluginID: "plugin", instanceID: "a/b"}: 2,
	}}
	if first, ok := values.Lookup("plugin/a", "b"); !ok || first != 1 {
		t.Fatalf("first generation = %d, %v", first, ok)
	}
	if second, ok := values.Lookup("plugin", "a/b"); !ok || second != 2 {
		t.Fatalf("second generation = %d, %v", second, ok)
	}
}

func appWithSecret(id, pluginID, reference string) config.App {
	return config.App{
		ID: id, PluginID: pluginID, Generation: 4, Enabled: true, Config: json.RawMessage(`{}`),
		Secrets:  map[string]string{"token": reference},
		Policies: map[string]presentation.PolicyConfig{"main": {Policy: presentation.PolicyWhenRelevant}},
	}
}

func readinessByID(values []AppReadiness) map[string]AppReadiness {
	result := make(map[string]AppReadiness, len(values))
	for _, value := range values {
		result[value.AppID] = value
	}
	return result
}
