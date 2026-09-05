package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/soak"
)

func TestRunSoakRejectsInvalidEvidencePath(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run([]string{"soak", "--output", "relative.jsonl"}, &stdout, &stderr)
	if code != exitFailure || !strings.Contains(stderr.String(), "invalid soak arguments") {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func TestSyntheticCodexAuthFixtureIsValidJSON(t *testing.T) {
	t.Parallel()
	fixture := syntheticCodexAuthFixture()
	if !json.Valid(fixture) {
		t.Fatalf("synthetic Codex auth fixture is invalid JSON: %q", fixture)
	}
	var document struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(fixture, &document); err != nil {
		t.Fatal(err)
	}
	if document.Tokens.AccessToken == "" || document.Tokens.AccountID == "" {
		t.Fatalf("synthetic Codex auth fixture is incomplete: %#v", document)
	}
}

func TestRunSoakProductionShape(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("production resource soak requires Darwin")
	}
	if os.Getenv("BSBCTL_RUN_SOAK_INTEGRATION") != "1" {
		t.Skip("set BSBCTL_RUN_SOAK_INTEGRATION=1 for the bounded process soak")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(t.TempDir(), "soak.jsonl")
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"soak", "--root", root, "--output", evidence,
		"--samples", "3", "--interval", "5s", "--warm", "5s", "--startup-timeout", "30s",
	}, &stdout, &stderr)
	if code != exitSuccess {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	file, err := os.Open(evidence)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	counts := make(map[string]int)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("invalid JSONL record: %v", err)
		}
		counts[record.Type]++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if counts["metadata"] != 1 || counts["readiness"] != 1 || counts["sample"] != 3 || counts["summary"] != 1 {
		t.Fatalf("evidence record counts = %#v", counts)
	}
}

func TestCollectSoakSamplesFailsOnMidRunHealthLoss(t *testing.T) {
	t.Parallel()
	identities := testSoakIdentities()
	baseline := testSoakStatus(time.Unix(100, 0))
	hooks := testSoakSamplingHooks(identities)
	statusCalls := 0
	hooks.Status = func(context.Context) (control.Status, error) {
		statusCalls++
		status := testSoakStatus(time.Unix(100+int64(statusCalls), 0))
		if statusCalls == 2 {
			status.Device.Phase = device.PhaseBackoff
		}
		return status, nil
	}

	samples, err := collectSoakSamples(
		context.Background(), 3, time.Second, identities, baseline,
		fakeRequestCounts{Version: 1, StatusStreams: 1, StateMessages: 1, CodexUsage: 1, DisplayDraw: 1}, hooks,
	)
	if err == nil || !strings.Contains(err.Error(), "sample 2 control health") {
		t.Fatalf("collectSoakSamples error = %v, want sample 2 control health failure", err)
	}
	if len(samples) != 1 || statusCalls != 2 {
		t.Fatalf("partial samples = %d, status calls = %d, want 1 and 2", len(samples), statusCalls)
	}
}

func TestCollectSoakSamplesFailsOnTreeChange(t *testing.T) {
	t.Parallel()
	identities := testSoakIdentities()
	baseline := testSoakStatus(time.Unix(100, 0))
	hooks := testSoakSamplingHooks(identities)
	treeCalls := 0
	hooks.Tree = func(context.Context) ([]soak.ProcessIdentity, error) {
		treeCalls++
		tree := append([]soak.ProcessIdentity(nil), identities...)
		if treeCalls == 2 {
			tree = append(tree, soak.ProcessIdentity{PID: 104, PPID: 102, Name: "leaked-child"})
		}
		return tree, nil
	}

	samples, err := collectSoakSamples(
		context.Background(), 3, time.Second, identities, baseline,
		fakeRequestCounts{Version: 1, StatusStreams: 1, StateMessages: 1, CodexUsage: 1, DisplayDraw: 1}, hooks,
	)
	if err == nil || !strings.Contains(err.Error(), "sample 2 process tree changed") {
		t.Fatalf("collectSoakSamples error = %v, want sample 2 child leak failure", err)
	}
	if len(samples) != 1 || treeCalls != 2 {
		t.Fatalf("partial samples = %d, tree calls = %d, want 1 and 2", len(samples), treeCalls)
	}
}

func TestCollectSoakSamplesFailsOnCounterRegression(t *testing.T) {
	t.Parallel()
	identities := testSoakIdentities()
	baseline := testSoakStatus(time.Unix(100, 0))
	hooks := testSoakSamplingHooks(identities)
	countCalls := 0
	hooks.Counts = func() fakeRequestCounts {
		countCalls++
		if countCalls == 2 {
			return fakeRequestCounts{Version: 1, StatusStreams: 1, StateMessages: 2, CodexUsage: 0, DisplayDraw: 1}
		}
		return fakeRequestCounts{Version: 1, StatusStreams: 1, StateMessages: 2, CodexUsage: 1, DisplayDraw: 1}
	}

	samples, err := collectSoakSamples(
		context.Background(), 3, time.Second, identities, baseline,
		fakeRequestCounts{Version: 1, StatusStreams: 1, StateMessages: 1, CodexUsage: 1, DisplayDraw: 1}, hooks,
	)
	if err == nil || !strings.Contains(err.Error(), "sample 2 fake request counter codex_usage regressed") {
		t.Fatalf("collectSoakSamples error = %v, want sample 2 counter regression", err)
	}
	if len(samples) != 1 || countCalls != 2 {
		t.Fatalf("partial samples = %d, count calls = %d, want 1 and 2", len(samples), countCalls)
	}
}

func TestCollectSoakSamplesRechecksHealthForEverySuccessfulSample(t *testing.T) {
	t.Parallel()
	identities := testSoakIdentities()
	baseline := testSoakStatus(time.Unix(100, 0))
	hooks := testSoakSamplingHooks(identities)
	statusCalls := 0
	treeCalls := 0
	countCalls := 0
	hooks.Status = func(context.Context) (control.Status, error) {
		statusCalls++
		return testSoakStatus(time.Unix(100+int64(statusCalls), 0)), nil
	}
	hooks.Tree = func(context.Context) ([]soak.ProcessIdentity, error) {
		treeCalls++
		return append([]soak.ProcessIdentity(nil), identities...), nil
	}
	hooks.Counts = func() fakeRequestCounts {
		countCalls++
		return fakeRequestCounts{Version: 1, StatusStreams: 1, StateMessages: int64(countCalls + 1), CodexUsage: 1, DisplayDraw: 1}
	}

	samples, err := collectSoakSamples(
		context.Background(), 3, time.Second, identities, baseline,
		fakeRequestCounts{Version: 1, StatusStreams: 1, StateMessages: 1, CodexUsage: 1, DisplayDraw: 1}, hooks,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 3 || statusCalls != 3 || treeCalls != 3 || countCalls != 3 {
		t.Fatalf("samples/status/tree/count calls = %d/%d/%d/%d, want 3/3/3/3", len(samples), statusCalls, treeCalls, countCalls)
	}
	for index, sample := range samples {
		if sample.Health.DevicePhase != device.PhaseReady || len(sample.Health.Processes) != 3 || sample.Health.FakeRequests.StateMessages != int64(index+2) {
			t.Fatalf("sample %d health = %#v", index+1, sample.Health)
		}
	}
}

func TestValidateSoakDescendantsRejectsChildLeak(t *testing.T) {
	t.Parallel()
	if err := validateSoakDescendants([]int{102, 103, 104}); err == nil || !strings.Contains(err.Error(), "3 descendants") {
		t.Fatalf("validateSoakDescendants error = %v, want child leak", err)
	}
}

func TestCleanupSoakRuntimeFailsOnSocketLeak(t *testing.T) {
	t.Parallel()
	hooks := soakCleanupHooks{
		Descendants: func(context.Context, int) ([]int, error) { return []int{102, 103}, nil },
		Stop:        func() error { return nil },
		ProcessAlive: func(int) (bool, error) {
			return false, nil
		},
		SocketExists: func(string) (bool, error) { return true, nil },
		Wait:         func(context.Context, time.Duration) error { return context.DeadlineExceeded },
	}
	err := cleanupSoakRuntime(context.Background(), 101, testSoakIdentities(), "/tmp/bsbctl.sock", hooks)
	if err == nil || !strings.Contains(err.Error(), "control socket still exists") {
		t.Fatalf("cleanupSoakRuntime error = %v, want socket leak", err)
	}
}

func TestCleanupSoakRuntimeProvesDaemonChildrenAndDescendantsExit(t *testing.T) {
	t.Parallel()
	checked := make(map[int]int)
	stopCalls := 0
	waitCalls := 0
	hooks := soakCleanupHooks{
		Descendants: func(context.Context, int) ([]int, error) { return []int{102, 103, 104}, nil },
		Stop: func() error {
			stopCalls++
			return nil
		},
		ProcessAlive: func(pid int) (bool, error) {
			checked[pid]++
			return false, nil
		},
		SocketExists: func(string) (bool, error) { return false, nil },
		Wait: func(context.Context, time.Duration) error {
			waitCalls++
			return errors.New("unexpected cleanup wait")
		},
	}
	if err := cleanupSoakRuntime(context.Background(), 101, testSoakIdentities(), "/tmp/bsbctl.sock", hooks); err != nil {
		t.Fatal(err)
	}
	if stopCalls != 1 || waitCalls != 0 {
		t.Fatalf("stop calls = %d, wait calls = %d, want 1 and 0", stopCalls, waitCalls)
	}
	for _, pid := range []int{101, 102, 103, 104} {
		if checked[pid] != 1 {
			t.Fatalf("pid %d checks = %d, want 1", pid, checked[pid])
		}
	}
}

func testSoakSamplingHooks(identities []soak.ProcessIdentity) soakSamplingHooks {
	now := time.Unix(200, 0)
	snapshotCalls := 0
	statusCalls := 0
	return soakSamplingHooks{
		Now: func() time.Time {
			now = now.Add(time.Second)
			return now
		},
		Wait: func(context.Context, time.Duration) error { return nil },
		Snapshot: func(context.Context, []soak.ProcessIdentity) ([]soak.ProcessSnapshot, error) {
			snapshotCalls++
			rows := make([]soak.ProcessSnapshot, 0, len(identities))
			for _, identity := range identities {
				rows = append(rows, soak.ProcessSnapshot{
					PID: identity.PID, PPID: identity.PPID, Name: identity.Name,
					CPUSeconds: float64(snapshotCalls) / 100, RSSBytes: 1024,
				})
			}
			return rows, nil
		},
		Status: func(context.Context) (control.Status, error) {
			statusCalls++
			return testSoakStatus(time.Unix(100+int64(statusCalls), 0)), nil
		},
		Tree: func(context.Context) ([]soak.ProcessIdentity, error) {
			return append([]soak.ProcessIdentity(nil), identities...), nil
		},
		Counts: func() fakeRequestCounts {
			return fakeRequestCounts{Version: 1, StatusStreams: 1, StateMessages: int64(statusCalls + 1), CodexUsage: 1, DisplayDraw: 1}
		},
	}
}

func testSoakIdentities() []soak.ProcessIdentity {
	return []soak.ProcessIdentity{
		{PID: 101, PPID: 1, Name: soak.DaemonName},
		{PID: 103, PPID: 101, Name: soak.CodexQuotaName},
		{PID: 102, PPID: 101, Name: soak.MacResourcesName},
	}
}

func testSoakStatus(lastStateAt time.Time) control.Status {
	return control.Status{
		Device: device.RuntimeStatus{Phase: device.PhaseReady, LastStateAt: lastStateAt},
		Plugins: []pluginhost.PluginStatus{
			{ID: "dev.bsbctl.codex-quota", Phase: pluginhost.PhaseRunning, Running: true, Healthy: true},
			{ID: "dev.bsbctl.mac-resources", Phase: pluginhost.PhaseRunning, Running: true, Healthy: true},
		},
		Readiness: []daemon.AppReadiness{
			{AppID: "codex-quota", Phase: daemon.AppReady},
			{AppID: "mac-resources", Phase: daemon.AppReady},
		},
	}
}
