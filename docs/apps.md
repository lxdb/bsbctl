# Apps

Choose an app, then follow its guide for setup and settings.

| App | App ID | Plugin ID | Use it for | Requirement | Guide |
| --- | --- | --- | --- | --- | --- |
| Calendar | `calendar` | `dev.bsbctl.calendar` | Upcoming and active Apple Calendar events | Full Calendar access on macOS 14 or later | [Calendar](calendar.md) |
| Codex | `codex` | `dev.bsbctl.codex` | Live Codex activity and device approvals | Codex CLI app-server | [Codex](codex-app-server.md) |
| Codex Quota | `codex-quota` | `dev.bsbctl.codex-quota` | Codex usage and reset windows | Authenticated Codex profile | [Codex Quota](codex-quota.md) |
| GitHub Notifications | `github-notifications` | `dev.bsbctl.github-notifications` | Unread notifications from selected GitHub.com repositories | Classic GitHub token with notifications or repo scope | [GitHub Notifications](github-notifications.md) |
| Mac Resources | `mac-resources` | `dev.bsbctl.mac-resources` | CPU, memory, and network pressure | No external account | [Mac Resources](mac-resources.md) |

## Install and manage

Setup installs selected plugin packages from the signed catalog for your core release and creates their default apps:

```sh
bsbctl setup --apps calendar,mac-resources
bsbctl app list
bsbctl app status calendar
bsbctl app disable calendar
bsbctl app enable calendar
bsbctl app delete calendar
```

Deleting an app removes that instance and its retained runtime state. Its plugin package remains installed for other instances. To recreate a built-in app with default settings, run `bsbctl app create calendar`.

## Change settings or add an instance

Use the complete JSON example in the app's guide:

```sh
bsbctl app config calendar --file calendar.json
bsbctl app create work-quota --plugin dev.bsbctl.codex-quota --file quota.json
```

The plugin package must already be installed. Configuration replaces the complete app definition, including secret references, channel policies, and launch action. Include every value you want to retain. Invalid input leaves the previous configuration unchanged.

If a command returns exit code `6`, inspect [partial results](reference/errors.md#partial-and-uncertain-results) before making another change.

## Queries and commands

A query reads state; a command can change it.

| App | Kind | Operation | Example |
| --- | --- | --- | --- |
| Calendar | Query | `calendars` | `bsbctl app query calendar calendars` |
| Codex | Query | `sessions` | `bsbctl app query codex sessions` |
| Codex | Command | `pin` | `bsbctl app command codex pin --file request.json` |
| Codex | Command | `unpin` | `bsbctl app command codex unpin --file request.json` |
| GitHub Notifications | Query | `status` | `bsbctl app query github-notifications status` |
| GitHub Notifications | Query | `items` | `bsbctl app query github-notifications items --file request.json` |

Use `--file -` to read a JSON request from standard input. See the app guide for request contents.

For device controls, see [Device and attention](device-and-attention.md). For package updates and rollback, see [Plugin packaging](plugin-packaging.md#install-from-a-catalog).
