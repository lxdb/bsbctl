# Getting started

You need macOS, a reachable BUSY Bar over USB networking or a local network, and `~/.local/bin` on `PATH`.

## Install

```sh
curl -fsSL https://github.com/lxdb/bsbctl/releases/latest/download/install.sh | sh
```

The installer verifies the release, installs the binary and per-user LaunchAgent, then asks which [apps](apps.md) to enable. Press Enter to skip an app. There is no background updater.

Release binaries are ad-hoc signed. They are not Developer ID signed or notarized, so Gatekeeper may restrict them.

## Choose apps and a device

After installation, select apps with a comma-separated list:

```sh
bsbctl setup --apps calendar,mac-resources
```

Use `--apps none` for core only. To override the default device address:

```sh
bsbctl setup --apps none --device-url http://192.0.2.10
```

Setup rejects unknown or duplicate app IDs before making changes. Repeated runs preserve existing app settings and update only selected managed plugins. Setup restarts the LaunchAgent, even if its definition and binary version are unchanged.

## Device authentication

If the BUSY Bar requires authentication, keep the credential in macOS Keychain. Do not put tokens in JSON configuration or command output.

To use an administrator key already stored in Keychain:

```sh
bsbctl setup \
  --apps mac-resources \
  --device-bootstrap-keychain keychain://bsbctl/device-bootstrap
```

Setup creates and verifies a dedicated access token, stores it at `keychain://bsbctl/device/access-token`, and saves only the reference in configuration. It reuses a working access token at the destination and refuses to overwrite a different value. A numeric administrator key cannot serve as the destination access token.

Use `--device-token-keychain` to select another Keychain reference, including one that already holds a device access token.

## Check the result

```sh
bsbctl version
bsbctl status
bsbctl app list
```

Confirm the expected binary version and a JSON status response. Check device readiness separately; a running daemon does not mean the BUSY Bar is reachable.

## Update

Run the installer again to update core. Select the apps to update when prompted. Existing app settings, enabled state, and secret references are preserved.

## Setup failures

| Symptom | Action |
| --- | --- |
| Command not found | Add `~/.local/bin` to `PATH`, or use `~/.local/bin/bsbctl`. |
| Daemon unavailable | Check `bsbctl service status`; see [Operations](operations.md#the-control-socket-is-unavailable). |
| Device not ready | Check the URL, network connection, and Keychain reference. |
| Exit code `6` | Stop making changes and inspect service, app, and plugin status before retrying. See [partial results](reference/errors.md#partial-and-uncertain-results). |
| macOS blocks the binary | Review the signing limitation above and your Gatekeeper policy. |
| No stable release | Follow [Development](maintainers/development.md). |
