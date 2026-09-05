# Errors and exit codes

Use the exit code to decide what to do next. Error messages exclude private details. Missing status fields mean unknown, not healthy.

| Code | Meaning | Next action |
| --- | --- | --- |
| `0` | Success | Continue. |
| `2` | Invalid usage or input | Correct the command, flag, or input document. |
| `3` | Valid request rejected | Inspect the conflict, retained version, or current generation. |
| `4` | Operational dependency or activation failure | Check the named service, plugin, provider, device, or package dependency. |
| `5` | Cancellation or deadline | Read current state before retrying. |
| `6` | Partial result or recovery required | Stop making changes and check what was saved or activated. |

## Partial and uncertain results

Exit code `6` can occur after a change was saved or a package activated. Do not assume that the command left everything unchanged.

1. Stop making changes.
2. Run `bsbctl status`.
3. Run `bsbctl app status <app-id>` or `bsbctl plugin status <plugin-id>` for the affected object.
4. Confirm the saved configuration generation and active package version before deciding whether to retry.

## Reporting an error

Include the command, exit code, redacted stderr, relevant status output, and log interval. Remove credentials, private keys, provider bodies, and sensitive paths.

See [Operations](../operations.md#diagnose-by-symptom) for symptom-based recovery.
