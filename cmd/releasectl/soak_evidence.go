package main

import (
	"bufio"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"slices"
	"time"

	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/soak"
)

func readyEvidence(status control.Status, identities []soak.ProcessIdentity, counts fakeRequestCounts, startup time.Duration) soakReadiness {
	health := runtimeHealthEvidence(status, identities, counts)
	return soakReadiness{
		StartupMilliseconds: startup.Milliseconds(), DevicePhase: health.DevicePhase,
		DeviceStateObserved: health.DeviceStateObserved, Plugins: health.Plugins, Apps: health.Apps,
		Processes: health.Processes, FakeRequests: health.FakeRequests,
	}
}

func runtimeHealthEvidence(status control.Status, identities []soak.ProcessIdentity, counts fakeRequestCounts) soakRuntimeHealth {
	plugins := make([]soakReadyPlugin, 0, len(status.Plugins))
	for _, plugin := range status.Plugins {
		plugins = append(plugins, soakReadyPlugin{ID: plugin.ID, Phase: plugin.Phase, Running: plugin.Running, Healthy: plugin.Healthy})
	}
	slices.SortFunc(plugins, func(left, right soakReadyPlugin) int { return cmp.Compare(left.ID, right.ID) })
	apps := make([]soakReadyApp, 0, len(status.Readiness))
	for _, app := range status.Readiness {
		apps = append(apps, soakReadyApp{AppID: app.AppID, Phase: app.Phase})
	}
	slices.SortFunc(apps, func(left, right soakReadyApp) int { return cmp.Compare(left.AppID, right.AppID) })
	return soakRuntimeHealth{
		DevicePhase:         status.Device.Phase,
		DeviceStateObserved: !status.Device.LastStateAt.IsZero(), Plugins: plugins, Apps: apps,
		Processes: slices.Clone(identities), FakeRequests: counts,
	}
}

func writeSoakEvidence(path string, evidence soakEvidence, soakErr error) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	buffer := bufio.NewWriter(file)
	encoder := json.NewEncoder(buffer)
	write := func(record any) error { return encoder.Encode(record) }
	err = write(struct {
		Type string `json:"type"`
		soakMetadata
	}{Type: "metadata", soakMetadata: evidence.Metadata})
	if err == nil {
		err = write(struct {
			Type string `json:"type"`
			soakReadiness
		}{Type: "readiness", soakReadiness: evidence.Readiness})
	}
	for _, sample := range evidence.Samples {
		if err != nil {
			break
		}
		err = write(struct {
			Type string `json:"type"`
			soakSampleRecord
		}{Type: "sample", soakSampleRecord: sample})
	}
	if err == nil && evidence.Summary != nil {
		err = write(struct {
			Type string `json:"type"`
			soak.Summary
		}{Type: "summary", Summary: *evidence.Summary})
	}
	if err == nil && soakErr != nil {
		err = write(struct {
			Type  string `json:"type"`
			Error string `json:"error"`
		}{Type: "failure", Error: soakErr.Error()})
	}
	if flushErr := buffer.Flush(); err == nil {
		err = flushErr
	}
	if syncErr := file.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
