package pluginhost

import (
	"maps"
	"reflect"
	"slices"
	"time"

	"github.com/lxdb/bsbctl/sdk/protocol"
)

func (s *supervisor) publish() {
	s.statusMu.Lock()
	s.status = PluginStatus{
		ID: s.id, Desired: s.desired, Phase: s.phase, Running: s.child != nil, Healthy: s.child != nil && s.healthy,
		HealthMisses: s.healthMisses, RetryAt: s.retryAt, LastErrorCode: s.lastErrorCode, LastErrorAt: s.lastErrorAt,
		SessionLifecycleErrorCode: s.sessionErrorCode, SessionLifecycleErrorAt: s.sessionErrorAt,
	}
	s.statusMu.Unlock()
	s.manager.notify()
}
func (s *supervisor) snapshot() PluginStatus {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	cutoff := s.manager.options.clock.Now().Add(-failureWindow)
	kept := s.exits[:0]
	for _, value := range s.exits {
		if !value.Before(cutoff) {
			kept = append(kept, value)
		}
	}
	s.exits = kept
	result := s.status
	result.ExitCount = len(s.exits)
	return result
}

func startupChanged(a, b Spec) bool {
	return a.Version != b.Version || a.Executable != b.Executable || a.SHA256 != b.SHA256 || !reflect.DeepEqual(a.Args, b.Args) || !reflect.DeepEqual(a.ExecutionModes, b.ExecutionModes) || !reflect.DeepEqual(a.Channels, b.Channels) || !reflect.DeepEqual(a.Operations, b.Operations)
}
func cloneSpec(spec Spec) Spec {
	spec.Args = slices.Clone(spec.Args)
	spec.ExecutionModes = slices.Clone(spec.ExecutionModes)
	spec.Channels = slices.Clone(spec.Channels)
	spec.Operations = slices.Clone(spec.Operations)
	spec.Instances = cloneInstances(spec.Instances)
	return spec
}
func cloneInstances(instances []Instance) []Instance {
	result := make([]Instance, len(instances))
	for i, instance := range instances {
		result[i] = instance
		result[i].Config = slices.Clone(instance.Config)
		result[i].Secrets = cloneMap(instance.Secrets)
		result[i].Policies = cloneMap(instance.Policies)
		if instance.Checkpoint != nil {
			restore := *instance.Checkpoint
			restore.Data = slices.Clone(instance.Checkpoint.Data)
			result[i].Checkpoint = &restore
		}
	}
	return result
}
func cloneMap[K comparable, V any](source map[K]V) map[K]V {
	return maps.Clone(source)
}
func hasExecutionMode(values []protocol.ExecutionMode, wanted protocol.ExecutionMode) bool {
	return slices.Contains(values, wanted)
}

type realClock struct{}

func (realClock) Now() time.Time                     { return time.Now().UTC() }
func (realClock) NewTimer(delay time.Duration) Timer { return realTimer{Timer: time.NewTimer(delay)} }

type realTimer struct{ *time.Timer }

func (timer realTimer) C() <-chan time.Time { return timer.Timer.C }
