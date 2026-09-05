package launchagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/lxdb/bsbctl/internal/localstate"
	"golang.org/x/sys/unix"
)

var ErrNotLoaded = errors.New("LaunchAgent is not loaded")
var ErrPartial = errors.New("service operation partially completed")

type State string

const (
	StateNotInstalled       State = "not_installed"
	StateInstalledNotLoaded State = "installed_not_loaded"
	StateLoaded             State = "loaded"
	StateDegraded           State = "degraded"
)

type Result struct {
	Status       State `json:"status"`
	PlistMatches bool  `json:"plist_matches"`
	Changed      bool  `json:"changed,omitempty"`
}

type Runner interface {
	Run(context.Context, ...string) error
}

type Manager struct {
	runner         Runner
	domain         string
	uid            int
	writeOptions   writeOptions
	removeOptions  removeOptions
	lockOptions    lifecycleLockOptions
	beforeMutation func()
}

func NewManager(runner Runner, uid int) *Manager {
	if runner == nil {
		runner = commandRunner{}
	}
	if uid < 0 {
		uid = os.Getuid()
	}
	return &Manager{runner: runner, domain: "gui/" + strconv.Itoa(uid), uid: uid}
}

func (m *Manager) Install(ctx context.Context, path string, config Config) (Result, error) {
	lifecycle, err := acquirePlistLifecycle(ctx, path, m.uid, m.lockOptions)
	if err != nil {
		return Result{Status: StateDegraded}, managerLifecycleError(err)
	}
	result, operationErr := m.installLocked(ctx, path, config, lifecycle)
	if err := lifecycle.close(); err != nil {
		return result, ErrPartial
	}
	return result, operationErr
}

func (m *Manager) installLocked(ctx context.Context, path string, config Config, lifecycle *plistLifecycle) (Result, error) {
	desired, err := render(config)
	if err != nil {
		return Result{}, errors.New("LaunchAgent configuration is invalid")
	}
	previous, err := m.readOwnedPlist(lifecycle)
	if err != nil {
		return Result{Status: StateDegraded}, errors.New("LaunchAgent plist is invalid")
	}
	if previous.exists && !previous.owned {
		return Result{Status: StateDegraded}, errors.New("LaunchAgent plist has a foreign owner")
	}
	loaded, err := m.loaded(ctx)
	if err != nil {
		return Result{Status: StateDegraded, PlistMatches: previous.exists && previous.protected}, err
	}
	identical := previous.exists && previous.protected && bytes.Equal(previous.data, desired)
	if identical && !lifecycle.pathStillPinned() {
		return Result{Status: StateDegraded}, ErrPartial
	}
	if identical && loaded {
		return Result{Status: StateLoaded, PlistMatches: true}, nil
	}
	if identical {
		if err := m.run(ctx, "bootstrap", m.domain, path); err != nil {
			result, observedErr := m.observeOperation(ctx, lifecycle, false)
			if observedErr != nil || result.Status == StateLoaded {
				return result, ErrPartial
			}
			return result, err
		}
		return Result{Status: StateLoaded, PlistMatches: true}, nil
	}
	var expected *entryExpectation
	if previous.exists {
		value := expectationFromRecord(previous)
		expected = &value
	}
	outcome, writeErr := writeDataPinned(lifecycle, desired, expected, m.currentWriteOptions())
	if !lifecycle.pathStillPinned() {
		return Result{Status: StateDegraded, Changed: outcome.IsCommitted()}, ErrPartial
	}
	if writeErr != nil {
		result, _ := m.observeOperation(ctx, lifecycle, outcome.IsCommitted())
		if outcome.IsCommitted() {
			return result, ErrPartial
		}
		return result, errors.New("LaunchAgent plist update failed")
	}
	if previous.exists && bytes.Equal(previous.data, desired) && loaded {
		return Result{Status: StateLoaded, PlistMatches: true, Changed: true}, nil
	}
	if loaded {
		if err := m.run(ctx, "bootout", m.domain, path); err != nil {
			return m.rollbackInstall(ctx, path, lifecycle, previous, true, err)
		}
	}
	if err := m.run(ctx, "bootstrap", m.domain, path); err != nil {
		return m.rollbackInstall(ctx, path, lifecycle, previous, loaded, err)
	}
	return Result{Status: StateLoaded, PlistMatches: true, Changed: true}, nil
}

func (m *Manager) Uninstall(ctx context.Context, path string) (Result, error) {
	lifecycle, err := acquirePlistLifecycle(ctx, path, m.uid, m.lockOptions)
	if err != nil {
		return Result{Status: StateDegraded}, managerLifecycleError(err)
	}
	result, operationErr := m.uninstallLocked(ctx, path, lifecycle)
	if err := lifecycle.close(); err != nil {
		return result, ErrPartial
	}
	return result, operationErr
}

// Restart asks launchd to terminate and immediately re-exec the currently
// loaded, bsbctl-owned service without changing its plist or desired state.
func (m *Manager) Restart(ctx context.Context, path string) (Result, error) {
	lifecycle, err := acquirePlistLifecycle(ctx, path, m.uid, m.lockOptions)
	if err != nil {
		return Result{Status: StateDegraded}, managerLifecycleError(err)
	}
	result, operationErr := m.restartLocked(ctx, lifecycle)
	if err := lifecycle.close(); err != nil {
		return result, ErrPartial
	}
	return result, operationErr
}

func (m *Manager) restartLocked(ctx context.Context, lifecycle *plistLifecycle) (Result, error) {
	record, err := m.readOwnedPlist(lifecycle)
	if err != nil || !record.exists || !record.owned || !record.protected {
		return Result{Status: StateDegraded}, errors.New("LaunchAgent plist is unavailable or invalid")
	}
	loaded, err := m.loaded(ctx)
	if err != nil {
		return Result{Status: StateDegraded, PlistMatches: true}, err
	}
	if !lifecycle.pathStillPinned() {
		return Result{Status: StateDegraded}, errors.New("LaunchAgent directory identity changed")
	}
	if !loaded {
		return Result{Status: StateInstalledNotLoaded, PlistMatches: true}, ErrNotLoaded
	}
	if err := m.run(ctx, "kickstart", "-k", m.domain+"/"+Label); err != nil {
		result, _ := m.observeOperation(ctx, lifecycle, false)
		return result, err
	}
	result, observedErr := m.observeOperation(ctx, lifecycle, true)
	if observedErr != nil || result.Status != StateLoaded || !result.PlistMatches {
		return result, ErrPartial
	}
	return result, nil
}

func (m *Manager) uninstallLocked(ctx context.Context, path string, lifecycle *plistLifecycle) (Result, error) {
	previous, err := m.readOwnedPlist(lifecycle)
	if err != nil {
		return Result{Status: StateDegraded}, errors.New("LaunchAgent plist is invalid")
	}
	if previous.exists && !previous.owned {
		return Result{Status: StateDegraded}, errors.New("LaunchAgent plist has a foreign owner")
	}
	loaded, err := m.loaded(ctx)
	if err != nil {
		return Result{Status: StateDegraded, PlistMatches: previous.exists && previous.protected}, err
	}
	if !lifecycle.pathStillPinned() {
		return Result{Status: StateDegraded}, ErrPartial
	}
	if loaded {
		if err := m.run(ctx, "bootout", m.domain, path); err != nil {
			result, observedErr := m.observeOperation(ctx, lifecycle, false)
			if observedErr != nil || result.Status != StateLoaded {
				return result, ErrPartial
			}
			return result, err
		}
	}
	if previous.exists {
		outcome, removeErr := removeDataPinned(lifecycle, expectationFromRecord(previous), m.currentRemoveOptions())
		if !lifecycle.pathStillPinned() {
			return Result{Status: StateDegraded, Changed: loaded || outcome.IsCommitted()}, ErrPartial
		}
		if removeErr != nil {
			result, _ := m.observeOperation(ctx, lifecycle, loaded || outcome.IsCommitted())
			if loaded || outcome.IsCommitted() {
				return result, ErrPartial
			}
			return result, errors.New("LaunchAgent plist removal failed")
		}
	}
	return Result{Status: StateNotInstalled, Changed: loaded || previous.exists}, nil
}

func (m *Manager) Status(ctx context.Context, path string) (Result, error) {
	lifecycle, err := acquirePlistLifecycle(ctx, path, m.uid, m.lockOptions)
	if err != nil {
		return Result{Status: StateDegraded}, managerLifecycleError(err)
	}
	result, operationErr := m.statusLocked(ctx, lifecycle)
	if err := lifecycle.close(); err != nil {
		return Result{Status: StateDegraded}, errors.New("LaunchAgent lifecycle lock release failed")
	}
	return result, operationErr
}

func managerLifecycleError(err error) error {
	if err == context.Canceled || err == context.DeadlineExceeded {
		return err
	}
	return errors.New("LaunchAgent lifecycle lock is invalid")
}

func (m *Manager) statusLocked(ctx context.Context, lifecycle *plistLifecycle) (Result, error) {
	record, err := m.readOwnedPlist(lifecycle)
	if err != nil {
		return Result{Status: StateDegraded}, nil
	}
	if record.exists && !record.owned {
		return Result{Status: StateDegraded}, nil
	}
	loaded, err := m.loaded(ctx)
	if err != nil {
		return Result{Status: StateDegraded, PlistMatches: record.exists && record.protected}, err
	}
	if !lifecycle.pathStillPinned() {
		return Result{Status: StateDegraded}, errors.New("LaunchAgent directory identity changed")
	}
	return resultFor(record, loaded, false), nil
}

func (m *Manager) currentWriteOptions() writeOptions {
	options := m.writeOptions
	configuredHook := options.beforeRename
	options.beforeRename = func() {
		if configuredHook != nil {
			configuredHook()
		}
		if m.beforeMutation != nil {
			m.beforeMutation()
		}
	}
	return options
}

func (m *Manager) currentRemoveOptions() removeOptions {
	options := m.removeOptions
	configuredHook := options.beforeRemove
	options.beforeRemove = func() {
		if configuredHook != nil {
			configuredHook()
		}
		if m.beforeMutation != nil {
			m.beforeMutation()
		}
	}
	return options
}

func (m *Manager) rollbackInstall(ctx context.Context, path string, lifecycle *plistLifecycle, previous plistRecord, wasLoaded bool, cause error) (Result, error) {
	current, currentErr := m.readOwnedPlist(lifecycle)
	if currentErr != nil || !current.exists {
		result, _ := m.observeOperation(ctx, lifecycle, true)
		return result, ErrPartial
	}
	var outcome localstate.CommitOutcome
	var restoreErr error
	if previous.exists {
		expected := expectationFromRecord(current)
		outcome, restoreErr = writeDataPinned(lifecycle, previous.data, &expected, m.currentWriteOptions())
	} else {
		outcome, restoreErr = removeDataPinned(lifecycle, expectationFromRecord(current), m.currentRemoveOptions())
	}
	if restoreErr != nil || outcome != localstate.Committed {
		result, _ := m.observeOperation(ctx, lifecycle, true)
		return result, ErrPartial
	}
	if wasLoaded {
		stillLoaded, queryErr := m.loaded(ctx)
		if queryErr != nil {
			result, _ := m.observeOperation(ctx, lifecycle, true)
			return result, ErrPartial
		}
		if !stillLoaded {
			if err := m.run(ctx, "bootstrap", m.domain, path); err != nil {
				result, _ := m.observeOperation(ctx, lifecycle, true)
				return result, ErrPartial
			}
		}
	}
	result, observedErr := m.observeOperation(ctx, lifecycle, true)
	if observedErr != nil || !sameObservedState(previous, wasLoaded, result) {
		return result, ErrPartial
	}
	return result, cause
}

func sameObservedState(previous plistRecord, wasLoaded bool, result Result) bool {
	if wasLoaded {
		return result.Status == StateLoaded && result.PlistMatches == previous.protected
	}
	if !previous.exists {
		return result.Status == StateNotInstalled
	}
	return result.Status == StateInstalledNotLoaded && result.PlistMatches == previous.protected
}

func (m *Manager) observeOperation(ctx context.Context, lifecycle *plistLifecycle, changed bool) (Result, error) {
	record, readErr := m.readOwnedPlist(lifecycle)
	loaded, loadedErr := m.loaded(ctx)
	if readErr != nil || loadedErr != nil {
		return Result{Status: StateDegraded, Changed: changed}, errors.Join(readErr, loadedErr)
	}
	return resultFor(record, loaded, changed), nil
}

func resultFor(record plistRecord, loaded, changed bool) Result {
	result := Result{PlistMatches: record.exists && record.protected, Changed: changed}
	switch {
	case loaded && result.PlistMatches:
		result.Status = StateLoaded
	case loaded || (record.exists && !record.protected):
		result.Status = StateDegraded
	case record.exists:
		result.Status = StateInstalledNotLoaded
	default:
		result.Status = StateNotInstalled
	}
	return result
}

func (m *Manager) loaded(ctx context.Context) (bool, error) {
	err := m.runner.Run(ctx, "print", m.domain+"/"+Label)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrNotLoaded) {
		return false, nil
	}
	return false, safeRunnerError(ctx, err)
}

func (m *Manager) run(ctx context.Context, args ...string) error {
	return safeRunnerError(ctx, m.runner.Run(ctx, args...))
}

func safeRunnerError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.New("launchctl operation failed")
}

const maxLaunchAgentPlistBytes = 64 << 10

type plistRecord struct {
	data      []byte
	identity  fileIdentity
	exists    bool
	owned     bool
	protected bool
	uid       uint32
	mode      uint32
}

func expectationFromRecord(record plistRecord) entryExpectation {
	return entryExpectation{identity: record.identity, data: slices.Clone(record.data), uid: record.uid, mode: record.mode, exact: true}
}

func (m *Manager) readOwnedPlist(lifecycle *plistLifecycle) (plistRecord, error) {
	var pathStat unix.Stat_t
	err := unix.Fstatat(lifecycle.directoryFD, lifecycle.name, &pathStat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return plistRecord{}, nil
	}
	if err != nil {
		return plistRecord{}, errors.New("invalid plist")
	}
	if uint32(pathStat.Mode)&unix.S_IFMT != unix.S_IFREG {
		return plistRecord{}, errors.New("invalid plist")
	}
	fd, err := unix.Openat(lifecycle.directoryFD, lifecycle.name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return plistRecord{}, nil
	}
	if err != nil {
		return plistRecord{}, errors.New("invalid plist")
	}
	file := os.NewFile(uintptr(fd), lifecycle.name)
	if file == nil {
		_ = unix.Close(fd)
		return plistRecord{}, errors.New("invalid plist")
	}
	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Size < 1 || before.Size > maxLaunchAgentPlistBytes {
		_ = file.Close()
		return plistRecord{}, errors.New("invalid plist")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxLaunchAgentPlistBytes+1))
	var after unix.Stat_t
	statErr := unix.Fstat(fd, &after)
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil || identityFromStat(before) != identityFromStat(after) || before.Size != after.Size || int64(len(data)) != after.Size || len(data) < 1 || len(data) > maxLaunchAgentPlistBytes {
		return plistRecord{}, errors.New("invalid plist")
	}
	if !plistMatchesLabel(data) {
		return plistRecord{}, errors.New("plist is not owned by bsbctl")
	}
	owned := int(after.Uid) == m.uid
	return plistRecord{
		data: data, identity: identityFromStat(after), exists: true, owned: owned,
		protected: owned && uint32(after.Mode)&0o7777 == 0o600,
		uid:       after.Uid, mode: uint32(after.Mode) & 0o7777,
	}, nil
}

type removeOptions struct {
	beforeRemove    func()
	renameExclusive func(int, string, string) error
	unlink          func(int, string) error
	syncDirectory   func(int) error
}

func removeDataPinned(lifecycle *plistLifecycle, expected entryExpectation, options removeOptions) (localstate.CommitOutcome, error) {
	quarantineName, err := newQuarantineName()
	if err != nil {
		return localstate.NotCommitted, err
	}
	if options.beforeRemove != nil {
		options.beforeRemove()
	}
	renameExclusive := options.renameExclusive
	if renameExclusive == nil {
		renameExclusive = platformRenameExclusive
	}
	unlink := options.unlink
	if unlink == nil {
		unlink = func(directoryFD int, name string) error { return unix.Unlinkat(directoryFD, name, 0) }
	}
	if err := renameExclusive(lifecycle.directoryFD, lifecycle.name, quarantineName); err != nil {
		return localstate.NotCommitted, err
	}
	displaced, displacedErr := fingerprintAt(lifecycle.directoryFD, quarantineName)
	validationErr := validateExpectedEntryAt(lifecycle.directoryFD, quarantineName, expected)
	if validationErr != nil {
		if err := renameExclusive(lifecycle.directoryFD, quarantineName, lifecycle.name); err != nil {
			return finishRemoveDirectory(lifecycle, options, localstate.CommittedDurabilityUncertain, errors.Join(validationErr, errors.New("LaunchAgent plist restoration failed"), err))
		}
		restored := displacedErr == nil && entryHasFingerprintAt(lifecycle.directoryFD, lifecycle.name, displaced)
		outcome := localstate.NotCommitted
		if !restored {
			outcome = localstate.CommittedDurabilityUncertain
		}
		return finishRemoveDirectory(lifecycle, options, outcome, errors.Join(validationErr, displacedErr))
	}
	if err := unlinkKnownEntry(lifecycle.directoryFD, quarantineName, expected.identity, unlink); err != nil {
		return finishRemoveDirectory(lifecycle, options, localstate.CommittedDurabilityUncertain, err)
	}
	return finishRemoveDirectory(lifecycle, options, localstate.Committed, nil)
}

func newQuarantineName() (string, error) {
	return newPrivateName(".bsbctl-launchagent-quarantine-")
}

func newPrivateName(prefix string) (string, error) {
	var random [16]byte
	if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func finishRemoveDirectory(lifecycle *plistLifecycle, options removeOptions, outcome localstate.CommitOutcome, operationErr error) (localstate.CommitOutcome, error) {
	syncDirectory := options.syncDirectory
	if syncDirectory == nil {
		syncDirectory = unix.Fsync
	}
	if err := syncDirectory(lifecycle.directoryFD); err != nil {
		return localstate.CommittedDurabilityUncertain, errors.Join(operationErr, err)
	}
	return outcome, operationErr
}

type plistXMLNode struct {
	name     string
	text     strings.Builder
	children []*plistXMLNode
}

func plistMatchesLabel(data []byte) bool {
	root, err := parsePlistXML(data)
	if err != nil || root.name != "plist" || len(root.children) != 1 || root.children[0].name != "dict" {
		return false
	}
	top := root.children[0]
	labelKeys := make([]struct {
		parent *plistXMLNode
		index  int
	}, 0, 1)
	invalidLabel := false
	var visit func(*plistXMLNode)
	visit = func(parent *plistXMLNode) {
		for index, child := range parent.children {
			if child.name == "key" && strings.TrimSpace(child.text.String()) == "Label" {
				if len(child.children) != 0 {
					invalidLabel = true
					continue
				}
				labelKeys = append(labelKeys, struct {
					parent *plistXMLNode
					index  int
				}{parent: parent, index: index})
			}
			visit(child)
		}
	}
	visit(root)
	if invalidLabel || len(labelKeys) != 1 || labelKeys[0].parent != top || labelKeys[0].index+1 >= len(top.children) {
		return false
	}
	value := top.children[labelKeys[0].index+1]
	return value.name == "string" && len(value.children) == 0 && strings.TrimSpace(value.text.String()) == Label
}

func parsePlistXML(data []byte) (*plistXMLNode, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var root *plistXMLNode
	stack := make([]*plistXMLNode, 0, 8)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			node := &plistXMLNode{name: value.Name.Local}
			if len(stack) == 0 {
				if root != nil {
					return nil, errors.New("multiple plist roots")
				}
				root = node
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, node)
			}
			stack = append(stack, node)
		case xml.CharData:
			if len(stack) != 0 {
				_, _ = stack[len(stack)-1].text.Write(value)
			} else if strings.TrimSpace(string(value)) != "" {
				return nil, errors.New("text outside plist root")
			}
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1].name != value.Name.Local {
				return nil, errors.New("invalid plist nesting")
			}
			stack = stack[:len(stack)-1]
		}
	}
	if root == nil || len(stack) != 0 {
		return nil, errors.New("invalid plist")
	}
	return root, nil
}

const maxLaunchctlOutput = 16 << 10

type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, args ...string) error {
	command := exec.CommandContext(ctx, "launchctl", args...)
	output := &boundedBuffer{remaining: maxLaunchctlOutput}
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	if err == nil {
		return nil
	}
	if len(args) != 0 && args[0] == "print" && (bytes.Contains(output.data, []byte("Could not find service")) || bytes.Contains(output.data, []byte("service not found"))) {
		return ErrNotLoaded
	}
	return err
}

type boundedBuffer struct {
	data      []byte
	remaining int
}

func (w *boundedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	if len(data) > w.remaining {
		data = data[:w.remaining]
	}
	w.data = append(w.data, data...)
	w.remaining -= len(data)
	return original, nil
}
