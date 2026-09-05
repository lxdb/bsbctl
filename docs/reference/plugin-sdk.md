# Plugin SDK reference

Start with [Plugin authoring](../plugin-authoring.md) and the [compiling example](../examples/external-plugin/main.go). This page lists the main entry points. Go documentation owns the complete declarations, fields, constants, and methods.

| Package | Use it for |
| --- | --- |
| [`sdk/plugin`](../../sdk/plugin/plugin.go) | Define a plugin and call daemon services. |
| [`sdk/protocol`](../../sdk/protocol/types.go) | Construct and validate protocol values. |
| [`sdk/rpc`](../../sdk/rpc/rpc.go) | Implement a transport peer outside the normal plugin runtime. |

From the repository, read the complete API with:

```sh
go doc -all github.com/lxdb/bsbctl/sdk/plugin
go doc -all github.com/lxdb/bsbctl/sdk/protocol
go doc -all github.com/lxdb/bsbctl/sdk/rpc
```

## Definition and lifecycle

| Entry point | Responsibility |
| --- | --- |
| `plugin.Definition`, `plugin.Contract` | Declare identity, capabilities, and handler construction. |
| `plugin.Run` | Serve the daemon's inherited socket and manage callback dispatch and shutdown. |
| `plugin.Plugin.ReplaceInstances` | Atomically replace the complete enabled instance set. |
| `plugin.SessionHandler` | Start sessions, handle input, and end the exact session idempotently. |
| `plugin.OperationHandler` | Handle declared queries and commands. |
| `plugin.HealthReporter` | Report current health. |
| `plugin.Shutdowner` | Cancel and join resident work before the deadline. |
| `plugin.PermanentConfiguration` | Mark a replacement error that requires a configuration or executable change. |
| `plugin.RejectSecrets` | Reject secret inputs when the plugin accepts none. |

## Host methods

| Method | Effect |
| --- | --- |
| `plugin.Host.PublishObservation` | Publish or replace one observation. |
| `plugin.Host.WithdrawObservation` | Remove an observation; removing an absent identity succeeds. |
| `plugin.Host.SaveCheckpoint` | Save up to 64 KiB of non-secret state for one instance generation. |
| `plugin.Host.BeginSessionExecution` | Obtain the final grant immediately before an irreversible session effect. |
| `plugin.Host.CompleteSession` | Finish the exact foreground session. |
| `plugin.Host.Log` | Send a bounded structured diagnostic. |
| `plugin.Host.RecordMetric` | Attempt a lossy metric; do not spin when the queue drops it. |

Until the first instance replacement commits, and during later replacements, state-changing calls return `not_ready`. Execution admission instead returns `session_generation_mismatch`. Logs and metrics remain available. Follow the [replacement rules](../protocol/v1/spec.md#transport-and-lifecycle).

## Protocol values

Use `protocol.InstanceRef` for generation-bound identity, `protocol.Observation` for published state, and `protocol.DomainError` for typed failures. Call validators before sending values, but still handle host rejection.

The [protocol specification](../protocol/v1/spec.md) defines sequencing, payload ownership, limits, and errors. The [JSON Schema](../protocol/v1/schema.json) checks structure; it does not cover every byte limit or cross-field rule.

## RPC transport

Most Go plugins need only `plugin.Run`. For an independent peer, `rpc.NewPeer` takes ownership of a private connection; do not read, write, or set deadlines on it afterward. Keep `rpc.Peer.Serve` running while making calls. `rpc.Peer.Close` joins transport workers without waiting for active handlers.

Protocol `1.0` uses JSON-RPC 2.0 framing and has no negotiation or fallback. See [Compatibility](compatibility.md).
