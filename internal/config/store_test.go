package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestStoreReplaceIsAtomicAndGenerationChecked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewStore(path)
	first := validDocument()
	if _, err := store.ReplaceWithOutcome(0, first); err != nil {
		t.Fatalf("initial Replace: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %o, want 600", got)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Generation != 1 || !loaded.Apps["ball8"].Enabled {
		t.Fatalf("loaded document = %#v", loaded)
	}

	next := loaded
	next.Generation = 2
	app := next.Apps["ball8"]
	app.Enabled = false
	next.Apps["ball8"] = app
	if _, err := store.ReplaceWithOutcome(0, next); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale Replace error = %v, want ErrConflict", err)
	}
	unchanged, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !unchanged.Apps["ball8"].Enabled || unchanged.Generation != 1 {
		t.Fatalf("stale replace changed durable state: %#v", unchanged)
	}

	if _, err := store.ReplaceWithOutcome(1, next); err != nil {
		t.Fatalf("second Replace: %v", err)
	}
	updated, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if updated.Apps["ball8"].Enabled || updated.Generation != 2 {
		t.Fatalf("updated document = %#v", updated)
	}
}

func TestStoreRejectsUnknownAssetField(t *testing.T) {
	root := t.TempDir()
	content := []byte("icon")
	if err := os.WriteFile(filepath.Join(root, "icon.png"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	document := validDocument()
	plugin := document.Plugins["dev.bsbctl.ball8"]
	plugin.PackageRoot = root
	plugin.Assets = []assets.Declaration{{
		Source: "icon.png", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(content)), MediaType: "image/png",
	}}
	document.Plugins[plugin.ID] = plugin
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewStore(path)
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"source":"icon.png"`, `"surprise":true,"source":"icon.png"`, 1))
	if !strings.Contains(string(data), `"surprise":true`) {
		t.Fatal("fixture did not inject the unknown asset field")
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("unknown asset field was accepted")
	}
}

func TestStoreRejectsUnknownChannelPolicyField(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewStore(path)
	if _, err := store.ReplaceWithOutcome(0, validDocument()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `"policy":"interactive"`, `"policy":"interactive","surprise":true`, 1))
	if !strings.Contains(string(data), `"surprise":true`) {
		t.Fatal("fixture did not inject the unknown policy field")
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Load(); err == nil {
		t.Fatal("unknown channel policy field was accepted")
	}
}

func TestStoreUpdateCommitsOneValidatedGenerationWithoutLeakingAliases(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewStore(path)
	if _, err := store.ReplaceWithOutcome(0, validDocument()); err != nil {
		t.Fatal(err)
	}
	var callbackDocument *Document
	updated, outcome, err := store.Update(1, func(document *Document) error {
		callbackDocument = document
		app := document.Apps["ball8"]
		app.Enabled = false
		app.Config[0] = '{'
		document.Apps["ball8"] = app
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if outcome != localstate.Committed || updated.Generation != 2 || updated.Apps["ball8"].Enabled {
		t.Fatalf("updated document/outcome = %#v, %q", updated, outcome)
	}
	callbackDocument.Apps["ball8"] = App{ID: "tampered"}
	callbackDocument.Plugins["dev.bsbctl.ball8"] = Plugin{ID: "tampered"}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Generation != 2 || loaded.Apps["ball8"].ID != "ball8" || loaded.Plugins["dev.bsbctl.ball8"].ID != "dev.bsbctl.ball8" {
		t.Fatalf("callback alias changed stored document: %#v", loaded)
	}
}

func TestStoreUpdateAdvancesOnlyAffectedAppGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewStore(path)
	document := validDocument()
	sibling := document.Apps["ball8"]
	sibling.ID = "sibling"
	document.Apps[sibling.ID] = sibling
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}

	updated, _, err := store.Update(1, func(document *Document) error {
		app := document.Apps["ball8"]
		app.Enabled = false
		document.Apps[app.ID] = app
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.Apps["ball8"].Generation; got != 2 {
		t.Fatalf("changed app generation = %d, want 2", got)
	}
	if got := updated.Apps["sibling"].Generation; got != 1 {
		t.Fatalf("unchanged sibling generation = %d, want 1", got)
	}
}

func TestStoreUpdateAdvancesAllAppGenerationsForChangedPackage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewStore(path)
	document := validDocument()
	sibling := document.Apps["ball8"]
	sibling.ID = "sibling"
	document.Apps[sibling.ID] = sibling
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}

	updated, _, err := store.Update(1, func(document *Document) error {
		plugin := document.Plugins["dev.bsbctl.ball8"]
		plugin.Version = "next"
		document.Plugins[plugin.ID] = plugin
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for id, app := range updated.Apps {
		if app.Generation != 2 {
			t.Fatalf("app %q generation = %d, want 2", id, app.Generation)
		}
	}
}

func TestStoreUpdateDoesNotPersistGenerationsForRejectedMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewStore(path)
	if _, err := store.ReplaceWithOutcome(0, validDocument()); err != nil {
		t.Fatal(err)
	}

	_, outcome, err := store.Update(1, func(document *Document) error {
		app := document.Apps["ball8"]
		app.PluginID = "missing"
		document.Apps[app.ID] = app
		return nil
	})
	if err == nil || outcome != localstate.NotCommitted {
		t.Fatalf("invalid update = %q, %v", outcome, err)
	}
	loaded, loadErr := store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if loaded.Generation != 1 || loaded.Apps["ball8"].Generation != 1 || loaded.Apps["ball8"].PluginID != "dev.bsbctl.ball8" {
		t.Fatalf("rejected mutation changed durable state: %#v", loaded)
	}
}

func TestStoreUpdateScopesGenerationAcrossPlugins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewStore(path)
	document := validDocument()
	plugin := document.Plugins["dev.bsbctl.ball8"]
	plugin.ID = "dev.bsbctl.calendar"
	plugin.Executable = "/tmp/bsbctl-plugin-calendar"
	document.Plugins[plugin.ID] = plugin
	calendar := document.Apps["ball8"]
	calendar.ID = "calendar"
	calendar.PluginID = plugin.ID
	document.Apps[calendar.ID] = calendar
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		t.Fatal(err)
	}

	updated, _, err := store.Update(1, func(document *Document) error {
		app := document.Apps["calendar"]
		app.Policies["answer"] = presentation.PolicyConfig{Policy: presentation.PolicyInteractive, DevicePriority: 7}
		document.Apps[app.ID] = app
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Apps["calendar"].Generation != 2 || updated.Apps["ball8"].Generation != 1 {
		t.Fatalf("cross-plugin generations = calendar:%d ball8:%d", updated.Apps["calendar"].Generation, updated.Apps["ball8"].Generation)
	}
}

func TestStoreRejectsZeroAppGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	document := validDocument()
	app := document.Apps["ball8"]
	app.Generation = 0
	document.Apps[app.ID] = app
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(path).Load(); err == nil {
		t.Fatal("zero app generation was accepted")
	}
}

func TestDocumentRejectsZeroAppGeneration(t *testing.T) {
	document := validDocument()
	app := document.Apps["ball8"]
	app.Generation = 0
	document.Apps[app.ID] = app
	if err := document.Validate(); err == nil || !strings.Contains(err.Error(), "generation must be greater than zero") {
		t.Fatalf("zero app generation error = %v", err)
	}
}

func TestStoreUpdateSerializesGenerationCheckAndMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewStore(path)
	if _, err := store.ReplaceWithOutcome(0, validDocument()); err != nil {
		t.Fatal(err)
	}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	secondReady := make(chan struct{})
	go func() {
		_, _, err := store.Update(1, func(document *Document) error {
			close(firstStarted)
			<-releaseFirst
			app := document.Apps["ball8"]
			app.Enabled = false
			document.Apps["ball8"] = app
			return nil
		})
		firstDone <- err
	}()
	<-firstStarted
	var secondCallback atomic.Bool
	secondDone := make(chan error, 1)
	go func() {
		close(secondReady)
		_, _, err := store.Update(1, func(*Document) error {
			secondCallback.Store(true)
			return nil
		})
		secondDone <- err
	}()
	<-secondReady
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Update: %v", err)
	}
	if err := <-secondDone; !errors.Is(err, ErrConflict) {
		t.Fatalf("second Update error = %v, want ErrConflict", err)
	}
	if secondCallback.Load() {
		t.Fatal("stale update callback ran")
	}
}

func TestStoreUpdateCallbackAndValidationFailuresWriteNothing(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*Document) error
	}{
		{name: "callback", mutate: func(*Document) error { return errInjectedMutation }},
		{name: "validation", mutate: func(document *Document) error {
			document.Version = 99
			return nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			store := NewStore(path)
			if _, err := store.ReplaceWithOutcome(0, validDocument()); err != nil {
				t.Fatal(err)
			}
			_, outcome, err := store.Update(1, test.mutate)
			if err == nil || outcome != localstate.NotCommitted {
				t.Fatalf("Update outcome/error = %q, %v", outcome, err)
			}
			loaded, loadErr := store.Load()
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if loaded.Generation != 1 || !loaded.Apps["ball8"].Enabled {
				t.Fatalf("failed mutation changed durable state: %#v", loaded)
			}
		})
	}
}

var errInjectedMutation = errors.New("injected mutation failure")

func TestStoreUpdateReportsCommittedDurabilityUncertain(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewStore(path)
	if _, err := store.ReplaceWithOutcome(0, validDocument()); err != nil {
		t.Fatal(err)
	}
	store.replace = func(path string, value any) (localstate.CommitOutcome, error) {
		outcome, err := localstate.ReplaceJSON(path, value)
		if err != nil || outcome != localstate.Committed {
			t.Fatalf("seed uncertain replacement: %q, %v", outcome, err)
		}
		return localstate.CommittedDurabilityUncertain, &localstate.CommitError{
			Outcome: localstate.CommittedDurabilityUncertain,
			Op:      "sync state directory",
			Err:     errors.New("injected directory sync failure"),
		}
	}
	updated, outcome, err := store.Update(1, func(document *Document) error {
		app := document.Apps["ball8"]
		app.Enabled = false
		document.Apps["ball8"] = app
		return nil
	})
	if outcome != localstate.CommittedDurabilityUncertain || err == nil {
		t.Fatalf("Update outcome/error = %q, %v", outcome, err)
	}
	if updated.Generation != 2 || updated.Apps["ball8"].Enabled {
		t.Fatalf("committed document = %#v", updated)
	}
	loaded, loadErr := store.Load()
	if loadErr != nil || loaded.Generation != 2 || loaded.Apps["ball8"].Enabled {
		t.Fatalf("durable document = %#v, %v", loaded, loadErr)
	}
}

func TestStoreRejectsUnknownDeviceCredentialField(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{"version":1,"generation":5,"device":{"base_url":"http://192.0.2.10","unexpected_secret":"keychain://bsbctl/device/access-token"},"plugins":{},"apps":{}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := NewStore(path).LoadDevice(); err == nil {
		t.Fatal("unknown device credential field was accepted")
	}
}

func TestStoreRejectsUnsupportedVersion(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{"version":9,"generation":1,"device":{"base_url":"http://192.0.2.10"},"plugins":{},"apps":{}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := NewStore(path).Load(); err == nil {
		t.Fatal("unsupported configuration version was accepted")
	}
}

func TestStoreRejectsNestedDuplicateNames(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	content := `{"version":1,"generation":1,"device":{"base_url":"http://192.0.2.10","base_url":"http://198.51.100.10"},"plugins":{},"apps":{}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := NewStore(path).Load(); err == nil {
		t.Fatal("configuration with a nested duplicate name was accepted")
	}
}

func TestStoreRejectsUnknownFieldsAndTrailingDocuments(t *testing.T) {
	for name, content := range map[string]string{
		"unknown":  `{"version":1,"generation":1,"plugins":{},"apps":{},"surprise":true}`,
		"trailing": `{"version":1,"generation":1,"plugins":{},"apps":{}} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewStore(path).Load(); err == nil {
				t.Fatal("invalid config unexpectedly loaded")
			}
		})
	}
}

func TestStoreUsesExact64KiBAppConfigLimit(t *testing.T) {
	objectOfSize := func(size int) json.RawMessage {
		const shell = `{"x":""}`
		return json.RawMessage(`{"x":"` + strings.Repeat("x", size-len(shell)) + `"}`)
	}
	for _, test := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "at limit", size: 64 << 10},
		{name: "over limit", size: (64 << 10) + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := validDocument()
			app := document.Apps["ball8"]
			app.Config = objectOfSize(test.size)
			document.Apps["ball8"] = app
			_, err := NewStore(filepath.Join(t.TempDir(), "config.json")).ReplaceWithOutcome(0, document)
			if (err != nil) != test.wantErr {
				t.Fatalf("Replace error = %v, wantErr=%t", err, test.wantErr)
			}
		})
	}
}

func TestDocumentRejectsNonObjectAppConfig(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		config json.RawMessage
	}{
		{name: "scalar", config: json.RawMessage(`"value"`)},
		{name: "array", config: json.RawMessage(`[]`)},
		{name: "null", config: json.RawMessage(`null`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			document := validDocument()
			app := document.Apps["ball8"]
			app.Config = test.config
			document.Apps["ball8"] = app
			if err := document.Validate(); err == nil {
				t.Fatalf("config %s unexpectedly validated", test.config)
			}
		})
	}
}

func TestDocumentRejectsPlaintextSecretReference(t *testing.T) {
	document := validDocument()
	app := document.Apps["ball8"]
	app.Secrets = map[string]string{"token": "plaintext-token"}
	document.Apps["ball8"] = app
	if err := document.Validate(); err == nil {
		t.Fatal("plaintext secret reference unexpectedly validated")
	}
}

func TestDocumentRejectsAppConfigurationThatViolatesPluginSchema(t *testing.T) {
	root := t.TempDir()
	schema := []byte(`{"type":"object","required":["answers"],"properties":{"answers":{"type":"array"}},"additionalProperties":false}`)
	if err := os.WriteFile(filepath.Join(root, "config.schema.json"), schema, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(schema)
	document := validDocument()
	plugin := document.Plugins["dev.bsbctl.ball8"]
	plugin.PackageRoot = root
	plugin.ConfigSchema = &catalog.ConfigSchemaDeclaration{
		Source: "config.schema.json", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(schema)),
	}
	document.Plugins[plugin.ID] = plugin
	app := document.Apps["ball8"]
	app.Config = json.RawMessage(`{"answers":"yes"}`)
	document.Apps[app.ID] = app

	if err := document.Validate(); err == nil {
		t.Fatal("schema-invalid app configuration unexpectedly validated")
	}
}

func TestDocumentUsesStrictKeychainReferenceParserWithoutEchoingReference(t *testing.T) {
	document := validDocument()
	const secretCanary = "keychain://bsbctl/account?token=secret-canary"
	app := document.Apps["ball8"]
	app.Secrets = map[string]string{"token": secretCanary}
	document.Apps["ball8"] = app
	err := document.Validate()
	if err == nil {
		t.Fatal("keychain reference with query unexpectedly validated")
	}
	if strings.Contains(err.Error(), secretCanary) || strings.Contains(err.Error(), "secret-canary") {
		t.Fatalf("validation error echoed reference: %v", err)
	}
}

func TestDocumentRejectsUnboundedOrControlCharacterIdentifiers(t *testing.T) {
	for name, id := range map[string]string{"control": "bad\x00id", "unbounded": strings.Repeat("a", 129)} {
		t.Run(name, func(t *testing.T) {
			document := validDocument()
			plugin := document.Plugins["dev.bsbctl.ball8"]
			delete(document.Plugins, plugin.ID)
			plugin.ID = id
			document.Plugins[id] = plugin
			app := document.Apps["ball8"]
			app.PluginID = id
			document.Apps["ball8"] = app
			if err := document.Validate(); err == nil {
				t.Fatalf("identifier %q unexpectedly validated", id)
			}
		})
	}
}

func TestDocumentRejectsUnknownOrDuplicateExecutionModes(t *testing.T) {
	for name, modes := range map[string][]protocol.ExecutionMode{
		"unknown":   {"network"},
		"duplicate": {protocol.ExecutionModeInteractive, protocol.ExecutionModeInteractive},
	} {
		t.Run(name, func(t *testing.T) {
			document := validDocument()
			plugin := document.Plugins["dev.bsbctl.ball8"]
			plugin.ExecutionModes = modes
			document.Plugins[plugin.ID] = plugin
			if err := document.Validate(); err == nil {
				t.Fatalf("execution modes %v unexpectedly validated", modes)
			}
		})
	}
}

func validDocument() Document {
	configJSON, _ := json.Marshal(map[string]any{"answers": []string{"yes", "no"}})
	return Document{
		Version: CurrentVersion, Generation: 1,
		Device: Device{BaseURL: "http://busybar.local", AccessTokenSecret: "keychain://bsbctl/device/access-token"},
		Plugins: map[string]Plugin{
			"dev.bsbctl.ball8": {
				ID: "dev.bsbctl.ball8", Version: "dev", Executable: "/tmp/bsbctl-plugin-ball8",
				ProtocolVersion: protocol.Version,
				ExecutionModes:  []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
				Channels:        []protocol.Channel{{ID: "answer"}},
			},
		},
		Apps: map[string]App{
			"ball8": {
				ID: "ball8", PluginID: "dev.bsbctl.ball8", Generation: 1, Enabled: true, Config: configJSON,
				Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}},
			},
		},
	}
}

func TestDocumentValidatesRotationSchedulingBoundsAndPolicyExclusivity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		policy  presentation.PolicyConfig
		wantErr bool
	}{
		{name: "minimum", policy: presentation.PolicyConfig{Policy: presentation.PolicyRotation, RotationIntervalMS: 10_000}},
		{name: "maximum", policy: presentation.PolicyConfig{Policy: presentation.PolicyRotation, RotationIntervalMS: 86_400_000, RotationJitterPercent: 50}},
		{name: "missing interval", policy: presentation.PolicyConfig{Policy: presentation.PolicyRotation}, wantErr: true},
		{name: "interval below minimum", policy: presentation.PolicyConfig{Policy: presentation.PolicyRotation, RotationIntervalMS: 9_999}, wantErr: true},
		{name: "interval above maximum", policy: presentation.PolicyConfig{Policy: presentation.PolicyRotation, RotationIntervalMS: 86_400_001}, wantErr: true},
		{name: "negative jitter", policy: presentation.PolicyConfig{Policy: presentation.PolicyRotation, RotationIntervalMS: 10_000, RotationJitterPercent: -1}, wantErr: true},
		{name: "jitter above maximum", policy: presentation.PolicyConfig{Policy: presentation.PolicyRotation, RotationIntervalMS: 10_000, RotationJitterPercent: 51}, wantErr: true},
		{name: "non rotation interval", policy: presentation.PolicyConfig{Policy: presentation.PolicyWhenRelevant, RotationIntervalMS: 10_000}, wantErr: true},
		{name: "non rotation jitter", policy: presentation.PolicyConfig{Policy: presentation.PolicyInteractive, RotationJitterPercent: 10}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := validDocument()
			app := document.Apps["ball8"]
			app.Policies["answer"] = test.policy
			document.Apps["ball8"] = app
			err := document.Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestDocumentValidatesChannelActivationAction(t *testing.T) {
	t.Parallel()
	for name, action := range map[string]string{
		"valid":    "open",
		"control":  "open\ndetail",
		"too long": strings.Repeat("x", 129),
	} {
		t.Run(name, func(t *testing.T) {
			document := validDocument()
			app := document.Apps["ball8"]
			policy := app.Policies["answer"]
			policy.ActivationAction = action
			app.Policies["answer"] = policy
			document.Apps["ball8"] = app
			err := document.Validate()
			if name == "valid" && err != nil {
				t.Fatalf("valid activation action: %v", err)
			}
			if name != "valid" && err == nil {
				t.Fatal("invalid activation action validated")
			}
		})
	}
}

func TestDocumentRejectsActivationActionForProtocolBeforeExactSessionTriggers(t *testing.T) {
	document := validDocument()
	plugin := document.Plugins["dev.bsbctl.ball8"]
	plugin.ProtocolVersion = "3.0"
	document.Plugins[plugin.ID] = plugin
	app := document.Apps["ball8"]
	policy := app.Policies["answer"]
	policy.ActivationAction = "open"
	app.Policies["answer"] = policy
	document.Apps[app.ID] = app
	if err := document.Validate(); err == nil {
		t.Fatal("protocol v3.1 plugin accepted an exact observation activation action")
	}
}

func TestDocumentRejectsUnboundedPluginAndAppCollections(t *testing.T) {
	t.Parallel()
	tooManyPlugins := validDocument()
	for index := 0; index < MaxPlugins; index++ {
		id := fmt.Sprintf("plugin-%d", index)
		tooManyPlugins.Plugins[id] = Plugin{ID: id, Version: "1", Executable: "/plugin", ProtocolVersion: protocol.Version, ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident}, Channels: []protocol.Channel{{ID: "main"}}}
	}
	if err := tooManyPlugins.Validate(); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("plugin capacity error = %v", err)
	}

	tooManyApps := validDocument()
	for index := 0; index < MaxApps; index++ {
		id := fmt.Sprintf("app-%d", index)
		tooManyApps.Apps[id] = App{ID: id, PluginID: "plugin", Config: json.RawMessage(`{}`), Policies: map[string]presentation.PolicyConfig{}}
	}
	if err := tooManyApps.Validate(); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("app capacity error = %v", err)
	}
}

func TestActivationInputPersistsAndRequiresAction(t *testing.T) {
	for _, tc := range []struct {
		input, action string
		valid         bool
	}{{"", "", true}, {"start", "open", true}, {"start_or_encoder", "open", true}, {"start_or_encoder", "", false}, {"start", "", false}, {"ok", "open", false}} {
		t.Run(tc.input+"/"+tc.action, func(t *testing.T) {
			d := validDocument()
			app := d.Apps["ball8"]
			p := app.Policies["answer"]
			p.ActivationInput = tc.input
			p.ActivationAction = tc.action
			app.Policies["answer"] = p
			d.Apps["ball8"] = app
			if err := d.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate = %v", err)
			}
			if tc.valid {
				store := NewStore(filepath.Join(t.TempDir(), "config.json"))
				if _, err := store.ReplaceWithOutcome(0, d); err != nil {
					t.Fatal(err)
				}
				loaded, err := store.Load()
				if err != nil {
					t.Fatal(err)
				}
				if loaded.Apps["ball8"].Policies["answer"].ActivationInput != tc.input {
					t.Fatal("activation input lost in persistence")
				}
			}
		})
	}
}
