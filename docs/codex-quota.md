# Codex Quota

Codex Quota shows remaining usage, reset time, and quota threshold changes for authenticated local Codex profiles. It does not require the interactive Codex app-server integration.

## Install

```sh
bsbctl setup --apps codex-quota
bsbctl app status codex-quota
```

The default app reads the main Codex profile. The plugin supports at most eight accounts with unique credential homes. Enabled accounts must also have unique badges.

## Configure another profile

| Field | Type | Default | Constraint |
| --- | --- | --- | --- |
| `credentials_home` | string | Main Codex home | Absolute path or `~/` path |
| `configuration_home` | string | Main Codex home | Absolute path or `~/` path |
| `label` | string | `MAIN` | 1-12 safe ASCII characters |
| `badge` | string | `M` | One uppercase letter or digit |
| `poll_interval_seconds` | integer | `120` | 60-900 |
| `warning_remaining_percent` | integer | `20` | Greater than critical; at most 100 |
| `critical_remaining_percent` | integer | `5` | At least 0; less than warning |

<details>
<summary>Complete configuration for a work profile</summary>

```json
{
  "config": {
    "credentials_home": "~/.codex-work",
    "configuration_home": "~/.codex-work",
    "label": "WORK",
    "badge": "W",
    "poll_interval_seconds": 120,
    "warning_remaining_percent": 20,
    "critical_remaining_percent": 5
  },
  "launch_action": "open",
  "policies": {
    "summary": {
      "policy": "rotation",
      "rotation_interval_ms": 300000,
      "rotation_jitter_percent": 10
    },
    "pressure": {
      "policy": "when_relevant"
    },
    "live": {
      "policy": "interactive"
    }
  }
}
```

</details>

Save the example as `quota.json`. Create another app or replace its existing configuration:

```sh
bsbctl app create work-quota --plugin dev.bsbctl.codex-quota --file quota.json
bsbctl app config work-quota --file quota.json
```

Configuration replaces the full app definition, so retain the policies and launch action. For resets at least 60 hours away, the display uses day/hour text because the firmware countdown wraps its hours field.

## Credentials and failures

The plugin reads the access token and optional account ID from `auth.json`. It sends them to the profile's `chatgpt_base_url` in `config.toml`, which defaults to `https://chatgpt.com/backend-api`. A custom endpoint receives the token: use only an endpoint you trust. Redirects are disabled.

If a read fails, the plugin reports degraded health and lets stale observations expire. It does not report zero usage. Credentials, full home paths, and provider bodies are excluded from displays and diagnostics. Data is not sent to bsbctl services.

The summary participates in rotation every five minutes by default. A transition into low or critical quota publishes a 30-second pressure episode; unchanged pressure does not remain continuously eligible or renew at every poll. Critical pressure uses critical impact. Other active app events therefore resume after the bounded episode, while actionable notifications remain in a higher priority band.

The [Codex app](codex-app-server.md#optional-quota) can also show quota. Both providers can remain enabled. This supports installations without app-server, one-account app-server installations, and installations where app-server remains linked to one account while the standalone app follows a different active `auth.json` profile. When both providers represent the same account, their bounded threshold episodes and periodic summaries may overlap.
