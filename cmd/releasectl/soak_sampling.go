package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/soak"
)

func collectSoakSamples(
	ctx context.Context,
	count int,
	interval time.Duration,
	identities []soak.ProcessIdentity,
	previousStatus control.Status,
	previousCounts fakeRequestCounts,
	hooks soakSamplingHooks,
) ([]soakSampleRecord, error) {
	if count < 1 || interval <= 0 || len(identities) == 0 || hooks.Now == nil || hooks.Wait == nil || hooks.Snapshot == nil || hooks.Status == nil || hooks.Tree == nil || hooks.Counts == nil {
		return nil, errors.New("soak sampling hooks are incomplete")
	}
	records := make([]soakSampleRecord, 0, count)
	for index := 1; index <= count; index++ {
		before, err := hooks.Snapshot(ctx, identities)
		if err != nil {
			return records, fmt.Errorf("sample %d initial process telemetry: %w", index, err)
		}
		intervalStarted := hooks.Now()
		if err := hooks.Wait(ctx, interval); err != nil {
			return records, fmt.Errorf("sample %d interrupted: %w", index, err)
		}
		after, err := hooks.Snapshot(ctx, identities)
		capturedAt := hooks.Now()
		if err != nil {
			return records, fmt.Errorf("sample %d final process telemetry: %w", index, err)
		}
		status, err := hooks.Status(ctx)
		if err != nil {
			return records, fmt.Errorf("sample %d control health: %w", index, err)
		}
		actualIdentities, err := hooks.Tree(ctx)
		if err != nil {
			return records, fmt.Errorf("sample %d process tree: %w", index, err)
		}
		counts := hooks.Counts()
		if err := validateSoakSampleHealth(previousStatus, status, previousCounts, counts, identities, actualIdentities); err != nil {
			return records, fmt.Errorf("sample %d %w", index, err)
		}
		sample, err := soak.MeasureInterval(index, intervalStarted, capturedAt, identities, before, after)
		if err != nil {
			return records, fmt.Errorf("sample %d accounting: %w", index, err)
		}
		records = append(records, soakSampleRecord{
			Sample: sample,
			Health: runtimeHealthEvidence(status, actualIdentities, counts),
		})
		previousStatus = status
		previousCounts = counts
	}
	return records, nil
}

func validateSoakSampleHealth(
	previousStatus, status control.Status,
	previousCounts, counts fakeRequestCounts,
	expectedIdentities, actualIdentities []soak.ProcessIdentity,
) error {
	if !statusReady(status) {
		return errors.New("control health is not ready")
	}
	if status.Device.LastStateAt.Before(previousStatus.Device.LastStateAt) {
		return errors.New("device state timestamp regressed")
	}
	if err := validateFakeRequestCounts(previousCounts, counts); err != nil {
		return err
	}
	if !sameProcessIdentities(expectedIdentities, actualIdentities) {
		return errors.New("process tree changed")
	}
	return nil
}

func validateFakeRequestCounts(previous, current fakeRequestCounts) error {
	fields := []struct {
		name     string
		previous int64
		current  int64
	}{
		{name: "version", previous: previous.Version, current: current.Version},
		{name: "status_streams", previous: previous.StatusStreams, current: current.StatusStreams},
		{name: "state_messages", previous: previous.StateMessages, current: current.StateMessages},
		{name: "codex_usage", previous: previous.CodexUsage, current: current.CodexUsage},
		{name: "display_draw", previous: previous.DisplayDraw, current: current.DisplayDraw},
		{name: "display_clear", previous: previous.DisplayClear, current: current.DisplayClear},
	}
	for _, field := range fields {
		if field.current < field.previous {
			return fmt.Errorf("fake request counter %s regressed", field.name)
		}
	}
	if current.Version == 0 || current.StatusStreams == 0 || current.StateMessages == 0 || current.CodexUsage == 0 || current.DisplayDraw == 0 {
		return errors.New("intended fake workload is not ready")
	}
	return nil
}

func sameProcessIdentities(left, right []soak.ProcessIdentity) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func discoverDaemonTree(ctx context.Context, parentPID int) ([]soak.ProcessIdentity, error) {
	descendants, err := listDescendantPIDs(ctx, parentPID)
	if err != nil {
		return nil, err
	}
	if err := validateSoakDescendants(descendants); err != nil {
		return nil, err
	}
	pids := append([]int{parentPID}, descendants...)
	rows, err := processSnapshotForPIDs(ctx, pids)
	if err != nil {
		return nil, err
	}
	return soak.SelectDaemonTree(rows, parentPID)
}

func validateSoakDescendants(descendants []int) error {
	if len(descendants) != 2 {
		return fmt.Errorf("bsbctl process tree has %d descendants, want exactly 2 resident plugins", len(descendants))
	}
	seen := make(map[int]struct{}, len(descendants))
	for _, pid := range descendants {
		if pid <= 0 {
			return errors.New("descendant process telemetry is invalid")
		}
		if _, exists := seen[pid]; exists {
			return errors.New("descendant process telemetry repeats a pid")
		}
		seen[pid] = struct{}{}
	}
	return nil
}

func listDescendantPIDs(ctx context.Context, parentPID int) ([]int, error) {
	if parentPID <= 0 {
		return nil, errors.New("process parent pid is invalid")
	}
	const maximumDescendants = 64
	seen := map[int]struct{}{parentPID: {}}
	queue := []int{parentPID}
	descendants := make([]int, 0, 2)
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		children, err := directChildPIDs(ctx, parent)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if _, exists := seen[child]; exists {
				return nil, errors.New("descendant process telemetry repeats a pid")
			}
			seen[child] = struct{}{}
			descendants = append(descendants, child)
			if len(descendants) > maximumDescendants {
				return nil, errors.New("descendant process telemetry exceeds the bounded inventory")
			}
			queue = append(queue, child)
		}
	}
	return descendants, nil
}

func directChildPIDs(ctx context.Context, parentPID int) ([]int, error) {
	command := exec.CommandContext(ctx, "pgrep", "-P", strconv.Itoa(parentPID))
	command.Env = os.Environ()
	output := &boundedOutput{remaining: soakCommandOutputLimit}
	command.Stdout = output
	command.Stderr = output
	err := command.Run()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 && strings.TrimSpace(output.String()) == "" {
			return nil, nil
		}
		return nil, errors.New("descendant process telemetry is unavailable")
	}
	children := make([]int, 0, 2)
	for _, field := range strings.Fields(output.String()) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid <= 0 {
			return nil, errors.New("descendant process telemetry is invalid")
		}
		children = append(children, pid)
	}
	return children, nil
}

func processSnapshot(ctx context.Context, identities []soak.ProcessIdentity) ([]soak.ProcessSnapshot, error) {
	pids := make([]int, 0, len(identities))
	for _, identity := range identities {
		pids = append(pids, identity.PID)
	}
	return processSnapshotForPIDs(ctx, pids)
}

func processSnapshotForPIDs(ctx context.Context, pids []int) ([]soak.ProcessSnapshot, error) {
	if len(pids) == 0 {
		return nil, errors.New("process pid inventory is empty")
	}
	args := []string{"-o", "pid=,ppid=,time=,rss=,comm="}
	for _, pid := range pids {
		if pid <= 0 {
			return nil, errors.New("process pid inventory is invalid")
		}
		args = append(args, "-p", strconv.Itoa(pid))
	}
	output, err := commandText(ctx, "", os.Environ(), "ps", args...)
	if err != nil {
		return nil, errors.New("ps process telemetry is unavailable")
	}
	rows, err := soak.ParseProcessSnapshot(strings.NewReader(output))
	if err != nil {
		return nil, err
	}
	if len(rows) != len(pids) {
		return nil, errors.New("ps process telemetry omitted a target process")
	}
	return rows, nil
}
