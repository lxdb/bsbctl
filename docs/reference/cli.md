# CLI reference

Run `bsbctl help` for the authoritative command synopsis. Setup, initialization, status, app, plugin, attention, and service commands write machine-readable JSON on success. `version` writes one plain-text version. `help` writes text, `daemon` runs until stopped, and `device screenshot` writes image files plus a JSON result.

## Core

```text
bsbctl setup [--apps APP-ID,...|none] [--device-url URL] [--device-bootstrap-keychain REF] [--device-token-keychain REF]
bsbctl init [--plugin /absolute/path]... [--device-url URL] [--device-token-keychain keychain://service/account]
bsbctl daemon [--config PATH] [--socket PATH] [--log ABSOLUTE_PATH]
bsbctl status [--socket PATH]
bsbctl version
```

Use `setup` for a supported installed system. Use `init` once for checkout-local development; it refuses to overwrite an existing configuration.

## Apps and plugins

```text
bsbctl app list [--socket PATH]
bsbctl app status <app-id> [--socket PATH]
bsbctl app enable <app-id> [--socket PATH]
bsbctl app disable <app-id> [--socket PATH]
bsbctl app create <built-in-app-id> [--enabled true|false] [--socket PATH]
bsbctl app create <app-id> --plugin PLUGIN-ID --file PATH|- [--enabled true|false] [--socket PATH]
bsbctl app delete <app-id> [--socket PATH]
bsbctl app config <app-id> --file PATH|- [--socket PATH]
bsbctl app setup <app-id> --file CONFIG [--token-stdin]
bsbctl app launch <app-id> [action] [--socket PATH]
bsbctl app query <app-id> <operation> [--file PATH|-] [--socket PATH]
bsbctl app command <app-id> <operation> [--file PATH|-] [--socket PATH]
bsbctl plugin list [--socket PATH]
bsbctl plugin status <plugin-id> [--socket PATH]
bsbctl plugin install <plugin-id> --catalog FILE --signature FILE --version VERSION [--socket PATH]
bsbctl plugin update <plugin-id> --catalog FILE --signature FILE --version VERSION [--socket PATH]
bsbctl plugin rollback <plugin-id> [--version VERSION] [--socket PATH]
bsbctl plugin verify --manifest PATH --fixture PATH [--executable PATH]
```

`PATH|-` means a file path or standard input. Configuration commands accept a complete JSON object, not a partial patch. `app setup` supports installed GitHub Notifications instances and requires a real configuration file when `--token-stdin` reads a token from standard input. See [Apps](../apps.md), [GitHub Notifications](../github-notifications.md), and [Plugin packaging](../plugin-packaging.md).

## Attention

```text
bsbctl attention status [--socket PATH]
bsbctl attention explain <observation-id> [--socket PATH]
bsbctl attention acknowledge <observation-id> [--socket PATH]
bsbctl attention history [--limit N] [--since DURATION] [--socket PATH]
```

See [Device and attention](../device-and-attention.md) for the decision and input model.

## Device

```text
bsbctl device screenshot [--display front|back|both] [--count N] [--interval-ms N] [--out DIR] [--config PATH]
```

Defaults are `--display both`, `--count 1`, `--interval-ms 500`, and `--config ~/.bsbctl/config.json`. `--count` cannot exceed 1000. A sequence requires an interval of at least 500 ms. Without `--out`, the command creates a unique `/tmp/bsbctl-screenshot-*` directory.

## Service

```text
bsbctl service install [--config PATH] [--socket PATH] [--plist PATH] [--log ABSOLUTE_PATH] [--stdout-path PATH] [--stderr-path PATH]
bsbctl service restart [--plist PATH]
bsbctl service uninstall [--plist PATH]
bsbctl service status [--plist PATH]
```

The default configuration is `~/.bsbctl/config.json`, the control socket is `~/.bsbctl/ctl.sock`, and the LaunchAgent property list is `~/Library/LaunchAgents/dev.bsbctl.plist`. See [Operations](../operations.md).

## Exit codes

See [Errors and exit codes](errors.md). Unknown flags, invalid arguments, and unsupported command shapes return code `2`.
