package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/internal/installer"
	"github.com/lxdb/bsbctl/internal/localstate"
	"golang.org/x/sys/unix"
)

type installDependencies struct {
	store     localConfigurationStore
	build     func(context.Context, string, []firstpartyplugins.Descriptor) (config.Document, string, error)
	inspect   func(context.Context) (bool, error)
	stop      func(context.Context) error
	start     func(context.Context) error
	waitReady func(context.Context, config.Document, string) error
}

type localConfigurationStore interface {
	Load() (config.Document, error)
	ReplaceWithOutcome(uint64, config.Document) (localstate.CommitOutcome, error)
}

type installResult struct {
	Directory string
	Installed bool
	Running   bool
}

func installLocal(ctx context.Context, home, apps string, progress io.Writer, deps installDependencies) (result installResult, err error) {
	stateRoot := filepath.Join(home, ".bsbctl")
	configPath := filepath.Join(stateRoot, "config.json")
	corePath := filepath.Join(home, ".local/bin/bsbctl")
	store := deps.store
	if store == nil {
		store = config.NewStore(configPath)
	}
	current, err := store.Load()
	if err != nil {
		return result, fmt.Errorf("an existing valid bsbctl configuration is required: %w", err)
	}
	fd, err := unix.Open(filepath.Join(stateRoot, ".local-install.lock"), unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return result, err
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return result, errors.New("another local installation is in progress")
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	state, err := loadCatalogState(stateRoot)
	if err != nil {
		return result, err
	}
	selected, err := selectLocalPlugins(current, state, apps)
	if err != nil {
		return result, err
	}
	running, err := deps.inspect(ctx)
	if err != nil {
		return result, err
	}
	buildRoot := filepath.Join(stateRoot, "local-builds")
	if err := os.MkdirAll(buildRoot, 0o700); err != nil {
		return result, err
	}
	stage, err := os.MkdirTemp(buildRoot, "install-")
	if err != nil {
		return result, err
	}
	keep := false
	defer func() {
		if !keep {
			err = errors.Join(err, os.RemoveAll(stage))
		}
	}()
	fmt.Fprintln(progress, "Building core and selected local packages before stopping the service...")
	built, version, err := deps.build(ctx, stage, selected)
	if err != nil {
		return result, err
	}
	if _, err := reconcileLocalDocument(current, built, apps); err != nil {
		return result, err
	}
	observedRunning, err := deps.inspect(ctx)
	if err != nil || observedRunning != running {
		return result, errors.Join(err, errors.New("service state changed while building; retry local installation"))
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if running {
		if err := deps.stop(ctx); err != nil {
			return result, err
		}
	}
	mutating, hadCore := false, false
	backup := filepath.Join(stage, "previous")
	// Before commit, restore only this transaction's files and resume the old
	// service. After commit, failed startup/readiness leaves the new install
	// intact for diagnosis instead of silently undoing runtime configuration.
	defer func() {
		if result.Installed {
			return
		}
		var restoreErr error
		if mutating {
			if hadCore {
				restoreErr = replaceLocalFile(filepath.Join(backup, "bsbctl"), corePath, 0o755)
			} else if removeErr := os.Remove(corePath); !errors.Is(removeErr, os.ErrNotExist) {
				restoreErr = errors.Join(restoreErr, removeErr)
			}
		}
		err = errors.Join(err, restoreErr)
		if running && restoreErr == nil {
			resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			defer cancel()
			resumeErr := deps.start(resumeCtx)
			result.Running = resumeErr == nil
			err = errors.Join(err, resumeErr)
		}
	}()
	// The running daemon may have committed user changes during the build.
	// Read again after it stops; never replace those changes with the snapshot.
	current, err = store.Load()
	if err != nil {
		return result, err
	}
	state, err = loadCatalogState(stateRoot)
	if err != nil {
		return result, err
	}
	latestSelection, err := selectLocalPlugins(current, state, apps)
	if err != nil || !sameLocalSelection(selected, latestSelection) {
		return result, errors.Join(err, errors.New("configured packages changed while building; retry local installation"))
	}
	next, err := reconcileLocalDocument(current, built, apps)
	if err != nil {
		return result, err
	}
	if err := copyLocalFile(configPath, filepath.Join(backup, "config.json"), 0o600); err != nil {
		return result, err
	}
	if _, statErr := os.Lstat(corePath); statErr == nil {
		if err := copyLocalFile(corePath, filepath.Join(backup, "bsbctl"), 0o755); err != nil {
			return result, err
		}
		hadCore = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return result, statErr
	}
	keep, result.Directory = true, stage
	if err := ctx.Err(); err != nil {
		return result, err
	}
	mutating = true
	if err := replaceLocalFile(filepath.Join(stage, "bsbctl"), corePath, 0o755); err != nil {
		return result, err
	}
	outcome, err := store.ReplaceWithOutcome(current.Generation, next)
	result.Installed = outcome.IsCommitted()
	if err != nil {
		if result.Installed {
			return result, fmt.Errorf("local configuration committed with an error; new files retained and service left stopped: %w", err)
		}
		return result, fmt.Errorf("register local packages: %w", err)
	}
	if running {
		if err := deps.start(ctx); err != nil {
			return result, fmt.Errorf("local files installed but service start failed: %w", err)
		}
		result.Running = true
		fmt.Fprintln(progress, "Local files installed; waiting for daemon, enabled apps, and device readiness...")
		if err := deps.waitReady(ctx, next, version); err != nil {
			return result, fmt.Errorf("local files installed but readiness was not verified: %w", err)
		}
	}
	return result, nil
}

func loadCatalogState(stateRoot string) (installer.InstallState, error) {
	root := filepath.Join(stateRoot, "installer")
	if _, err := os.Lstat(filepath.Join(root, "operation-journal.json")); !errors.Is(err, os.ErrNotExist) {
		return installer.InstallState{}, errors.New("catalog recovery must finish before local installation")
	}
	return installer.NewStateStore(root).LoadState()
}

func sameLocalSelection(left, right []firstpartyplugins.Descriptor) bool {
	return slices.EqualFunc(left, right, func(a, b firstpartyplugins.Descriptor) bool { return a.ID == b.ID })
}

func copyLocalFile(source, destination string, mode os.FileMode) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", source)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	return errors.Join(copyErr, output.Sync(), output.Close())
}

func replaceLocalFile(source, destination string, mode os.FileMode) error {
	if info, err := os.Lstat(destination); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to replace non-regular file: %s", destination)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory := filepath.Dir(destination)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp(directory, ".bsbctl-replace-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	path := filepath.Join(temporary, "file")
	if err := copyLocalFile(source, path, mode); err != nil {
		return err
	}
	if err := os.Rename(path, destination); err != nil {
		return err
	}
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}
