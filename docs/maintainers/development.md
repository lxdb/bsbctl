# Development

Use this workflow to build bsbctl and first-party plugins from a checkout. It creates local artifacts only.

## Requirements

- macOS for the supported runtime and Calendar build.
- Go 1.26.6.
- ShellCheck 0.11.0 for the full repository verification.
- A reachable BUSY Bar endpoint for device tests only.

The committed module uses `github.com/lxdb/busylib-go v0.2.0`. An ignored `go.work` can select a sibling busylib-go checkout without changing the committed dependency.

## Reinstall changes locally

For an existing installation, run this from the checkout after making changes:

```sh
./install.sh --local
```

This rebuilds the core and all configured local first-party plugins, including disabled plugins. It uses the pinned Go toolchain and the normal build cache, with `go.work` disabled. It does not download GitHub releases or catalogs; Go may download missing modules or the pinned toolchain. Local macOS binaries are ad-hoc signed, not notarized releases.

To also add selected first-party apps, or build only the core:

```sh
./install.sh --local --apps calendar,codex,codex-quota,mac-resources
./install.sh --local --apps none
```

Existing app settings, enablement, device credentials, and checkpoints are preserved. Explicitly selected apps are enabled only when first created. Catalog-managed plugins are not rebuilt by default, and selecting one explicitly is rejected. The signed catalog and its installation records are not changed.

The installer builds and validates executables, schemas, assets, and the resulting configuration before stopping the default LaunchAgent. It reads configuration again after shutdown to preserve changes made during the build. It replaces the core atomically, registers the new package paths and hashes, and restarts only a previously loaded service. Success after restart requires the expected daemon version and configuration generation, healthy enabled apps, uploaded package assets, and a ready device. A stopped service stays stopped.

This mode requires an existing valid `~/.bsbctl/config.json`. It does not provision device credentials, change PATH, or accept `--device-url`. Stop foreground daemons first. Custom LaunchAgent executable, configuration, or socket paths are rejected without changing the service.

Each successful replacement retains its build directory under `~/.bsbctl/local-builds/install-*`. The printed directory contains `previous/bsbctl` when a core was already installed and `previous/config.json`; earlier package directories remain untouched. Do not delete a build directory while configuration still references its plugins. A failure before configuration commit restores the replaced core and attempts to resume the old service without overwriting configuration from another writer. A committed configuration write with uncertain durability leaves the new files in place and the service stopped. A startup or readiness failure after commit also returns an error but keeps the new installation and recovery files for diagnosis. Stop the daemon before restoring either saved file.

Local installation does not run the test suite. Run the checks in [Testing](testing.md) separately.

## Build a minimal local system

```sh
mkdir -p ./bin/mac-resources
go build -o ./bin/bsbctl ./cmd/bsbctl
go build \
  -o ./bin/mac-resources/bsbctl-plugin-mac-resources \
  ./cmd/bsbctl-plugin-mac-resources
cp plugins/macresources/config.schema.json \
  ./bin/mac-resources/config.schema.json
```

Create the first configuration generation. Replace the example URL with the endpoint for your device:

```sh
./bin/bsbctl init \
  --plugin "$PWD/bin/mac-resources/bsbctl-plugin-mac-resources" \
  --device-url http://192.0.2.10
```

`init` accepts absolute plugin executable paths and refuses to overwrite an existing configuration. Start the daemon:

```sh
./bin/bsbctl daemon
```

Keep it running. In another terminal:

```sh
./bin/bsbctl app create mac-resources
./bin/bsbctl status
```

A JSON status response confirms that the control socket responds. Check the device readiness field to find out whether BUSY Bar is reachable.

## Build optional plugins

For a new configuration, build optional packages before running `init`. Calendar requires macOS 14 or later and cgo:

```sh
mkdir -p ./bin/calendar ./bin/codex/assets
go build -o ./bin/calendar/bsbctl-plugin-calendar ./cmd/bsbctl-plugin-calendar
cp ./plugins/calendar/config.schema.json ./bin/calendar/config.schema.json
go build -o ./bin/codex/bsbctl-plugin-codex ./cmd/bsbctl-plugin-codex
cp ./plugins/codex/config.schema.json ./bin/codex/config.schema.json
cp ./plugins/codex/assets/codex-mark.png ./bin/codex/assets/codex-mark.png
```

Pass an additional `--plugin` absolute executable path to `init` for each package. After the daemon starts, create its app with `./bin/bsbctl app create calendar` or `./bin/bsbctl app create codex`. For an existing installation, use [local reinstall](#reinstall-changes-locally).

## Repository conventions

- Use CLI transactions after initialization. Do not hand-edit the live configuration.
- Keep runtime construction in `internal/daemonrun`; `cmd/bsbctl` owns argument parsing, streams, signals, and exit-code mapping only.
- Put mutable daemon behavior in its existing owner: `DesiredState`, `LiveState`, `Reconciler`, `SessionCoordinator`, or `PackageOps`. Do not recreate a broad service facade or install required dependencies with setters.
- Keep plugin completion admission and child lifecycle inside `pluginhost.Manager`; callbacks notify already-constructed narrow owners.
- Use `sdk/rpc` and `sdk/protocol` directly. Do not add internal alias or compatibility packages around their public contracts.
- Keep plugin configuration schemas with their packages.
- Keep runtime secrets in supported secret references, not JSON configuration or environment variables.
- When a public command, app, schema, SDK, or protocol changes, update the task documentation and its drift check.
- Do not use release commands as a substitute for local verification.

## Preview generation

Regenerate the publishable previews from the reviewed mock-only framebuffer fixtures:

```sh
go run -tags preview ./cmd/previewgen --out docs/previews
```

Without `--capture`, the generator never contacts a BUSY Bar or reads local provider configuration. It authenticates the reviewed 72x16 fixture bytes, then applies the official device frame and rounded LED presentation. It replaces all four outputs only after every artifact is valid and no larger than 1 MiB.

Mock only provider inputs, timestamps, and user-safe labels. Build every preview through the plugin's production reducer, interaction, and scene-building paths. Do not hand-author a preview-only scene or duplicate production display text; a preview must match what the downloaded plugin publishes for the same inputs.

After a production-scene change, explicitly render the mock-only Calendar, Codex, and Codex Quota scenes through the configured BUSY Bar, refresh their reviewed fixtures and checksums, and regenerate all four outputs:

```bash
go run -tags preview ./cmd/previewgen --capture
```

Capture mode reads the existing device configuration and credential, uses only the `bsbctl-preview` display and asset namespace, and does not install or start bsbctl. Review every captured animation frame before retaining the generated files.

See [Testing](testing.md) before reporting a change as complete.
