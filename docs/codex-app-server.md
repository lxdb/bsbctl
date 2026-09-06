# Codex app-server integration

Codex shows local session activity and lets you answer supported approval requests on the BUSY Bar. It connects through `~/.local/bin/codex app-server proxy`. It does not install hooks, start the app-server daemon, or restart Codex.

## Install and enable

You need a working local Codex app-server. Only one enabled Codex app instance is supported.

```sh
bsbctl setup --apps codex
bsbctl app status codex
```

By default, the device can show command, reason, path, and permission details. For a shared or visible device, set `show_sensitive_request_details=false` using the configuration example below. Command output, assistant output, plan text, and raw RPC payloads are never displayed.

## Read the display

Cards identify the working directory and session without showing the full directory path.

| Display | Meaning |
| --- | --- |
| `CODEX ...` / `CODEX ON` / `CODEX OFF` | Connecting / connected / unavailable |
| `RUN` / `PIN` / `<n> ACT` | A turn started / pinned activity / multiple active threads |
| `PLAN x/y` / `PLAN READY` | Plan progress / completed plan |
| `COMPACT` / `COMPACTED` | Context compaction in progress / complete |
| `WAIT CMD` / `WAIT FILE` / `WAIT PERM` | A supported approval needs an answer |
| `ASK` | A supported question needs an answer |
| `OPEN CODEX` | Continue in Codex; the card has no device action |
| `DONE` / `STOP` / `FAIL` | Turn completed / interrupted / failed |

Open Codex from APPS for a read-only live view of the thread or run changed by the latest event. The panel stays open across reconnects. BACK closes it; other controls have no effect.

## Answer a request

Press START on an actionable card. For command, file, permission, or interrupt controls:

1. Rotate to select an option.
2. Press OK to stage it.
3. Press OK again to confirm.

BACK clears a staged choice before closing the session.

For `ASK`, rotate through the options and press OK to advance through the questions and submit the final answer. BACK cancels the device session; START has no effect while it is open. `Answer in Codex` closes the device session without submitting answers or interrupting the turn.

Device answers support non-secret requests with at most eight questions and one to eight explicit options each. Secret, free-text-only, unsupported, or ambiguous requests remain display-only.

Actions stay bound to the displayed request. A stale request or disconnect closes its actionable session; input cannot act on a newer request.

## Configuration

| Field | Type | Default | Constraint |
| --- | --- | --- | --- |
| `show_sensitive_request_details` | boolean | `true` | Use generic context for sensitive request kinds when false |
| `show_quota` | boolean | `false` | Enable quota from the app-server connection |
| `quota_warning_remaining_percent` | integer | `20` | 1-100 and greater than critical |
| `quota_critical_remaining_percent` | integer | `5` | 0-99 and less than warning |

<details>
<summary>Complete example: hide sensitive details and enable quota</summary>

```json
{
  "config": {
    "show_sensitive_request_details": false,
    "show_quota": true,
    "quota_warning_remaining_percent": 20,
    "quota_critical_remaining_percent": 5
  },
  "launch_action": "open",
  "policies": {
    "attention": {
      "policy": "attention",
      "activation_action": "open"
    },
    "guidance": {
      "policy": "when_relevant"
    },
    "outcome": {
      "policy": "when_relevant",
      "activation_action": "open"
    },
    "activity": {
      "policy": "rotation",
      "activation_action": "open",
      "rotation_interval_ms": 30000,
      "rotation_jitter_percent": 10
    },
    "progress": {
      "policy": "when_relevant",
      "activation_action": "open",
      "cooldown_ms": 1
    },
    "overview": {
      "policy": "rotation",
      "activation_action": "open",
      "rotation_interval_ms": 60000,
      "rotation_jitter_percent": 10
    },
    "connection": {
      "policy": "when_relevant",
      "activation_action": "open"
    },
    "detail": {
      "policy": "interactive"
    },
    "quota-summary": {
      "policy": "rotation",
      "rotation_interval_ms": 300000,
      "rotation_jitter_percent": 10
    },
    "quota-pressure": {
      "policy": "when_relevant"
    }
  }
}
```

</details>

Save the object as `codex.json` and apply it:

```sh
bsbctl app config codex --file codex.json
```

This replaces the full app definition. Retain every policy and launch action you need. Hiding request details still shows explicit question prompts and options.

## Optional quota

`show_quota=true` reads quota over the existing app-server connection at startup and every 120 seconds, with updates from provider notifications. Missing data is unavailable, not zero usage. Summary observations expire six minutes after the last provider update. A transition into low or critical quota creates a 30-second pressure episode; unchanged pressure does not remain continuously eligible.

Quota failures do not stop thread or approval monitoring. You can also use the standalone [Codex Quota app](codex-quota.md). Both can remain enabled when app-server and the active standalone `auth.json` profile represent different accounts; when they represent the same account, their bounded threshold episodes and periodic summaries may overlap.

## List and pin sessions

```sh
bsbctl app query codex sessions
printf '%s\n' '{"thread_id":"THREAD_ID"}' | \
  bsbctl app command codex pin --file -
bsbctl app command codex unpin
```

The query returns up to 128 loaded-thread summaries with ID, title, status, and pin state. Only an active thread can be pinned. Pins survive restart and clear when the thread is no longer loaded.

## Connection problems

Check that the fixed Codex executable and app-server work, then inspect `bsbctl app status codex`. The plugin reconnects with backoff. Incompatible required methods or unsafe actionable requests are rejected; unknown notifications and additional response fields are ignored.

For source builds, see [Development](maintainers/development.md#build-optional-plugins). Protocol rules belong in the [plugin specification](protocol/v1/spec.md). A physical-device and app-server soak is still needed for release verification.
