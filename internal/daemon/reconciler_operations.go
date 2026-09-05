package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/lxdb/bsbctl/internal/pluginhost"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func (s *Reconciler) Launch(ctx context.Context, appID, action string, payload json.RawMessage) error {
	return s.launch(ctx, appID, action, payload, &protocol.SessionTrigger{Kind: protocol.SessionTriggerLauncher}, nil)
}

func (s *Reconciler) PluginOperation(ctx context.Context, appID string, kind protocol.OperationKind, operation string, payload json.RawMessage) (protocol.OperationResult, error) {
	if err := protocol.ValidateJSONObject("operation payload", payload, true); err != nil {
		return protocol.OperationResult{}, protocol.NewDomainError(protocol.ErrorInvalidArgument, err)
	}
	s.live.mu.RLock()
	app, exists := s.live.document.Apps[appID]
	plugin := s.live.document.Plugins[app.PluginID]
	generation, generationExists := s.live.generations.Lookup(app.PluginID, app.ID)
	loaded := s.live.loaded
	s.live.mu.RUnlock()
	if !loaded || !exists || !app.Enabled || !generationExists || !operationDeclared(plugin.Operations, operation, kind) {
		return protocol.OperationResult{}, ErrAppNotEnabled
	}
	request := protocol.OperationRequest{
		Instance: protocol.InstanceRef{ID: app.ID, Generation: generation}, Operation: operation, Payload: append(json.RawMessage(nil), payload...),
	}
	if err := request.Validate(); err != nil {
		return protocol.OperationResult{}, protocol.NewDomainError(protocol.ErrorInvalidArgument, err)
	}
	result, err := s.plugins.Operation(ctx, app.PluginID, request)
	if err != nil {
		return protocol.OperationResult{}, err
	}
	if err := result.Validate(); err != nil {
		return protocol.OperationResult{}, errors.New("plugin operation returned an invalid result")
	}
	result.Payload = append(json.RawMessage(nil), result.Payload...)
	return result, nil
}

func operationDeclared(descriptors []protocol.OperationDescriptor, id string, kind protocol.OperationKind) bool {
	for _, descriptor := range descriptors {
		if descriptor.ID == id && descriptor.Kind == kind {
			return true
		}
	}
	return false
}

func (s *Reconciler) launch(
	ctx context.Context,
	appID, action string,
	payload json.RawMessage,
	trigger *protocol.SessionTrigger,
	source *observationSessionSource,
) error {
	if err := protocol.ValidateJSONObject("action payload", payload, true); err != nil {
		return protocol.NewDomainError(protocol.ErrorInvalidArgument, err)
	}
	unlockLaunch := s.sessions.serializeLaunch()
	defer unlockLaunch()

	s.live.mu.RLock()
	app, exists := s.live.document.Apps[appID]
	loaded := s.live.loaded
	if !loaded || !exists || !app.Enabled {
		s.live.mu.RUnlock()
		return ErrAppNotEnabled
	}
	if action == "" {
		action = app.LaunchAction
		if action == "" {
			action = "start"
		}
	}
	admissionGeneration, generationReady := s.live.generations.Lookup(app.PluginID, app.ID)
	s.live.mu.RUnlock()
	if !generationReady {
		return ErrAppNotReady
	}
	admission, admitted := s.sessions.begin(app.PluginID, appID, admissionGeneration)
	if !admitted {
		return ErrForegroundUnavailable
	}
	err := s.plugins.Invoke(ctx, app.PluginID, pluginhost.InvokeRequest{
		InstanceID: app.ID, Generation: admissionGeneration, Action: action, Payload: slices.Clone(payload),
		SessionToken: string(admission.token), Trigger: trigger,
	}, pluginhost.InvocationInteractive, admission.token)
	if err != nil {
		s.sessions.cancel(admission)
		return err
	}
	s.live.mu.RLock()
	currentApp, stillExists := s.live.document.Apps[appID]
	currentGeneration, generationReady := s.live.generations.Lookup(app.PluginID, app.ID)
	stillValid := s.live.loaded && stillExists && currentApp.Enabled && currentApp.PluginID == app.PluginID &&
		generationReady && currentGeneration == admission.generation
	s.live.mu.RUnlock()
	if !stillValid {
		s.sessions.cancel(admission)
		_ = s.endSession(ctx, app.PluginID, protocol.InstanceRef{ID: app.ID, Generation: admission.generation}, admission.token)
		return nil
	}
	previous, accepted := s.sessions.promote(admission, app.PluginID, source)
	if !accepted {
		_ = s.endSession(ctx, app.PluginID, protocol.InstanceRef{ID: app.ID, Generation: admission.generation}, admission.token)
		return nil
	}
	s.live.mu.RLock()
	controller := s.attentionController
	s.live.mu.RUnlock()
	if previous.instanceID != "" && previous.token != "" && previous.pluginID != "" {
		_ = s.endSession(ctx, previous.pluginID, protocol.InstanceRef{ID: previous.instanceID, Generation: previous.generation}, previous.token)
	}
	if controller != nil {
		controller.Wake()
	}
	return nil
}

// ActivateSelected launches only the exact observation revision that the
// attention engine successfully rendered and only when its channel declares a
// generic activation action.
func (s *Reconciler) ActivateSelected(ctx context.Context) (bool, error) {
	record, selected := s.attentionController.SelectedObservation()
	if !selected {
		return false, nil
	}
	s.live.mu.RLock()
	app, exists := s.live.document.Apps[record.Observation.Instance.ID]
	policy, hasPolicy := app.Policies[record.Observation.Channel]
	eligible := s.live.loaded && exists && app.Enabled && app.PluginID == record.PluginID &&
		s.live.generations.matches(record.PluginID, app.ID, record.Generation) && hasPolicy && policy.ActivationAction != ""
	s.live.mu.RUnlock()
	if !eligible {
		return false, nil
	}
	trigger := &protocol.SessionTrigger{
		Kind: protocol.SessionTriggerObservation,
		Observation: &protocol.ObservationRef{
			Channel: record.Observation.Channel, Key: record.Observation.Key, Revision: record.Observation.Revision,
		},
	}
	source := &observationSessionSource{
		pluginID: record.PluginID, instanceID: app.ID, channel: record.Observation.Channel,
		key: record.Observation.Key, generation: record.Generation, revision: record.Observation.Revision,
	}
	return true, s.launch(ctx, app.ID, policy.ActivationAction, nil, trigger, source)
}

// AttentionRule returns core-owned presentation state for one authenticated observation.
