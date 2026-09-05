package pluginverify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/configschema"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestVerifyExercisesAuthenticatedPackageAndLiveV1Contract(t *testing.T) {
	root := t.TempDir()
	executablePath := filepath.Join(root, "verify-plugin")
	testBinary := strings.ReplaceAll(os.Args[0], "'", `'\''`)
	writeTestFile(t, executablePath, []byte("#!/bin/sh\nexec '"+testBinary+"' -test.run '^TestVerifyHelperProcess$'\n"))
	if err := os.Chmod(executablePath, 0o700); err != nil {
		t.Fatal(err)
	}

	executableData, err := os.ReadFile(executablePath)
	if err != nil {
		t.Fatal(err)
	}
	schemaData := []byte(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["enabled"],"properties":{"enabled":{"type":"boolean"}},"additionalProperties":false}`)
	writeTestFile(t, filepath.Join(root, configschema.FileName), schemaData)
	assetData := []byte("authenticated test asset")
	writeTestFile(t, filepath.Join(root, "assets", "status.png"), assetData)
	manifest := catalog.PackageManifest{
		ID: "dev.bsbctl.verify-test", Version: "1.0.0", ProtocolVersion: protocol.Version,
		Executable: filepath.Base(executablePath), ExecutableSHA256: testDigest(executableData), ExecutableSize: int64(len(executableData)),
		ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
		Channels:       []protocol.Channel{{ID: "main"}},
		Operations:     []protocol.OperationDescriptor{{ID: "inspect", Kind: protocol.OperationQuery}},
		ConfigSchema:   &configschema.Declaration{Source: configschema.FileName, SHA256: testDigest(schemaData), Size: int64(len(schemaData))},
		Assets:         []assets.Declaration{{Source: "assets/status.png", SHA256: testDigest(assetData), Size: int64(len(assetData)), MediaType: "image/png"}},
	}
	manifestPath := filepath.Join(root, "manifest.json")
	writeTestJSON(t, manifestPath, manifest)
	now := time.Now().UTC()
	ref := protocol.InstanceRef{ID: "main", Generation: 7}
	fixture := Fixture{
		Version:   1,
		Instances: []protocol.Instance{{ID: ref.ID, Generation: ref.Generation, Config: json.RawMessage(`{"enabled":true}`)}},
		Sessions: []SessionCase{{
			Start:  protocol.SessionStartRequest{Instance: ref, Action: "open", SessionToken: "session-1"},
			Inputs: []protocol.SessionInputRequest{{Sequence: 1, OccurredAt: now, Instance: ref, SessionToken: "session-1", Input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonOK, Action: protocol.ButtonPress}}}},
			End:    protocol.SessionEndRequest{Instance: ref, SessionToken: "session-1"},
		}},
		Operations: []protocol.OperationRequest{{Instance: ref, Operation: "inspect", Payload: json.RawMessage(`{"detail":true}`)}},
	}
	fixturePath := filepath.Join(root, "fixture.json")
	writeTestJSON(t, fixturePath, fixture)

	report, err := Verify(t.Context(), Options{ManifestPath: manifestPath, FixturePath: fixturePath, CoreVersion: "test-core"})
	if err != nil {
		t.Fatalf("Verify: %v; report=%#v", err, report)
	}
	if !report.Passed || report.PluginID != manifest.ID || report.PluginVersion != manifest.Version || report.Protocol != protocol.Version || report.SignatureTrust != "not_evaluated" {
		t.Fatalf("report identity = %#v", report)
	}
	wantCalls := HostCalls{Observations: 1, Withdrawals: 1, Checkpoints: 1, SessionCompletions: 1, Logs: 1, Metrics: 1}
	if !reflect.DeepEqual(report.HostCalls, wantCalls) {
		t.Fatalf("host calls = %#v, want %#v", report.HostCalls, wantCalls)
	}
	wantChecks := []string{"manifest", "executable_digest", "assets", "fixture_coverage", "initialize_and_replace", "idempotent_replace", "session_start_0", "session_0_input_0", "session_end_0", "operation_0", "health", "pre_canceled_call", "empty_replace", "shutdown"}
	if len(report.Checks) != len(wantChecks) {
		t.Fatalf("checks = %#v", report.Checks)
	}
	for index, check := range report.Checks {
		if !check.Passed || check.Name != wantChecks[index] {
			t.Fatalf("check %d = %#v, want %q passed", index, check, wantChecks[index])
		}
	}
	fixture.Operations[0].Payload = json.RawMessage(`{"fail":true}`)
	writeTestJSON(t, fixturePath, fixture)
	report, err = Verify(t.Context(), Options{ManifestPath: manifestPath, FixturePath: fixturePath, CoreVersion: "test-core"})
	if err == nil || report.Passed || report.Error == "" {
		t.Fatalf("failed plugin operation was reported as success: %#v, %v", report, err)
	}
	wantCalls.Logs += 2 // The failing callback and joined shutdown both log.
	if !reflect.DeepEqual(report.HostCalls, wantCalls) {
		t.Fatalf("failure report omitted callbacks during cleanup: %#v, want %#v", report.HostCalls, wantCalls)
	}
}

func TestVerifyHelperProcess(t *testing.T) {
	if os.Getenv("BSBCTL_RPC_FD") == "" {
		return
	}
	if err := pluginsdk.Run(t.Context(), pluginsdk.Definition{
		ID: "dev.bsbctl.verify-test", Version: "1.0.0",
		Contract: pluginsdk.Contract{
			ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
			Channels:       []protocol.Channel{{ID: "main"}},
			Operations:     []protocol.OperationDescriptor{{ID: "inspect", Kind: protocol.OperationQuery}},
		},
		New: func(host *pluginsdk.Host) pluginsdk.Plugin { return &verifierHelper{host: host} },
	}); err != nil {
		t.Fatal(err)
	}
}

type verifierHelper struct {
	host   *pluginsdk.Host
	failed bool
}

func (*verifierHelper) ReplaceInstances(context.Context, []protocol.Instance) error { return nil }

func (h *verifierHelper) StartSession(ctx context.Context, request protocol.SessionStartRequest) error {
	now := time.Now().UTC()
	if err := h.host.PublishObservation(ctx, protocol.Observation{
		Instance: request.Instance, Channel: "main", Key: "state", Revision: 1,
		Disposition: protocol.DispositionResolved, Impact: protocol.ImpactNormal, ReasonCode: "verified",
		ObservedAt: now, UpdatedAt: now,
	}); err != nil {
		return err
	}
	if err := h.host.Log(ctx, protocol.LogNotification{Level: protocol.LogLevelInfo, Event: "session.started", Instance: request.Instance}); err != nil {
		return err
	}
	_, err := h.host.RecordMetric(protocol.MetricNotification{Instance: request.Instance, Name: "session.count", Value: 1, Unit: "items"})
	return err
}

func (h *verifierHelper) HandleSessionInput(ctx context.Context, request protocol.SessionInputRequest) (protocol.SessionInputResult, error) {
	if err := h.host.SaveCheckpoint(ctx, protocol.CheckpointRequest{Instance: request.Instance, Data: json.RawMessage(`{"sequence":1}`)}); err != nil {
		return protocol.SessionInputResult{}, err
	}
	return protocol.SessionInputResult{Disposition: protocol.SessionInputConsumed}, h.host.CompleteSession(ctx, protocol.CompleteSessionRequest{Instance: request.Instance, SessionToken: request.SessionToken})
}

func (h *verifierHelper) EndSession(ctx context.Context, request protocol.SessionEndRequest) error {
	return h.host.WithdrawObservation(ctx, protocol.WithdrawRequest{Instance: request.Instance, Channel: "main", Key: "state"})
}

func (h *verifierHelper) InvokeOperation(ctx context.Context, request protocol.OperationRequest) (protocol.OperationResult, error) {
	if string(request.Payload) == `{"fail":true}` {
		h.failed = true
		_ = h.host.Log(ctx, protocol.LogNotification{Level: protocol.LogLevelInfo, Event: "operation.failed"})
		return protocol.OperationResult{}, errors.New("test operation failed")
	}
	return protocol.OperationResult{Payload: json.RawMessage(`{"verified":true}`)}, nil
}

func (*verifierHelper) Health(context.Context) protocol.HealthResult {
	return protocol.HealthResult{Healthy: true, ObservedAt: time.Now().UTC()}
}

func (h *verifierHelper) Shutdown(ctx context.Context) error {
	if h.failed {
		return h.host.Log(ctx, protocol.LogNotification{Level: protocol.LogLevelInfo, Event: "shutdown.joined"})
	}
	return nil
}

func writeTestFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, data)
}

func testDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func TestFirstPartyFixtureShapesCoverTheirDeclaredContracts(t *testing.T) {
	t.Parallel()
	for _, descriptor := range firstpartyplugins.All() {
		t.Run(descriptor.ID, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(descriptor.FixturePath)))
			if err != nil {
				t.Fatal(err)
			}
			var fixture Fixture
			if err := protocol.DecodeStrict(data, &fixture); err != nil {
				t.Fatal(err)
			}
			manifest := catalog.PackageManifest{
				ExecutionModes: descriptor.DefinitionForVersion(descriptor.DevelopmentVersion).Contract.ExecutionModes,
				Operations:     descriptor.DefinitionForVersion(descriptor.DevelopmentVersion).Contract.Operations,
			}
			if err := validateFixture(manifest, fixture, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestValidateFixtureRequiresOneCaseForEveryDeclaredContract(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	manifest := catalog.PackageManifest{
		ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive},
		Operations:     []protocol.OperationDescriptor{{ID: "inspect", Kind: protocol.OperationQuery}},
	}
	fixture := Fixture{Version: 1, Instances: []protocol.Instance{{ID: "main", Generation: 7, Config: json.RawMessage(`{}`)}}}
	if err := validateFixture(manifest, fixture, now); err == nil {
		t.Fatal("fixture without declared contract cases was accepted")
	}
	ref := protocol.InstanceRef{ID: "main", Generation: 7}
	fixture.Sessions = []SessionCase{{
		Start:  protocol.SessionStartRequest{Instance: ref, Action: "open", SessionToken: "session-1"},
		Inputs: []protocol.SessionInputRequest{{Sequence: 1, OccurredAt: now, Instance: ref, SessionToken: "session-1", Input: protocol.SessionInput{Button: &protocol.ButtonInput{Button: protocol.ButtonOK, Action: protocol.ButtonPress}}}},
		End:    protocol.SessionEndRequest{Instance: ref, SessionToken: "session-1"},
	}}
	fixture.Operations = []protocol.OperationRequest{{Instance: ref, Operation: "inspect", Payload: json.RawMessage(`{}`)}}
	if err := validateFixture(manifest, fixture, now); err != nil {
		t.Fatalf("complete fixture rejected: %v", err)
	}
}

func TestValidateFixtureRejectsInstanceReferencesOutsideDesiredSet(t *testing.T) {
	t.Parallel()
	manifest := catalog.PackageManifest{ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive}}
	fixture := Fixture{
		Version:   1,
		Instances: []protocol.Instance{{ID: "main", Generation: 1, Config: json.RawMessage(`{}`)}},
		Sessions: []SessionCase{{
			Start: protocol.SessionStartRequest{Instance: protocol.InstanceRef{ID: "other", Generation: 1}, Action: "open", SessionToken: "session"},
			End:   protocol.SessionEndRequest{Instance: protocol.InstanceRef{ID: "other", Generation: 1}, SessionToken: "session"},
		}},
	}
	if err := validateFixture(manifest, fixture, time.Now().UTC()); err == nil {
		t.Fatal("fixture referencing an unknown instance was accepted")
	}
}
