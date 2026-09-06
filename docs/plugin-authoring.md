# Plugin authoring

A bsbctl plugin is a supervised executable. It reports observations and handles declared operations; the daemon owns configuration, attention selection, and device writes.

Use the public [Go SDK](reference/plugin-sdk.md). Plugins can compile in separate modules, but production installation requires a reviewed package in an authorized signed catalog. See [packaging](plugin-packaging.md) before planning distribution.

## Build the example

The [minimal example](examples/external-plugin/main.go) implements an interactive plugin using only public SDK packages:

```sh
cd docs/examples/external-plugin
GOWORK=off GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go test ./...
```

This compiles the example; it does not run the plugin or contact a device. The module uses a local replacement for this checkout. For an external project, require a compatible immutable bsbctl release and remove that replacement.

Do not run the executable directly. `plugin.Run` expects a private socket inherited from the daemon. Do not hand-edit live configuration to register it.

## Define the plugin

Pass an immutable `plugin.Definition` to `plugin.Run`. Declare the ID, version, execution modes, channels, and operations that the executable implements. These declarations must match the package manifest. The SDK supplies protocol `1.0`; there is no version negotiation.

One process serves all enabled instances of its package. Each instance receives its configuration, resolved secrets, and optional checkpoint through RPC.

## Implement the plugin lifecycle

Implement `plugin.Plugin.ReplaceInstances` to validate the complete enabled instance set and replace it atomically. On failure, leave the prior set intact. Use `plugin.PermanentConfiguration` only when retrying cannot help without a configuration or executable change.

Add only the interfaces your plugin needs:

| Capability | Interface |
| --- | --- |
| Interactive sessions | `plugin.SessionHandler` |
| Declared queries or commands | `plugin.OperationHandler` |
| Current health | `plugin.HealthReporter` |
| Resident background work | `plugin.Shutdowner` |

The SDK runs callbacks one at a time. Your background workers can run concurrently: own their cancellation and synchronization, and stop them before shutdown returns. Pass callback contexts to host calls and provider I/O. Treat cancellation and deadlines as stop signals.

## Publish observations

Use `plugin.Host.PublishObservation` to report state. Each observation needs the exact instance generation, a declared channel, a stable key, and an increasing positive revision.

An unresolved observation needs a future expiry and one presentation: a scene or a native BUSY timer. A resolved observation has neither. Use `snapshot` for foreground state; ambient observations must be `notable` or `actionable`. Validate locally and still handle host rejection.

Reference assets by their authenticated package path or firmware stock name. Never write to the device directly. Core owns ranking, timing, rotation, and acknowledgement.

Use [the protocol specification](protocol/v1/spec.md#observations-and-presentation) for element types, dimensions, limits, and asset rules.

## Handle input and external effects

Bind work to the supplied instance generation and session token. Make session end idempotent. Input must return `consumed` or `not_consumed`; do not retry ambiguous input. For BACK, return `consumed` only if your plugin handled navigation or a terminal action. Otherwise the daemon dismisses the view and applies its cooldown.

`StartSession` runs while admission is pending. Bind the supplied observation identity and revision there; wait for session input before requesting execution. Channel policies can opt into forwarding the activating START press or encoder event after exact-session promotion:

| Daemon policy field | Accepted value | Behavior |
| --- | --- | --- |
| `activation_action` | Action identifier | Opens an observation-triggered session. |
| `activation_input` | Unset | Consumes the activating press without forwarding it. |
| `activation_input` | `"start"` | Requires `activation_action`; forwards the original press once through the session input broker after promotion. |
| `activation_input` | `"start_or_encoder"` | Requires `activation_action`; lets START or an encoder event activate the observation and forwards that original input once after promotion. |

The forwarded input retains its original UTC event time and receives the broker's ordinary sequence. Core binds it to the promoted token and generation; replacement or preemption never redirects it to another foreground session. These fields belong to daemon channel policy and do not extend the plugin configuration or session input wire format.

Before opening a URL, replying to a provider, or performing another irreversible effect:

1. Finish reversible validation and user confirmation.
2. Call `plugin.Host.BeginSessionExecution` immediately before the effect.
3. If the grant is rejected, perform no effect.
4. If granted, finish that exact action and call `plugin.Host.CompleteSession`.

Keep effects within the built-ins' five-second budget, leaving time inside the seven-second OK/Start deadline for cleanup. After a granted effect fails, terminate the session instead of returning it to interactive state. Keep recovery state only when needed to finish persistence without repeating the external effect.

See [session input](protocol/v1/spec.md#session-input) and [atomic execution](protocol/v1/spec.md#atomic-session-execution) for exact deadlines, invalidation, and error rules.

## Save state and diagnostics

Checkpoints hold up to 64 KiB of non-secret JSON per instance generation. Define your own restore and schema-version rules. Do not acknowledge a state-changing operation until checkpoint persistence succeeds, and never repeat an external effect just because saving its result failed.

Use structured host logs; supervised stdout and stderr are discarded. Keep credentials, provider bodies, and private paths out of observations, checkpoints, logs, metrics, health, and errors.

State-changing host calls can be rejected while instances are being replaced. Handle these errors using the [SDK reference](reference/plugin-sdk.md#host-methods) and [protocol lifecycle](protocol/v1/spec.md#transport-and-lifecycle).

## Package and create an app

[Package and verify the executable](plugin-packaging.md). After its package is installed, create an app with a complete configuration:

```sh
bsbctl app create APP-ID --plugin PLUGIN-ID --file config.json
```

Creation defaults to enabled; `--enabled false` creates a dormant instance. The command creates an app, not a plugin package. See [app management](apps.md) for configuration replacement and deletion.
