package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/launchagent"
	"golang.org/x/sys/unix"
)

type localService struct {
	home      string
	status    func(context.Context, string) (launchagent.Result, error)
	command   func(context.Context, string, ...string) ([]byte, error)
	listening func(context.Context) (bool, error)
}

func newLocalService(home string) *localService {
	service := &localService{home: home, status: launchagent.NewManager(nil, os.Getuid()).Status,
		command: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).Output()
		},
	}
	service.listening = func(ctx context.Context) (bool, error) {
		dialer := net.Dialer{Timeout: time.Second}
		connection, err := dialer.DialContext(ctx, "unix", service.socket())
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ECONNREFUSED) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, connection.Close()
	}
	return service
}

func (s *localService) plist() string {
	return filepath.Join(s.home, "Library/LaunchAgents", launchagent.Label+".plist")
}
func (s *localService) socket() string { return filepath.Join(s.home, ".bsbctl/ctl.sock") }
func (s *localService) domain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func (s *localService) inspect(ctx context.Context) (bool, error) {
	result, err := s.status(ctx, s.plist())
	if err != nil {
		return false, fmt.Errorf("inspect LaunchAgent: %w", err)
	}
	if result.Status == launchagent.StateDegraded {
		return false, errors.New("LaunchAgent is degraded; resolve its ownership/configuration before local installation")
	}
	loaded := result.Status == launchagent.StateLoaded
	if result.Status != launchagent.StateNotInstalled {
		if !result.PlistMatches {
			return false, errors.New("LaunchAgent is not owned by bsbctl")
		}
		data, err := s.command(ctx, "/usr/bin/plutil", "-extract", "ProgramArguments", "json", "-o", "-", s.plist())
		if err != nil {
			return false, fmt.Errorf("read LaunchAgent arguments: %w", err)
		}
		var args []string
		if err := json.Unmarshal(data, &args); err != nil {
			return false, errors.New("invalid LaunchAgent arguments")
		}
		want := []string{filepath.Join(s.home, ".local/bin/bsbctl"), "daemon", "--config", filepath.Join(s.home, ".bsbctl/config.json"), "--socket", s.socket()}
		if len(args) < len(want) || !slices.Equal(args[:len(want)], want) || (len(args) != 6 && (len(args) != 8 || args[6] != "--log" || !filepath.IsAbs(args[7]))) {
			return false, errors.New("--local requires the default installed executable, configuration, and socket; custom LaunchAgents are not changed")
		}
	}
	listening, err := s.listening(ctx)
	if err != nil {
		return false, fmt.Errorf("inspect control socket: %w", err)
	}
	if !loaded && listening {
		return false, errors.New("a foreground or unmanaged daemon is running; stop it before local installation")
	}
	return loaded, nil
}

func (s *localService) stop(ctx context.Context) (err error) {
	stopCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	loaded, err := s.inspect(stopCtx)
	if err != nil || !loaded {
		return errors.Join(err, errors.New("service changed before shutdown"))
	}
	data, err := s.command(stopCtx, "/bin/launchctl", "print", s.domain()+"/"+launchagent.Label)
	if err != nil {
		return fmt.Errorf("inspect daemon PID: %w", err)
	}
	pid := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "pid" && fields[1] == "=" {
			pid, err = strconv.Atoi(fields[2])
			if err != nil || pid <= 0 {
				return errors.New("invalid launchd daemon PID")
			}
		}
	}
	defer func() {
		if err != nil {
			resumeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			defer cancel()
			state, statusErr := s.status(resumeCtx, s.plist())
			err = errors.Join(err, statusErr)
			if statusErr == nil && state.Status == launchagent.StateInstalledNotLoaded && state.PlistMatches {
				err = errors.Join(err, s.start(resumeCtx))
			}
		}
	}()
	if _, err := s.command(stopCtx, "/bin/launchctl", "bootout", s.domain()+"/"+launchagent.Label); err != nil {
		return fmt.Errorf("stop LaunchAgent: %w", err)
	}
	for {
		listening, err := s.listening(stopCtx)
		if err != nil {
			return err
		}
		gone := pid == 0 || errors.Is(unix.Kill(pid, 0), unix.ESRCH)
		if gone && !listening {
			return nil
		}
		select {
		case <-stopCtx.Done():
			return fmt.Errorf("daemon shutdown was not confirmed: %w", stopCtx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (s *localService) start(ctx context.Context) error {
	startCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := s.command(startCtx, "/bin/launchctl", "bootstrap", s.domain(), s.plist())
	return err
}

func (s *localService) waitReady(ctx context.Context, document config.Document, version string) error {
	waitCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	for {
		callCtx, cancel := context.WithTimeout(waitCtx, time.Second)
		client, err := control.Dial(callCtx, s.socket())
		var status control.Status
		if err == nil {
			err = client.Call(callCtx, "daemon.status", nil, &status)
			err = errors.Join(err, client.Close())
		}
		cancel()
		if err == nil && localStatusReady(status, document, version) {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func localStatusReady(status control.Status, document config.Document, version string) bool {
	if status.Version != version || status.Generation != document.Generation || status.Device.Phase != device.PhaseReady {
		return false
	}
	for id, app := range document.Apps {
		if !app.Enabled {
			continue
		}
		ready := slices.ContainsFunc(status.Readiness, func(value daemon.AppReadiness) bool { return value.AppID == id && value.Phase == daemon.AppReady })
		healthy := false
		for _, plugin := range status.Plugins {
			if plugin.ID == app.PluginID && plugin.Running && plugin.Healthy {
				healthy = true
			}
		}
		if !ready || !healthy {
			return false
		}
		plugin := document.Plugins[app.PluginID]
		if len(plugin.Assets) != 0 && !slices.ContainsFunc(status.Assets, func(value assets.State) bool {
			return value.PluginID == app.PluginID && value.ObservedVersion == plugin.Version && value.Phase == assets.PhaseReady
		}) {
			return false
		}
	}
	return true
}
