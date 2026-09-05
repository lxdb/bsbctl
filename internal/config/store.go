// Package config owns validated, generation-checked, atomic daemon configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/configschema"
	"github.com/lxdb/bsbctl/internal/identifier"
	"github.com/lxdb/bsbctl/internal/localstate"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/internal/secrets"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	CurrentVersion = 1
	maxConfigBytes = 1 << 20
	MaxPlugins     = 64
	MaxApps        = 256
)

var ErrConflict = errors.New("configuration generation conflict")

// Document is the complete non-secret desired state owned by the daemon.
type Document struct {
	Version    int               `json:"version"`
	Generation uint64            `json:"generation"`
	Device     Device            `json:"device,omitempty"`
	Plugins    map[string]Plugin `json:"plugins"`
	Apps       map[string]App    `json:"apps"`
}

type Device struct {
	BaseURL           string `json:"base_url,omitempty"`
	AccessTokenSecret string `json:"access_token_secret,omitempty"`
}

// Plugin identifies one installed and verified executable.
type Plugin struct {
	ID              string                         `json:"id"`
	Version         string                         `json:"version"`
	Executable      string                         `json:"executable"`
	SHA256          string                         `json:"sha256,omitempty"`
	ProtocolVersion string                         `json:"protocol_version"`
	ExecutionModes  []protocol.ExecutionMode       `json:"execution_modes"`
	Channels        []protocol.Channel             `json:"channels"`
	Operations      []protocol.OperationDescriptor `json:"operations,omitempty"`
	ConfigSchema    *configschema.Declaration      `json:"config_schema,omitempty"`
	PackageRoot     string                         `json:"package_root,omitempty"`
	Assets          []assets.Declaration           `json:"assets,omitempty"`
}

// App is one configured instance of a plugin package.
type App struct {
	ID           string                               `json:"id"`
	PluginID     string                               `json:"plugin_id"`
	Generation   uint64                               `json:"generation,omitzero"`
	Enabled      bool                                 `json:"enabled"`
	LaunchAction string                               `json:"launch_action,omitempty"`
	Config       json.RawMessage                      `json:"config"`
	Secrets      map[string]string                    `json:"secrets,omitempty"`
	Policies     map[string]presentation.PolicyConfig `json:"policies"`
}

// Store serializes durable replacements for one configuration path.
type Store struct {
	mu      sync.Mutex
	path    string
	replace func(string, any) (localstate.CommitOutcome, error)
}

func NewStore(path string) *Store { return &Store{path: path, replace: localstate.ReplaceJSONCompact} }

// Load decodes one bounded document and rejects unknown or trailing input.
func (s *Store) Load() (Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

type deviceConfigDocument struct {
	Version    int             `json:"version"`
	Generation uint64          `json:"generation"`
	Device     Device          `json:"device,omitempty"`
	Plugins    json.RawMessage `json:"plugins"`
	Apps       json.RawMessage `json:"apps"`
}

// LoadDevice decodes and validates only the configuration envelope and device
// settings. It intentionally does not require unrelated runtime records to be
// valid for direct, read-only device diagnostics.
func (s *Store) LoadDevice() (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var envelope deviceConfigDocument
	if err := s.decode(&envelope); err != nil {
		return Device{}, err
	}
	return envelope.Device, validateConfigEnvelope(envelope.Version, envelope.Generation, envelope.Device)
}

func validateConfigEnvelope(version int, generation uint64, device Device) error {
	var errs []error
	if version != CurrentVersion {
		errs = append(errs, fmt.Errorf("configuration version must be %d", CurrentVersion))
	}
	if generation == 0 {
		errs = append(errs, errors.New("configuration generation must be greater than zero"))
	}
	if err := device.validate(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *Store) decode(target any) error {
	file, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	if len(data) > maxConfigBytes {
		return errors.New("configuration exceeds 1 MiB")
	}
	if err := protocol.DecodeStrict(data, target); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	return nil
}

func (s *Store) load() (Document, error) {
	var document Document
	if err := s.decode(&document); err != nil {
		return Document{}, err
	}
	if err := document.Validate(); err != nil {
		return Document{}, err
	}
	return document, nil
}

// ReplaceWithOutcome persists next only when the current generation equals
// expected and reports whether the atomic rename committed.
func (s *Store) ReplaceWithOutcome(expected uint64, next Document) (localstate.CommitOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.replaceLocked(expected, next)
}

func (s *Store) replaceLocked(expected uint64, next Document) (localstate.CommitOutcome, error) {
	if next.Generation != expected+1 {
		return localstate.NotCommitted, fmt.Errorf("%w: next generation must be %d", ErrConflict, expected+1)
	}
	current, err := s.load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return localstate.NotCommitted, err
	}
	if err == nil && current.Generation != expected {
		return localstate.NotCommitted, fmt.Errorf("%w: current generation is %d", ErrConflict, current.Generation)
	}
	if errors.Is(err, os.ErrNotExist) && expected != 0 {
		return localstate.NotCommitted, fmt.Errorf("%w: configuration does not exist", ErrConflict)
	}
	var previous *Document
	if err == nil {
		previous = &current
	}
	assignAppGenerations(previous, &next)
	if err := next.Validate(); err != nil {
		return localstate.NotCommitted, err
	}
	return s.write(next)
}

// Update serializes load, clone, mutation, generation increment, validation,
// and atomic commit under one store lock. mutate must not re-enter this Store.
func (s *Store) Update(expectedGeneration uint64, mutate func(*Document) error) (Document, localstate.CommitOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.load()
	if err != nil {
		return Document{}, localstate.NotCommitted, err
	}
	if current.Generation != expectedGeneration {
		return Document{}, localstate.NotCommitted, fmt.Errorf("%w: current generation is %d", ErrConflict, current.Generation)
	}
	if mutate == nil {
		return Document{}, localstate.NotCommitted, errors.New("configuration mutation is required")
	}
	next := cloneDocument(current)
	if err := mutate(&next); err != nil {
		return Document{}, localstate.NotCommitted, err
	}
	next.Generation = current.Generation + 1
	assignAppGenerations(&current, &next)
	if err := next.Validate(); err != nil {
		return Document{}, localstate.NotCommitted, err
	}
	outcome, err := s.write(next)
	if !outcome.IsCommitted() {
		return Document{}, outcome, err
	}
	return cloneDocument(next), outcome, err
}

// Validate checks cross-reference, path, policy, and secret-reference invariants.
func (d Document) Validate() error {
	var errs []error
	configurationSchemas := make(map[string]*configschema.Schema)
	if err := validateConfigEnvelope(d.Version, d.Generation, d.Device); err != nil {
		errs = append(errs, err)
	}
	if len(d.Plugins) > MaxPlugins {
		errs = append(errs, fmt.Errorf("configuration supports at most %d plugins", MaxPlugins))
	}
	if len(d.Apps) > MaxApps {
		errs = append(errs, fmt.Errorf("configuration supports at most %d app instances", MaxApps))
	}
	pluginIDs := make([]string, 0, len(d.Plugins))
	for pluginID := range d.Plugins {
		pluginIDs = append(pluginIDs, pluginID)
	}
	if err := assets.ValidatePluginHashCollisions(pluginIDs); err != nil {
		errs = append(errs, err)
	}
	for key, plugin := range d.Plugins {
		if key != plugin.ID {
			errs = append(errs, fmt.Errorf("plugin map key %q must match id", key))
		}
		if err := identifier.Validate("plugin id", plugin.ID); err != nil {
			errs = append(errs, err)
		}
		if strings.TrimSpace(plugin.Version) == "" {
			errs = append(errs, fmt.Errorf("plugin %q version must not be empty", key))
		}
		if err := validatePluginProtocol(plugin); err != nil {
			errs = append(errs, fmt.Errorf("plugin %q: %w", key, err))
		}
		if !filepath.IsAbs(plugin.Executable) {
			errs = append(errs, fmt.Errorf("plugin %q executable must be an absolute path", key))
		}
		if plugin.SHA256 != "" && (len(plugin.SHA256) != 64 || !isLowerHex(plugin.SHA256)) {
			errs = append(errs, fmt.Errorf("plugin %q sha256 must be 64 lowercase hexadecimal characters", key))
		}
		if err := protocol.ValidateExecutionModes(plugin.ExecutionModes); err != nil {
			errs = append(errs, fmt.Errorf("plugin %q execution modes: %w", key, err))
		}
		if err := validateChannels(plugin); err != nil {
			errs = append(errs, err)
		}
		operations := make(map[string]struct{}, len(plugin.Operations))
		for _, descriptor := range plugin.Operations {
			if err := descriptor.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("plugin %q: %w", key, err))
			}
			if _, exists := operations[descriptor.ID]; exists {
				errs = append(errs, fmt.Errorf("plugin %q operation %q is duplicated", key, descriptor.ID))
			}
			operations[descriptor.ID] = struct{}{}
		}
		root := plugin.PackageRoot
		if root == "" {
			root = filepath.Dir(plugin.Executable)
		}
		if err := assets.ValidatePackage(assets.Package{
			PluginID: plugin.ID, Version: plugin.Version, Root: root,
			Enabled: true, Assets: plugin.Assets,
		}); err != nil {
			errs = append(errs, fmt.Errorf("plugin %q assets: %w", key, err))
		}
		if plugin.ConfigSchema != nil {
			schema, err := configschema.Load(root, *plugin.ConfigSchema)
			if err != nil {
				errs = append(errs, fmt.Errorf("plugin %q configuration schema is invalid", key))
			} else {
				configurationSchemas[plugin.ID] = schema
			}
		}
	}
	for key, app := range d.Apps {
		if key != app.ID {
			errs = append(errs, fmt.Errorf("app map key %q must match id", key))
		}
		if err := identifier.Validate("app id", app.ID); err != nil {
			errs = append(errs, err)
		}
		if app.Generation == 0 {
			errs = append(errs, fmt.Errorf("app %q generation must be greater than zero", key))
		} else if app.Generation > d.Generation {
			errs = append(errs, fmt.Errorf("app %q generation must not exceed configuration generation", key))
		}
		plugin, exists := d.Plugins[app.PluginID]
		if !exists {
			errs = append(errs, fmt.Errorf("app %q references unknown plugin %q", key, app.PluginID))
			continue
		}
		if strings.ContainsAny(app.LaunchAction, "\r\n\x00") {
			errs = append(errs, fmt.Errorf("app %q launch_action contains invalid control characters", key))
		}
		if err := protocol.ValidateJSONObject("config", app.Config, false); err != nil {
			errs = append(errs, fmt.Errorf("app %q config must be a JSON object no larger than 64 KiB", key))
		}
		if schema := configurationSchemas[app.PluginID]; schema != nil {
			if err := schema.Validate(app.Config); err != nil {
				errs = append(errs, fmt.Errorf("app %q config does not match the plugin schema", key))
			}
		}
		for name, reference := range app.Secrets {
			if strings.TrimSpace(name) == "" || !validSecretReference(reference) {
				errs = append(errs, fmt.Errorf("app %q secret %q must be a keychain reference", key, name))
			}
		}
		for channelID, policy := range app.Policies {
			_, ok := findChannel(plugin.Channels, channelID)
			if !ok {
				errs = append(errs, fmt.Errorf("app %q configures unknown channel %q", key, channelID))
				continue
			}
			if !supportedPolicy(policy.Policy) {
				errs = append(errs, fmt.Errorf("app %q channel %q has unsupported policy %q", key, channelID, policy.Policy))
			}
			if policy.DevicePriority < 0 || policy.DevicePriority > 100 || policy.HoldMS < 0 || policy.CooldownMS < 0 {
				errs = append(errs, fmt.Errorf("app %q channel %q has invalid policy bounds", key, channelID))
			}
			if policy.ActivationAction != "" {
				if err := identifier.Validate("activation action", policy.ActivationAction); err != nil {
					errs = append(errs, fmt.Errorf("app %q channel %q: %w", key, channelID, err))
				}
			}
			if policy.Policy == presentation.PolicyRotation {
				if policy.RotationIntervalMS < 10_000 || policy.RotationIntervalMS > 86_400_000 {
					errs = append(errs, fmt.Errorf("app %q channel %q rotation_interval_ms must be between 10000 and 86400000", key, channelID))
				}
				if policy.RotationJitterPercent < 0 || policy.RotationJitterPercent > 50 {
					errs = append(errs, fmt.Errorf("app %q channel %q rotation_jitter_percent must be between 0 and 50", key, channelID))
				}
			} else if policy.RotationIntervalMS != 0 || policy.RotationJitterPercent != 0 {
				errs = append(errs, fmt.Errorf("app %q channel %q rotation scheduling requires rotation policy", key, channelID))
			}
		}
	}
	return errors.Join(errs...)
}

func assignAppGenerations(previous *Document, next *Document) {
	for id, app := range next.Apps {
		generation := next.Generation
		if previous != nil {
			oldApp, existed := previous.Apps[id]
			oldPlugin, hadPlugin := previous.Plugins[oldApp.PluginID]
			newPlugin, hasPlugin := next.Plugins[app.PluginID]
			if existed && hadPlugin && hasPlugin && sameAppConfiguration(oldApp, app) && reflect.DeepEqual(oldPlugin, newPlugin) {
				generation = oldApp.Generation
			}
		}
		app.Generation = generation
		next.Apps[id] = app
	}
}

func sameAppConfiguration(left, right App) bool {
	left.Generation = 0
	right.Generation = 0
	return reflect.DeepEqual(left, right)
}

func validatePluginProtocol(plugin Plugin) error {
	if plugin.ProtocolVersion != protocol.Version {
		return fmt.Errorf("protocol_version must be %q", protocol.Version)
	}
	return nil
}

func (d Device) validate() error {
	if d.BaseURL != "" {
		parsed, err := url.Parse(d.BaseURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
			return errors.New("device base_url must be an http or https URL without user information")
		}
	}
	if d.AccessTokenSecret != "" && !validSecretReference(d.AccessTokenSecret) {
		return errors.New("device access_token_secret must be a keychain reference")
	}
	return nil
}

func (s *Store) write(document Document) (localstate.CommitOutcome, error) {
	data, err := localstate.MarshalJSONCompact(document)
	if err != nil {
		return localstate.NotCommitted, err
	}
	if len(data) > maxConfigBytes {
		return localstate.NotCommitted, errors.New("configuration exceeds 1 MiB")
	}
	return s.replace(s.path, document)
}

func validateChannels(plugin Plugin) error {
	seen := make(map[string]struct{}, len(plugin.Channels))
	for _, channel := range plugin.Channels {
		if err := identifier.Validate("channel id", channel.ID); err != nil {
			return fmt.Errorf("plugin %q: %w", plugin.ID, err)
		}
		if _, exists := seen[channel.ID]; exists {
			return fmt.Errorf("plugin %q channel %q is duplicated", plugin.ID, channel.ID)
		}
		seen[channel.ID] = struct{}{}
	}
	return nil
}

func supportedPolicy(policy presentation.Policy) bool {
	switch policy {
	case presentation.PolicyAttention, presentation.PolicyInteractive, presentation.PolicyWhenRelevant, presentation.PolicyRotation:
		return true
	default:
		return false
	}
}

func validSecretReference(reference string) bool {
	_, err := secrets.ParseReference(reference)
	return err == nil
}

func cloneDocument(source Document) Document {
	result := source
	result.Plugins = make(map[string]Plugin, len(source.Plugins))
	for id, plugin := range source.Plugins {
		plugin.ExecutionModes = slices.Clone(plugin.ExecutionModes)
		plugin.Channels = slices.Clone(plugin.Channels)
		plugin.Operations = slices.Clone(plugin.Operations)
		if plugin.ConfigSchema != nil {
			copy := *plugin.ConfigSchema
			plugin.ConfigSchema = &copy
		}
		plugin.Assets = slices.Clone(plugin.Assets)
		result.Plugins[id] = plugin
	}
	result.Apps = make(map[string]App, len(source.Apps))
	for id, app := range source.Apps {
		app.Config = slices.Clone(app.Config)
		app.Secrets = cloneMap(app.Secrets)
		app.Policies = cloneMap(app.Policies)
		result.Apps[id] = app
	}
	return result
}

func cloneMap[K comparable, V any](source map[K]V) map[K]V {
	if source == nil {
		return nil
	}
	result := make(map[K]V, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func isLowerHex(value string) bool {
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func findChannel(channels []protocol.Channel, id string) (protocol.Channel, bool) {
	for _, channel := range channels {
		if channel.ID == id {
			return channel, true
		}
	}
	return protocol.Channel{}, false
}
