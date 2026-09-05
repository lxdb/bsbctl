package daemonrun

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/daemon"
	"github.com/lxdb/bsbctl/internal/device"
	"github.com/lxdb/bsbctl/internal/eventbus"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/internal/pluginlog"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

type retiredInstance struct {
	pluginID   string
	instanceID string
	generation uint64
}

type sessionContextInvalidation struct {
	done chan struct{}
}

func runInstanceRetirementRelay(ctx context.Context, retired <-chan retiredInstance, cleaner daemon.InstanceCleaner) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case instance := <-retired:
			cleaner.RemoveInstance(instance.pluginID, instance.instanceID, instance.generation)
		}
	}
}

func runSessionChangeRelay(ctx context.Context, changes <-chan daemon.SessionChange, inputs daemon.SessionInputController, wake func()) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case change := <-changes:
			change.Apply(inputs)
			wake()
		}
	}
}

func runSessionFailureRelay(ctx context.Context, failures <-chan eventbus.Failure, sessions *daemon.SessionRuntime) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case failure := <-failures:
			sessions.ClearForegroundSessionContext(ctx, failure.Request.Instance.ID, failure.Request.SessionToken)
		}
	}
}

func runSessionContextRelay(ctx context.Context, changes <-chan sessionContextInvalidation, launcher interface{ Close() }, inputs interface{ InvalidateContext() }) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case change := <-changes:
			launcher.Close()
			inputs.InvalidateContext()
			close(change.done)
		}
	}
}

type deviceIdentityReader interface {
	DeviceIdentity(context.Context) (device.DeviceIdentity, error)
}

type audioDiagnosticRenderer struct {
	gateway *device.Gateway
	logs    *pluginlog.Sink
}

func (r *audioDiagnosticRenderer) Render(ctx context.Context, candidate *presentation.Candidate) (attention.DeliveryOutcome, error) {
	previous := r.gateway.Status()
	outcome, err := r.gateway.Render(ctx, candidate)
	if err != nil {
		return outcome, err
	}
	current := r.gateway.Status()
	if code := current.LastErrorCode; code != "" && current.Attempts != previous.Attempts {
		r.logs.Log("bsbctl", protocol.LogNotification{
			Level: protocol.LogLevelWarn, Event: "device_audio_degraded",
			Fields: map[string]string{"error_code": code},
		})
	}
	return outcome, nil
}

func runDeviceIdentityDiagnostics(
	ctx context.Context,
	reader deviceIdentityReader,
	wake <-chan struct{},
	log func(protocol.LogNotification),
) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
			identityCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			identity, err := reader.DeviceIdentity(identityCtx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				log(protocol.LogNotification{
					Level: protocol.LogLevelWarn, Event: "device_identity_unavailable",
					Message: "device identity could not be read for this connection",
				})
				continue
			}
			log(protocol.LogNotification{
				Level: protocol.LogLevelInfo, Event: "device_connected", Message: "BUSY Bar connection identity",
				Fields: map[string]string{
					"api_semver": identity.APISemVer, "serial": identity.SerialNumber,
					"otp_model": identity.OTPModel, "otp_valid": strconv.FormatBool(identity.OTPValid),
					"firmware_target": strconv.Itoa(identity.FirmwareTarget), "firmware_version": identity.FirmwareVersion,
					"firmware_commit": identity.FirmwareCommit, "firmware_dirty": identity.FirmwareDirty, "uptime": identity.Uptime,
				},
			})
		}
	}
}

func daemonPluginLogCallbacks(logs *pluginlog.Sink) pluginhost.Callbacks {
	return pluginhost.Callbacks{Log: logs.Log}
}

func daemonObservationCallback(store *observation.Store, resolver presentation.AssetCompilerResolver) func(observation.Source, protocol.Observation) error {
	return func(source observation.Source, value protocol.Observation) error {
		if err := presentation.ValidateObservation(source.PluginID, value, resolver); err != nil {
			kind := protocol.ErrorInvalidArgument
			if errors.Is(err, assets.ErrAssetsNotReady) {
				kind = protocol.ErrorNotReady
			}
			return protocol.NewDomainError(kind, err)
		}
		return store.Publish(source, value)
	}
}
