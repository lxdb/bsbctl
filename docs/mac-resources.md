# Mac Resources

Mac Resources shows local CPU, memory, and network usage. It needs no account, makes no provider requests, and does not inspect processes or file contents.

## Install

```sh
bsbctl setup --apps mac-resources
bsbctl app status mac-resources
```

## Configure alerts

Warnings and critical alerts require consecutive samples above the threshold. Recovery requires usage to fall below the threshold by the recovery margin. A brief spike can change a bar's color before it triggers an alert.

| Field | Type | Default | Constraint |
| --- | --- | --- | --- |
| `sample_interval_seconds` | integer | `2` | 1-60 |
| `summary_interval_seconds` | integer | `180` | 120-300 |
| `warning_percent` | number | `70` | Greater than 0; less than critical |
| `critical_percent` | number | `90` | Greater than warning; at most 100 |
| `sustain_samples` | integer | `3` | 1-30 |
| `recovery_margin_percent` | number | `5` | 0-20 and less than warning |
| `network_capacity_bytes_per_second` | number | `10485760` | 1 KiB/s through 1 TiB/s |

Set `network_capacity_bytes_per_second` to the practical capacity of your connection. It converts throughput to a percentage; it does not limit traffic.

<details>
<summary>Complete configuration example</summary>

```json
{
  "config": {
    "sample_interval_seconds": 2,
    "summary_interval_seconds": 180,
    "warning_percent": 70,
    "critical_percent": 90,
    "sustain_samples": 3,
    "recovery_margin_percent": 5,
    "network_capacity_bytes_per_second": 10485760
  },
  "launch_action": "open",
  "policies": {
    "summary": {
      "policy": "rotation",
      "rotation_interval_ms": 60000,
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

Save the object as `resources.json` and apply it:

```sh
bsbctl app config mac-resources --file resources.json
```

This replaces the full app definition. Retain the policies and launch action.

## Read the display

| Current utilization | Default range | Semantic color |
| --- | --- | --- |
| Below `warning_percent` | Below 70% | Healthy emerald `#35D07F` |
| At or above warning, below critical | 70% to below 90% | Warning amber `#F2B84B` |
| At or above `critical_percent` | 90% and above | Danger coral `#FF786F` |

CPU and memory are evaluated independently. Front NET combines receive and transmit rates; rear RX and TX show them separately. Bar colors reflect the current sample. The `!` and `!!` markers reflect sustained warning and critical states.
