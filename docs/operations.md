# Operations

One daemon owns the device, plugins, and configuration. Do not run another writer against the same BUSY Bar or edit the live configuration by hand.

## Manage the LaunchAgent

After [setup](getting-started.md), use:

```sh
bsbctl service status
bsbctl service restart
bsbctl status
```

After a restart, confirm that required plugins and the device are ready. A new process ID alone does not prove recovery.

`bsbctl service install` installs or updates the per-user service definition. `bsbctl service uninstall` unloads it and removes the managed property list. Both require a valid initialized configuration.

| State | Default path |
| --- | --- |
| Configuration | `~/.bsbctl/config.json` |
| Control socket | `~/.bsbctl/ctl.sock` |
| LaunchAgent property list | `~/Library/LaunchAgents/dev.bsbctl.plist` |

Use matching `--config`, `--socket`, and `--plist` overrides when working with custom paths. See the [CLI reference](reference/cli.md#service) for supported flags.

For explicit file diagnostics, use `service install --log ABSOLUTE_PATH`. The file uses mode `0600`, rotates at 10 MiB, and retains three archives. Its immediate parent must exist or be creatable; bsbctl creates a missing parent with mode `0700` and rejects symlinked paths.

## Start a foreground daemon

After [initializing a source build](maintainers/development.md#build-a-minimal-local-system), run:

```sh
./bin/bsbctl daemon
```

Keep it running and use another terminal for commands. Do not run it alongside the LaunchAgent for the same device.

## Capture device screens

```sh
bsbctl device screenshot --display both
bsbctl device screenshot --display back --out ./capture
bsbctl device screenshot --display both --count 10 --interval-ms 500
```

The command reads directly from the BUSY Bar using the configured URL and Keychain token. It does not need the daemon.

Without `--out`, it creates a unique `/tmp/bsbctl-screenshot-*` directory. An explicit output path must be absent or an empty real directory. Output includes scaled PNGs and `manifest.json` with hashes, dimensions, and timing; stdout contains the same manifest.

Sequences allow at most 1,000 rounds and an interval of at least 500 ms. If a later capture fails, completed images remain and the command returns exit code `6` with a partial manifest. See [CLI defaults](reference/cli.md#device).

## Diagnose by symptom

### The control socket is unavailable

1. Run `bsbctl service status`.
2. Confirm that the command uses the service's socket path.
3. Inspect the configured LaunchAgent stderr path or daemon log.

Restart after correcting the service definition or paths. Confirm recovery with `bsbctl status`.

### A plugin is not healthy

```sh
bsbctl app status <app-id>
bsbctl plugin status <plugin-id>
```

Use the reported lifecycle state and stable error code to identify a configuration, startup, provider, or protocol failure. Correct that cause and verify that the affected app reports ready. The [app guides](apps.md) cover provider-specific requirements.

### The device does not show the expected scene

Check [attention decisions and device controls](device-and-attention.md#diagnose-a-missing-scene). A Back action can suppress non-critical presentation for 30 seconds. Wait for the cooldown to expire instead of restarting the daemon.

### A mutation returns exit code 6

Stop making changes. Read daemon, app, and plugin status before retrying: the earlier change may already be committed. Follow [partial-result recovery](reference/errors.md#partial-and-uncertain-results).

### Attention state is degraded

Acknowledgements or repeat suppression may revert to the last confirmed saved state after a restart. Read the status code and check persistence errors. The daemon can still start; launcher and interactive sessions are not restored from this state.

## Ask for help

Include the exact command, exit code, redacted error, relevant status output, and log interval. Remove credentials and private provider data. See [Support](../SUPPORT.md) and [exit codes](reference/errors.md).
