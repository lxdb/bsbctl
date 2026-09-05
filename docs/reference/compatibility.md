# Compatibility

The current executable plugin protocol is exactly `1.0`.

| Boundary | Contract |
| --- | --- |
| RPC framing | JSON-RPC 2.0, newline-delimited messages, no batches |
| Protocol selection | Exact string `1.0` |
| Version negotiation | None |
| Maximum RPC message | 1 MiB |
| Structural schema | `docs/protocol/v1/schema.json` |
| Normative behavior | Go validators and `docs/protocol/v1/spec.md` |

Core, the SDK, runtime configuration, package manifest, and plugin initialization result must agree on the protocol string. A semantic plugin version change does not change the protocol automatically.

The JSON Schema validates fixture structure. It does not express every byte limit, Unicode-control rule, or cross-field relationship. Use the public Go types and validators when writing a Go plugin. An independent implementation must satisfy both the specification and the behavioral validation enforced by core.

Do not silently reinterpret durable configuration or checkpoint data. Checkpoint content and any schema version are plugin-owned; the protocol does not require a version field, and core does not interpret checkpoint fields.

## Durable configuration

The daemon configuration schema is exactly version `1`. Unknown fields, unsupported versions, and zero app generations are rejected. `init` and `setup` create version 1. Calendar and Codex checkpoints require `schema_version: 1`; unsupported or unversioned checkpoints are not restored.

See [Protocol v1](../protocol/v1/spec.md), [Plugin SDK](plugin-sdk.md), and [Plugin packaging](../plugin-packaging.md).
