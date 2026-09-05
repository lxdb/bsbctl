package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/lxdb/bsbctl/internal/soak"
)

func cleanupSoakDaemon(process *soakDaemon, identities []soak.ProcessIdentity, socketPath string) error {
	if process == nil || process.command == nil || process.command.Process == nil {
		return errors.New("bsbctl daemon process is unavailable for cleanup")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return cleanupSoakRuntime(ctx, process.command.Process.Pid, identities, socketPath, soakCleanupHooks{
		Descendants:  listDescendantPIDs,
		Stop:         process.Stop,
		ProcessAlive: soakProcessAlive,
		SocketExists: soakSocketExists,
		Wait:         waitBounded,
	})
}

func cleanupSoakRuntime(
	ctx context.Context,
	daemonPID int,
	identities []soak.ProcessIdentity,
	socketPath string,
	hooks soakCleanupHooks,
) error {
	if daemonPID <= 0 || strings.TrimSpace(socketPath) == "" || hooks.Descendants == nil || hooks.Stop == nil || hooks.ProcessAlive == nil || hooks.SocketExists == nil || hooks.Wait == nil {
		return errors.New("soak cleanup hooks are incomplete")
	}
	descendants, inventoryErr := hooks.Descendants(ctx, daemonPID)
	targetSet := map[int]struct{}{daemonPID: {}}
	for _, identity := range identities {
		if identity.PID > 0 {
			targetSet[identity.PID] = struct{}{}
		}
	}
	for _, pid := range descendants {
		if pid <= 0 {
			inventoryErr = errors.Join(inventoryErr, errors.New("cleanup descendant inventory is invalid"))
			continue
		}
		targetSet[pid] = struct{}{}
	}
	targets := make([]int, 0, len(targetSet))
	for pid := range targetSet {
		targets = append(targets, pid)
	}
	sort.Ints(targets)
	stopErr := hooks.Stop()
	for {
		leaks := make([]string, 0)
		var probeErr error
		for _, pid := range targets {
			alive, err := hooks.ProcessAlive(pid)
			if err != nil {
				probeErr = errors.Join(probeErr, fmt.Errorf("probe process %d after cleanup: %w", pid, err))
				continue
			}
			if alive {
				leaks = append(leaks, fmt.Sprintf("process %d still exists", pid))
			}
		}
		socketExists, err := hooks.SocketExists(socketPath)
		if err != nil {
			probeErr = errors.Join(probeErr, fmt.Errorf("probe control socket after cleanup: %w", err))
		} else if socketExists {
			leaks = append(leaks, "control socket still exists")
		}
		if len(leaks) == 0 && probeErr == nil {
			return errors.Join(inventoryErr, stopErr)
		}
		var leakErr error
		if len(leaks) > 0 {
			leakErr = errors.New(strings.Join(leaks, "; "))
		}
		proofErr := errors.Join(probeErr, leakErr)
		if err := hooks.Wait(ctx, 50*time.Millisecond); err != nil {
			return errors.Join(inventoryErr, stopErr, proofErr, fmt.Errorf("cleanup proof interrupted: %w", err))
		}
	}
}

func soakProcessAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, errors.New("process pid is invalid")
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return false, err
}

func soakSocketExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func waitBounded(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
