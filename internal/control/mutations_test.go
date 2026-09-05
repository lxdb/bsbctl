package control

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/installer"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
	"github.com/lxdb/bsbctl/sdk/rpc"
	"golang.org/x/sys/unix"
)

func TestControlMutationResultsAreStableAndDoNotEchoConfiguration(t *testing.T) {
	backend := &mutationBackend{fakeBackend: &fakeBackend{document: config.Document{
		Version: config.CurrentVersion, Generation: 4,
		Plugins: map[string]config.Plugin{"plugin": {ID: "plugin", Version: "1", Executable: "/plugin", ProtocolVersion: protocol.Version, ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive}, Channels: []protocol.Channel{{ID: "answer"}}}},
		Apps:    map[string]config.App{"ball8": {ID: "ball8", PluginID: "plugin", Config: json.RawMessage(`{}`), Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}}}},
	}}}
	_, client, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer client.Close()

	var enabled AppMutationResult
	if err := client.Call(context.Background(), "app.set_enabled", SetEnabledRequest{AppID: "ball8", Enabled: true}, &enabled); err != nil {
		t.Fatal(err)
	}
	if enabled.Status != MutationUpdated || enabled.AppID != "ball8" || !enabled.Enabled || enabled.Generation != 4 {
		t.Fatalf("enable result = %#v", enabled)
	}
	replacement := ReplaceConfigRequest{
		AppID: "ball8", Config: json.RawMessage(`{"sentinel":"private-config"}`),
		Secrets:  map[string]string{"token": "keychain://bsbctl/private-account"},
		Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}}, LaunchAction: "ask",
	}
	var configured AppConfigResult
	if err := client.Call(context.Background(), "app.replace_config", replacement, &configured); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(configured)
	if err != nil {
		t.Fatal(err)
	}
	if configured.Status != MutationUpdated || configured.AppID != "ball8" || configured.Generation != 5 || containsAny(string(encoded), "private-config", "private-account", "keychain://") {
		t.Fatalf("config result = %s", encoded)
	}
}

func TestControlCreatesAndDeletesAppInstancesWithRedactedStableResults(t *testing.T) {
	backend := &mutationBackend{fakeBackend: &fakeBackend{document: config.Document{
		Version: config.CurrentVersion, Generation: 4,
		Plugins: map[string]config.Plugin{"plugin": {ID: "plugin", Version: "1", Executable: "/plugin", ProtocolVersion: protocol.Version, ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeInteractive}, Channels: []protocol.Channel{{ID: "answer"}}}},
		Apps:    map[string]config.App{},
	}}}
	_, client, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer client.Close()

	request := CreateAppRequest{
		AppID: "codex-secondary", PluginID: "plugin", Enabled: true,
		Config:   json.RawMessage(`{"sentinel":"private-config"}`),
		Secrets:  map[string]string{"token": "keychain://bsbctl/private-account"},
		Policies: map[string]presentation.PolicyConfig{"answer": {Policy: presentation.PolicyInteractive}},
	}
	var created AppInstanceResult
	if err := client.Call(context.Background(), "app.create", request, &created); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(created)
	if created.Status != MutationCreated || created.AppID != request.AppID || created.PluginID != request.PluginID || !created.Enabled || created.Generation != 5 || containsAny(string(encoded), "private-config", "private-account", "keychain://") {
		t.Fatalf("create result = %s", encoded)
	}
	var deleted AppInstanceResult
	if err := client.Call(context.Background(), "app.delete", DeleteAppRequest{AppID: request.AppID}, &deleted); err != nil {
		t.Fatal(err)
	}
	if deleted.Status != MutationDeleted || deleted.AppID != request.AppID || deleted.PluginID != request.PluginID || deleted.Generation != 6 {
		t.Fatalf("delete result = %#v", deleted)
	}
}

func TestControlCatalogOperationsReturnOnlyRedactedResultAndCode(t *testing.T) {
	backend := &mutationBackend{fakeBackend: &fakeBackend{document: emptyControlDocument()}}
	backend.catalogResult = installer.Result{Status: installer.StatusInstalled, Release: installer.ReleaseRef{ID: "plugin", Version: "1", OS: "darwin", Arch: "arm64"}}
	directory := t.TempDir()
	catalogPath := filepath.Join(directory, "catalog.json")
	signaturePath := filepath.Join(directory, "catalog.sig")
	if err := os.WriteFile(catalogPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signaturePath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, client, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer client.Close()

	var response CatalogOperationResponse
	if err := client.Call(context.Background(), "plugin.install", catalogInstallRequest(catalogPath, signaturePath, "plugin", "1"), &response); err != nil {
		t.Fatal(err)
	}
	if response.ErrorCode != "" || response.Result.Status != installer.StatusInstalled || backend.catalogOperation != "install" {
		t.Fatalf("catalog response/backend = %#v/%q", response, backend.catalogOperation)
	}
	backend.catalogErr = &installer.Error{Code: installer.CodeRecoveryRequired}
	if err := client.Call(context.Background(), "plugin.update", catalogInstallRequest(catalogPath, signaturePath, "plugin", "2"), &response); err != nil {
		t.Fatal(err)
	}
	if response.ErrorCode != installer.CodeRecoveryRequired || backend.catalogOperation != "update" {
		t.Fatalf("catalog recovery response/backend = %#v/%q", response, backend.catalogOperation)
	}
	backend.catalogErr = errors.New("raw downloader token=secret /private/path")
	if err := client.Call(context.Background(), "plugin.install", catalogInstallRequest(catalogPath, signaturePath, "plugin", "3"), &response); err != nil {
		t.Fatal(err)
	}
	if response.ErrorCode != CatalogDependencyFailed {
		t.Fatalf("catalog dependency response = %#v", response)
	}
}

func TestControlCatalogInstallFailsClosedOnPathReplacementAndRelativePaths(t *testing.T) {
	previous := runtimePlatform
	runtimePlatform = func() (string, string) { return "darwin", "arm64" }
	defer func() { runtimePlatform = previous }()
	backend := &mutationBackend{fakeBackend: &fakeBackend{document: emptyControlDocument()}}
	directory := t.TempDir()
	catalogPath := filepath.Join(directory, "catalog.json")
	signaturePath := filepath.Join(directory, "catalog.sig")
	originalCatalog := []byte(`{"sequence":1}`)
	if err := os.WriteFile(catalogPath, originalCatalog, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signaturePath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	request := catalogInstallRequest(catalogPath, signaturePath, "plugin", "1")
	if err := os.WriteFile(filepath.Join(directory, "replacement.json"), []byte(`{"sequence":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(directory, "replacement.json"), catalogPath); err != nil {
		t.Fatal(err)
	}
	_, client, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer client.Close()

	err := client.Call(context.Background(), "plugin.install", request, nil)
	rpcErr, ok := errors.AsType[*rpc.Error](err)
	if !ok || rpcErr.Code != -32063 {
		t.Fatalf("replaced catalog error = %v", err)
	}
	request.CatalogPath = "catalog.json"
	err = client.Call(context.Background(), "plugin.install", request, nil)
	rpcErr, ok = errors.AsType[*rpc.Error](err)
	if !ok || rpcErr.Code != -32602 {
		t.Fatalf("relative catalog error = %v", err)
	}
	if backend.catalogOperation != "" {
		t.Fatalf("backend operation = %q", backend.catalogOperation)
	}
}

func TestControlCatalogInstallRejectsFIFOAndUnixSocketInputsWithoutOpeningThem(t *testing.T) {
	previous := runtimePlatform
	runtimePlatform = func() (string, string) { return "darwin", "arm64" }
	defer func() { runtimePlatform = previous }()
	backend := &mutationBackend{fakeBackend: &fakeBackend{document: emptyControlDocument()}}
	directory := t.TempDir()
	fifoPath := filepath.Join(directory, "catalog.fifo")
	if err := unix.Mkfifo(fifoPath, 0o600); err != nil {
		t.Fatal(err)
	}
	signaturePath := filepath.Join(directory, "catalog.sig")
	signatureData := []byte(`{}`)
	if err := os.WriteFile(signaturePath, signatureData, 0o600); err != nil {
		t.Fatal(err)
	}
	signatureDigest := sha256.Sum256(signatureData)
	placeholderDigest := sha256.Sum256(nil)
	request := CatalogInstallRequest{
		CatalogPath: fifoPath, SignaturePath: signaturePath,
		CatalogSHA256: fmt.Sprintf("%x", placeholderDigest), SignatureSHA256: fmt.Sprintf("%x", signatureDigest),
		PluginID: "plugin", Version: "1", OS: "darwin", Arch: "arm64",
	}
	_, client, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer client.Close()

	resultChannel := make(chan error, 1)
	go func() {
		resultChannel <- client.Call(context.Background(), "plugin.install", request, nil)
	}()
	stopProbe := make(chan struct{})
	probeConnected := make(chan struct{})
	probeDone := make(chan struct{})
	probeError := make(chan error, 1)
	go probeCatalogFIFOWriter(fifoPath, stopProbe, probeConnected, probeDone, probeError)

	var callErr error
	select {
	case callErr = <-resultChannel:
		close(stopProbe)
		<-probeDone
	case <-probeConnected:
		callErr = <-resultChannel
		<-probeDone
		t.Fatalf("daemon opened FIFO before classifying it: %v", callErr)
	case err := <-probeError:
		close(stopProbe)
		<-probeDone
		t.Fatal(err)
	}
	rpcErr, ok := errors.AsType[*rpc.Error](callErr)
	if !ok || rpcErr.Code != -32602 {
		t.Fatalf("FIFO error = %v", callErr)
	}

	socketDirectory, err := os.MkdirTemp("/tmp", "bctl-catalog-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDirectory) })
	socketPath := filepath.Join(socketDirectory, "catalog.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	request.CatalogPath = socketPath
	callErr = client.Call(context.Background(), "plugin.install", request, nil)
	rpcErr, ok = errors.AsType[*rpc.Error](callErr)
	if !ok || rpcErr.Code != -32602 {
		t.Fatalf("Unix socket error = %v", callErr)
	}
	if backend.catalogOperation != "" {
		t.Fatalf("backend operation = %q", backend.catalogOperation)
	}
}

func probeCatalogFIFOWriter(path string, stop <-chan struct{}, connected chan<- struct{}, done chan<- struct{}, failure chan<- error) {
	defer close(done)
	for {
		select {
		case <-stop:
			return
		default:
		}
		fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			connected <- struct{}{}
			if closeErr := unix.Close(fd); closeErr != nil {
				failure <- closeErr
			}
			return
		}
		if !errors.Is(err, unix.ENXIO) {
			failure <- err
			return
		}
		runtime.Gosched()
	}
}

func TestControlRejectsDuplicateAndUnknownConfigFieldsBeforeBackend(t *testing.T) {
	backend := &mutationBackend{fakeBackend: &fakeBackend{document: emptyControlDocument()}}
	_, client, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer client.Close()
	inputs := []json.RawMessage{
		json.RawMessage(`{"app_id":"ball8","config":{},"config":{},"policies":{}}`),
		json.RawMessage(`{"app_id":"ball8","config":{"x":1,"x":2},"policies":{}}`),
		json.RawMessage(`{"app_id":"ball8","config":{},"policies":{},"unknown":true}`),
		json.RawMessage(`{"app_id":"ball8","config":{},"secrets":{"token":"keychain://bsbctl/team/account"},"policies":{}}`),
	}
	for index, input := range inputs {
		err := client.Call(context.Background(), "app.replace_config", input, nil)
		if err == nil {
			t.Fatalf("input %d was accepted", index)
		}
	}
	if backend.replaceCalls != 0 {
		t.Fatalf("backend replacement calls = %d", backend.replaceCalls)
	}
}

func TestControlRejectsAggregateConfigRequestOver256KiBBeforeBackend(t *testing.T) {
	backend := &mutationBackend{fakeBackend: &fakeBackend{document: emptyControlDocument()}}
	_, client, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer client.Close()

	raw := json.RawMessage(`{"app_id":"ball8","config":{},"policies":{},"launch_action":"` + strings.Repeat("x", 256<<10) + `"}`)
	err := client.Call(context.Background(), "app.replace_config", raw, nil)
	rpcErr, ok := errors.AsType[*rpc.Error](err)
	if !ok || rpcErr.Code != -32602 {
		t.Fatalf("oversized replacement error = %v", err)
	}
	if backend.replaceCalls != 0 {
		t.Fatalf("backend replacement calls = %d", backend.replaceCalls)
	}
}

func TestControlRejectsCallerPlatformThatDoesNotMatchDaemon(t *testing.T) {
	previous := runtimePlatform
	runtimePlatform = func() (string, string) { return "darwin", "arm64" }
	defer func() { runtimePlatform = previous }()
	backend := &mutationBackend{fakeBackend: &fakeBackend{document: emptyControlDocument()}}
	_, client, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer client.Close()

	err := client.Call(context.Background(), "plugin.rollback", CatalogRollbackRequest{PluginID: "plugin", OS: "darwin", Arch: "amd64"}, nil)
	rpcErr, ok := errors.AsType[*rpc.Error](err)
	if !ok || rpcErr.Code != -32602 {
		t.Fatalf("opposite-architecture rollback error = %v", err)
	}
	if backend.catalogOperation != "" {
		t.Fatalf("backend operation = %q", backend.catalogOperation)
	}
}

func TestControlClassifiesInvalidAndCommittedPartialConfigReplacement(t *testing.T) {
	backend := &mutationBackend{
		fakeBackend:    &fakeBackend{document: emptyControlDocument()},
		replaceOutcome: localstate.NotCommitted, replaceErr: daemon.ErrInvalidAppConfiguration,
	}
	_, client, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer client.Close()
	request := ReplaceConfigRequest{AppID: "ball8", Config: json.RawMessage(`{}`), Policies: map[string]presentation.PolicyConfig{}}
	err := client.Call(context.Background(), "app.replace_config", request, nil)
	rpcErr, ok := errors.AsType[*rpc.Error](err)
	if !ok || rpcErr.Code != -32602 {
		t.Fatalf("invalid replacement error = %v", err)
	}
	backend.replaceOutcome = localstate.Committed
	backend.replaceErr = errors.New("raw reconciliation path /secret")
	backend.document.Generation = 8
	var result AppConfigResult
	if err := client.Call(context.Background(), "app.replace_config", request, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != MutationPartial || result.Generation != 9 {
		t.Fatalf("partial result = %#v", result)
	}
}

func TestControlReturnsCommittedEnablementAsPartialWithoutRawError(t *testing.T) {
	base := &mutationBackend{fakeBackend: &fakeBackend{document: config.Document{
		Version: config.CurrentVersion, Generation: 3, Apps: map[string]config.App{"ball8": {ID: "ball8", PluginID: "plugin"}}, Plugins: map[string]config.Plugin{},
	}}}
	backend := &partialEnableBackend{mutationBackend: base}
	_, client, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer client.Close()
	var result AppMutationResult
	if err := client.Call(context.Background(), "app.set_enabled", SetEnabledRequest{AppID: "ball8", Enabled: true}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != MutationPartial || result.Generation != 4 || !result.Enabled {
		t.Fatalf("partial enablement result = %#v", result)
	}
}

func TestControlEnablementUsesOnlyOperationLocalOutcomeUnderConcurrency(t *testing.T) {
	backend := &operationLocalEnableBackend{fakeBackend: &fakeBackend{document: emptyControlDocument()}}
	server, first, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer first.Close()
	second, err := Dial(context.Background(), server.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	type response struct {
		result AppMutationResult
		err    error
	}
	responses := make(chan response, 2)
	var start sync.WaitGroup
	start.Add(2)
	go func() {
		start.Done()
		start.Wait()
		var result AppMutationResult
		err := first.Call(context.Background(), "app.set_enabled", SetEnabledRequest{AppID: "ball8", Enabled: true}, &result)
		responses <- response{result: result, err: err}
	}()
	go func() {
		start.Done()
		start.Wait()
		var result AppMutationResult
		err := second.Call(context.Background(), "app.set_enabled", SetEnabledRequest{AppID: "ball8", Enabled: false}, &result)
		responses <- response{result: result, err: err}
	}()

	seen := make(map[bool]AppMutationResult)
	for range 2 {
		response := <-responses
		if response.err != nil {
			t.Fatal(response.err)
		}
		seen[response.result.Enabled] = response.result
	}
	if got := seen[true]; got.Status != MutationDurabilityUncertain || got.Generation != 10 {
		t.Fatalf("enable result = %#v", got)
	}
	if got := seen[false]; got.Status != MutationPartial || got.Generation != 11 {
		t.Fatalf("disable result = %#v", got)
	}
}

func TestControlDeleteUsesTransactionLocalMetadataAcrossSameIDRecreation(t *testing.T) {
	backend := &interleavedAppInstanceBackend{
		fakeBackend: &fakeBackend{document: config.Document{
			Version: config.CurrentVersion, Generation: 8,
			Apps: map[string]config.App{"shared": {ID: "shared", PluginID: "old-plugin", Enabled: true}},
		}},
		deleteStarted:     make(chan struct{}),
		recreateCommitted: make(chan struct{}),
	}
	server, first, cancel := startControlTestServer(t, backend)
	defer cancel()
	defer first.Close()
	second, err := Dial(context.Background(), server.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	deleted := make(chan AppInstanceResult, 1)
	deleteErr := make(chan error, 1)
	go func() {
		var result AppInstanceResult
		deleteErr <- first.Call(context.Background(), "app.delete", DeleteAppRequest{AppID: "shared"}, &result)
		deleted <- result
	}()
	<-backend.deleteStarted

	var created AppInstanceResult
	err = second.Call(context.Background(), "app.create", CreateAppRequest{
		AppID: "shared", PluginID: "new-plugin", Enabled: true,
		Config: json.RawMessage(`{}`), Policies: map[string]presentation.PolicyConfig{},
	}, &created)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-deleteErr; err != nil {
		t.Fatal(err)
	}
	result := <-deleted
	if result.PluginID != "new-plugin" || result.Generation != 10 {
		t.Fatalf("delete result = %#v", result)
	}
}

type mutationBackend struct {
	*fakeBackend
	catalogResult    installer.Result
	catalogSnapshot  installer.Snapshot
	catalogErr       error
	catalogOperation string
	replaceCalls     int
	replaceOutcome   localstate.CommitOutcome
	replaceErr       error
}

type partialEnableBackend struct{ *mutationBackend }

type operationLocalEnableBackend struct{ *fakeBackend }

type interleavedAppInstanceBackend struct {
	*fakeBackend
	deleteStarted     chan struct{}
	recreateCommitted chan struct{}
}

func (b *interleavedAppInstanceBackend) CreateAppInstance(_ context.Context, app config.App) (daemon.AppInstanceResult, error) {
	close(b.recreateCommitted)
	return daemon.AppInstanceResult{
		Document: config.Document{Version: config.CurrentVersion, Generation: 9}, Outcome: localstate.Committed,
		AppID: app.ID, PluginID: app.PluginID, Enabled: app.Enabled,
	}, nil
}

func (b *interleavedAppInstanceBackend) DeleteAppInstance(_ context.Context, appID string) (daemon.AppInstanceResult, error) {
	close(b.deleteStarted)
	<-b.recreateCommitted
	return daemon.AppInstanceResult{
		Document: config.Document{Version: config.CurrentVersion, Generation: 10}, Outcome: localstate.Committed,
		AppID: appID, PluginID: "new-plugin", Enabled: true,
	}, nil
}

func (b *operationLocalEnableBackend) SetEnabled(_ context.Context, _ string, enabled bool) (daemon.EnableResult, error) {
	document := emptyControlDocument()
	app := document.Apps["ball8"]
	app.Enabled = enabled
	document.Apps["ball8"] = app
	if enabled {
		document.Generation = 10
		return daemon.EnableResult{Document: document, Changed: true, Outcome: localstate.CommittedDurabilityUncertain}, nil
	}
	document.Generation = 11
	return daemon.EnableResult{Document: document, Changed: true, Outcome: localstate.Committed, ReconciliationError: errors.New("raw apply path /secret")}, nil
}

func (b *partialEnableBackend) SetEnabled(_ context.Context, appID string, enabled bool) (daemon.EnableResult, error) {
	document := b.document
	app := document.Apps[appID]
	app.Enabled = enabled
	document.Apps[appID] = app
	document.Generation++
	b.document = document
	return daemon.EnableResult{Document: document, Changed: true, Outcome: localstate.Committed, ReconciliationError: errors.New("raw apply failure /secret")}, nil
}

func (b *mutationBackend) ReplaceAppConfiguration(_ context.Context, appID string, replacement daemon.AppConfiguration) (config.Document, localstate.CommitOutcome, error) {
	b.replaceCalls++
	document := b.document
	app := document.Apps[appID]
	app.Config = append(json.RawMessage(nil), replacement.Config...)
	app.Secrets = replacement.Secrets
	app.Policies = replacement.Policies
	app.LaunchAction = replacement.LaunchAction
	document.Apps[appID] = app
	document.Generation++
	b.document = document
	outcome := b.replaceOutcome
	if outcome == "" {
		outcome = localstate.Committed
	}
	return document, outcome, b.replaceErr
}
func (b *mutationBackend) CreateAppInstance(_ context.Context, app config.App) (daemon.AppInstanceResult, error) {
	if _, exists := b.document.Apps[app.ID]; exists {
		return daemon.AppInstanceResult{}, daemon.ErrAppAlreadyExists
	}
	b.document.Generation++
	b.document.Apps[app.ID] = app
	return daemon.AppInstanceResult{
		Document: b.document, AppID: app.ID, PluginID: app.PluginID, Enabled: app.Enabled, Outcome: localstate.Committed,
	}, nil
}
func (b *mutationBackend) DeleteAppInstance(_ context.Context, appID string) (daemon.AppInstanceResult, error) {
	app, exists := b.document.Apps[appID]
	if !exists {
		return daemon.AppInstanceResult{}, daemon.ErrAppNotFound
	}
	delete(b.document.Apps, appID)
	b.document.Generation++
	result := daemon.AppInstanceResult{
		Document: b.document, AppID: app.ID, PluginID: app.PluginID, Enabled: app.Enabled, Outcome: localstate.Committed,
	}
	return result, nil
}
func (b *mutationBackend) CatalogInstall(_ context.Context, _ installer.InstallRequest, update bool) (installer.Result, error) {
	b.catalogOperation = "install"
	if update {
		b.catalogOperation = "update"
	}
	return b.catalogResult, b.catalogErr
}
func (b *mutationBackend) CatalogRollback(context.Context, installer.RollbackRequest) (installer.Result, error) {
	b.catalogOperation = "rollback"
	return b.catalogResult, b.catalogErr
}
func (b *mutationBackend) CatalogStatus(context.Context, string) (installer.Snapshot, error) {
	b.catalogOperation = "status"
	return b.catalogSnapshot, b.catalogErr
}

func catalogInstallRequest(catalogPath, signaturePath, pluginID, version string) CatalogInstallRequest {
	catalogData, err := os.ReadFile(catalogPath)
	if err != nil {
		panic(err)
	}
	signatureData, err := os.ReadFile(signaturePath)
	if err != nil {
		panic(err)
	}
	catalogDigest := sha256.Sum256(catalogData)
	signatureDigest := sha256.Sum256(signatureData)
	return CatalogInstallRequest{
		CatalogPath: catalogPath, SignaturePath: signaturePath,
		CatalogSHA256: fmt.Sprintf("%x", catalogDigest), SignatureSHA256: fmt.Sprintf("%x", signatureDigest),
		PluginID: pluginID, Version: version, OS: "darwin", Arch: "arm64",
	}
}
