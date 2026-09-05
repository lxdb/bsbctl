package daemon

import (
	"context"
	"testing"
)

func TestSessionRuntimeConstructorRejectsEveryMissingRequiredDependency(t *testing.T) {
	state := NewSessionCoordinator(nil)
	plugins := sessionPluginController(&safePluginController{})
	inputs := SessionInputController(&recordingSessionInputController{})
	invalidate := sessionContextInvalidator(func(context.Context) error { return nil })
	tests := []struct {
		name       string
		state      *SessionCoordinator
		plugins    sessionPluginController
		inputs     SessionInputController
		invalidate sessionContextInvalidator
	}{
		{name: "state", plugins: plugins, inputs: inputs, invalidate: invalidate},
		{name: "plugins", state: state, inputs: inputs, invalidate: invalidate},
		{name: "inputs", state: state, plugins: plugins, invalidate: invalidate},
		{name: "invalidator", state: state, plugins: plugins, inputs: inputs},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewSessionRuntime(test.state, test.plugins, test.inputs, test.invalidate); err == nil {
				t.Fatal("NewSessionRuntime accepted a missing required dependency")
			}
		})
	}
}
