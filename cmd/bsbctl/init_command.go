package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/configschema"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/internal/secrets"
	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
	"github.com/lxdb/bsbctl/sdk/protocol"
	busylib "github.com/lxdb/busylib-go"
)

func runInit(args []string, stdout, stderr io.Writer) error {
	options, pluginPaths, positionals, err := parseInitOptions(args)
	if err != nil {
		return commandFailure(exitUsage, "invalid init flags")
	}
	if len(positionals) != 0 {
		return commandFailure(exitUsage, "init does not accept positional arguments")
	}
	configPath, err := resolveStatePath(options, "config", "config.json")
	if err != nil {
		return err
	}
	deviceURL := optionDefault(options, "device-url", busylib.DefaultLocalBaseURL)
	deviceTokenKeychain := options["device-token-keychain"]
	if deviceTokenKeychain != "" {
		if _, err := secrets.ParseReference(deviceTokenKeychain); err != nil {
			return commandFailure(exitUsage, "--device-token-keychain must be a keychain reference")
		}
	}
	document := config.Document{
		Version: config.CurrentVersion, Generation: 1,
		Device:  config.Device{BaseURL: deviceURL, AccessTokenSecret: deviceTokenKeychain},
		Plugins: make(map[string]config.Plugin, len(pluginPaths)),
		Apps:    map[string]config.App{},
	}
	for _, pluginPath := range pluginPaths {
		plugin, loadErr := localFirstPartyPlugin(pluginPath)
		if loadErr != nil {
			return commandFailure(exitUsage, "local plugin package is invalid")
		}
		if _, duplicate := document.Plugins[plugin.ID]; duplicate {
			return commandFailure(exitUsage, "local plugin package is duplicated")
		}
		document.Plugins[plugin.ID] = plugin
	}
	if err := document.Validate(); err != nil {
		return commandFailure(exitUsage, "init configuration is invalid")
	}
	store := config.NewStore(configPath)
	if _, err := store.ReplaceWithOutcome(0, document); err != nil {
		return commandFailure(exitOperational, "write configuration failed")
	}
	return writeJSON(stdout, struct {
		Status     string `json:"status"`
		Generation uint64 `json:"generation"`
	}{Status: "initialized", Generation: document.Generation})
}

func parseInitOptions(args []string) (map[string]string, []string, []string, error) {
	filtered := make([]string, 0, len(args))
	plugins := make([]string, 0)
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--plugin" {
			index++
			if index >= len(args) || strings.HasPrefix(args[index], "--") || args[index] == "" {
				return nil, nil, nil, errors.New("plugin value is required")
			}
			plugins = append(plugins, args[index])
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--plugin="); ok {
			if value == "" {
				return nil, nil, nil, errors.New("plugin value is required")
			}
			plugins = append(plugins, value)
			continue
		}
		filtered = append(filtered, arg)
	}
	options, positionals, err := parseOptions(filtered, "config", "device-url", "device-token-keychain")
	return options, plugins, positionals, err
}

func localFirstPartyPlugin(executable string) (config.Plugin, error) {
	if !filepath.IsAbs(executable) {
		return config.Plugin{}, errors.New("plugin executable must be absolute")
	}
	descriptor, ok := firstpartyplugins.LookupBinary(executable)
	if !ok {
		return config.Plugin{}, errors.New("plugin executable is not registered")
	}
	info, err := os.Stat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return config.Plugin{}, errors.New("plugin executable is unavailable")
	}
	file, err := os.Open(executable)
	if err != nil {
		return config.Plugin{}, err
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return config.Plugin{}, err
	}
	root := filepath.Dir(executable)
	definition := descriptor.DefinitionForVersion(descriptor.DevelopmentVersion)
	plugin := pluginConfigurationFromDefinition(definition, executable)
	plugin.SHA256 = hex.EncodeToString(digest.Sum(nil))
	plugin.PackageRoot = root
	plugin.Assets = descriptor.Assets
	schema, err := localConfigSchema(root)
	if err != nil {
		return config.Plugin{}, err
	}
	plugin.ConfigSchema = schema
	return plugin, nil
}

func localConfigSchema(root string) (*configschema.Declaration, error) {
	path := filepath.Join(root, configschema.FileName)
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() < 1 || before.Size() > configschema.MaxBytes {
		return nil, errors.New("plugin configuration schema is unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("plugin configuration schema is unavailable")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, configschema.MaxBytes+1))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil || !os.SameFile(before, after) || int64(len(data)) != before.Size() {
		return nil, errors.New("plugin configuration schema is unavailable")
	}
	if _, err := configschema.Compile(data); err != nil {
		return nil, errors.New("plugin configuration schema is invalid")
	}
	digest := sha256.Sum256(data)
	return &configschema.Declaration{Source: configschema.FileName, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data))}, nil
}

func pluginConfigurationFromDefinition(definition pluginsdk.Definition, executable string) config.Plugin {
	return config.Plugin{
		ID: definition.ID, Version: definition.Version, Executable: executable,
		ProtocolVersion: protocol.Version,
		ExecutionModes:  slices.Clone(definition.Contract.ExecutionModes),
		Channels:        slices.Clone(definition.Contract.Channels),
		Operations:      slices.Clone(definition.Contract.Operations),
	}
}
