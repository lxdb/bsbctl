# Slack

The Slack app shows bounded activity observed after the current connection starts. It retains incoming direct messages, ordinary messages in joined public or private channels selected by the local policy, and replies in watched threads. Mentions are an overlapping flag on those locations. It does not reconstruct Slack unread state, send messages, call Slack read-state APIs, download files, or backfill history.

## Create the internal Slack app

Start with [`examples/slack-manifest.json`](../examples/slack-manifest.json). Create an app from this manifest in the workspace you will monitor. The manifest enables Socket Mode and requests the message and membership subscriptions needed for public channels, private channels, one-to-one direct messages, and group direct messages. It also grants the read scopes used to prove membership and resolve full public and private channel names.

The manifest also puts `tokens_revoked` and `app_uninstalled` under `settings.event_subscriptions.bot_events`. Slack documents these as app lifecycle events with no additional OAuth scopes, while `user_events` is the manifest field for events subscribed to on behalf of an authorized user. This placement is based on those documented semantics because Slack does not publish an event-by-event manifest compatibility matrix. Import the manifest and confirm both lifecycle event types are delivered before relying on immediate invalidation. Do not add bot scopes or a bot message subscription to work around a hypothetical importer error.

In the Slack app settings:

1. Keep Socket Mode enabled. Under **Basic Information**, generate an app-level token with only `connections:write`. This is the `app_token` and normally starts with `xapp-`.
2. Install the app to the workspace as the one human account this instance represents. The installation grants the user scopes that Slack uses to filter `user_events`. Save the generated user OAuth token, which normally starts with `xoxp-`, in Keychain as described below. For every configured public or private channel, the plugin uses it with `conversations.info` to prove current membership and resolve the channel name.
3. Record the Slack App ID, workspace ID, and that human's member ID. The plugin requires all three values in configuration. It verifies the App ID in the Socket Mode `hello` message and accepts message events only when the Events API envelope proves the matching app, workspace, and non-bot user authorization. Use a dedicated single-human installation; shared app installations can produce events whose visible authorization is not the configured human.

The shipped manifest includes each message subscription and matching user scope used by the supported domains:

| Configured domain | `settings.event_subscriptions.user_events` | `oauth_config.scopes.user` |
| --- | --- | --- |
| One-to-one direct messages | `message.im` | `im:history` |
| Accessible public channels | `message.channels` | `channels:history`, plus `channels:read` for names |
| Private channels the authorized user belongs to | `message.groups` | `groups:history`, plus `groups:read` for names |
| Group direct messages | `message.mpim` | `mpim:history` |
| Public/private membership changes | `member_joined_channel`, `member_left_channel` | `channels:read`, `groups:read` |

Keep all message subscriptions in `user_events`. The plugin detects mentions of the authorized human in ordinary message events; it does not use `app_mention` or consume a bot token. Slack channel ID prefixes do not reliably distinguish public and private conversations, so configure subscriptions and scopes from the actual conversation type. Slack limits delivery to conversations visible to the authorized user. The local `all_channels`, `channels`, direct-message, and watched-thread settings determine what bsbctl retains and displays; they do not increase what Slack delivers under the granted subscriptions and scopes.

## Store the tokens in Keychain Access

Create two **generic password** items in the macOS Keychain Access application:

| Name (service) | Account | Password | Configuration reference |
| --- | --- | --- | --- |
| `bsbctl` | `slack-app-token` | App-level `xapp-` token | `keychain://bsbctl/slack-app-token` |
| `bsbctl` | `slack-user-token` | User OAuth `xoxp-` token | `keychain://bsbctl/slack-user-token` |

The app configuration contains only these opaque references. bsbctl has no general secret-management CLI, and Slack tokens are not supported as command arguments or environment variables. The user token is required when `all_channels` is `true` or `channels` contains an explicit public or private channel. A direct-message-only configuration requires only `app_token`.

## Configure bsbctl

Copy [`examples/slack.json`](../examples/slack.json), replace `app_id`, `workspace_id`, and `user_id`, and add only the domains that have matching subscriptions and user scopes. Then replace the whole default app definition:

```json
{
  "config": {
    "app_id": "A01234567",
    "workspace_id": "T01234567",
    "user_id": "U01234567",
    "all_channels": true,
    "channels": [],
    "direct_messages": true,
    "group_direct_messages": true,
    "watched_threads": [],
    "watch_participated_threads": true,
    "label": "SLK",
    "rear_details": false,
    "front_message_preview": false
  },
  "secrets": {
    "app_token": "keychain://bsbctl/slack-app-token",
    "user_token": "keychain://bsbctl/slack-user-token"
  },
  "policies": {
    "attention": {"policy": "attention", "requires_ack": true, "activation_action": "open", "activation_input": "start_or_encoder"},
    "connection": {"policy": "when_relevant", "activation_action": "open"},
    "live": {"policy": "interactive"}
  },
  "launch_action": "open"
}
```

```sh
bsbctl app config slack --file slack.json
bsbctl app status slack
bsbctl app query slack status
printf '%s\n' '{"limit":20}' | bsbctl app query slack items --file -
```

`app config` replaces the complete app record. Keep the app-token secret reference, every channel policy, and `launch_action` in the file. Invalid or incomplete configuration leaves the previous generation unchanged. Exit code `6` means the result is partial or uncertain; inspect app status before retrying.

The `items` query accepts `{}` for the default limit of 20 or `{"limit":N}`, where `N` is 1 through 50.

To select channels explicitly, set `all_channels` to `false` and provide objects with the required `id` and `alias` fields:

```text
"channels": [
  {"id": "C01234567", "alias": "BUILD"},
  {"id": "G01234567", "alias": "PRIVATE"}
]
```

Channel IDs must start with `C` or `G` and contain only uppercase letters or digits after the prefix. Aliases must contain 1 through 100 printable ASCII characters, including at least one non-space character.

| Field | Type | Default | Limits and behavior |
| --- | --- | --- | --- |
| `app_id` | String | Required when configured | Slack App ID beginning with `A`; checked against Socket Mode `hello` and every Events API callback |
| `workspace_id` | String | Required when configured | Slack workspace ID beginning with `T`; checked against every Events API envelope |
| `user_id` | String | Required when configured | Human Slack member ID beginning with `U` or `W`; checked against a non-bot authorization in every message envelope |
| `all_channels` | Boolean | `false` | Retain ordinary messages from every joined public or private channel after current membership is proven; use `CHANNEL` until a name is available |
| `channels` | Array | `[]` | Up to 32 `{id, alias}` objects for joined public/private conversations; an explicit ID selects policy scope but does not bypass the membership proof |
| `direct_messages` | Boolean | `true` | Retain incoming one-to-one DMs |
| `group_direct_messages` | Boolean | `false` | Retain group DMs only when explicitly enabled |
| `watched_threads` | Array | `[]` | Up to 32 unique `{channel_id, thread_ts}` roots in a selected or enabled domain |
| `watch_participated_threads` | Boolean | `true` | Watch in-scope threads where the authorized user participates or is mentioned |
| `label` | String | `SLK` | 1-100 printable ASCII characters, including at least one non-space character; long values scroll on the front display |
| `front_message_preview` | Boolean | `false` | Explicitly allow transient sanitized message text on the public front display, independently of rear details |
| `rear_details` | Boolean | `false` | Allow transient sanitized message detail on the private rear display |

Exact `{}` is the healthy unconfigured default. It resolves no secrets, opens no Slack connection, publishes no proactive summary or attention, and keeps the launcher setup panel available. Any nonempty configuration requires `app_id`, `workspace_id`, and `user_id`; runtime validation also requires watched roots to belong to a selected or enabled domain. Direct-message-only instances require exactly the resolved secret name `app_token`. Instances with `all_channels: true` or a nonempty `channels` list require exactly `app_token` and `user_token`.

## Activity and recovery behavior

### Pending state and privacy

Pending counts describe unhandled activity observed locally, not Slack unread counts. Each pending item has one location: DM, channel, or thread. Mentions overlap those locations and must not be added to the location total.

Mentions and direct-conversation activity can publish attention. Ordinary channel and watched-thread activity remains available through the launcher and local queries. It does not enter ambient rotation or interrupt the device.

The plugin keeps at most 128 records, 32 attention items, and bounded metadata checkpoints. Default displays, logs, queries, and checkpoints exclude message bodies, credentials, provider IDs, and URLs. With `rear_details: true`, sanitized message text is transient and limited to 160 UTF-8 bytes on the rear display. The separate `front_message_preview: true` option permits that text on the public front display. Neither option stores message bodies in checkpoints.

### Socket coverage

A Socket Mode acknowledgement confirms transport receipt only. If the queue is full, the plugin acknowledges and drops the new envelope. It then records a persistent coverage gap.

During a Slack-requested refresh, the plugin keeps the established connection active until a replacement returns a valid `hello`. It then retires the old connection. A failed replacement attempt does not interrupt coverage while the established connection remains active. If that connection is lost before promotion, the plugin records a persistent gap.

Disconnects, throttling, lost lifecycle events, and events that Slack never delivers can also leave coverage incomplete. The plugin does not call `conversations.history` to reconstruct missing activity.

Current connectivity and historical coverage are separate. A live connection is operationally healthy even when `coverage_incomplete` remains true from an earlier loss. A new coverage loss publishes one connection warning for 15 seconds; after that, the persistent warning remains in the manual panel and `status` output without occupying ambient rotation. A current disconnect, stale connection, authentication failure, membership lookup failure, or checkpoint failure remains degraded.

### Channel membership and metadata

Public and private channel messages are held until `conversations.info` proves that the configured user is a member. This applies to `all_channels` and explicit `channels`; direct messages remain independent. Lookups run on a separate worker, with at most one active lookup per channel, so they do not delay Socket Mode acknowledgements or direct-message reduction. Positive proofs are cached for five minutes and negative proofs for one minute.

The worker holds at most 128 unverified normalized events in memory and never writes their message bodies to a checkpoint. Before holding the first event, it saves a content-free unresolved-admission marker and clears that marker after all pending admissions are resolved. Restarting from a checkpoint that still contains the marker creates a persistent coverage gap because an acknowledged event may have been lost. Overflow drops the new event, records `queue_overflow`, and also creates a persistent coverage gap. A lookup failure retains the bounded pending events for retry and degrades current health without claiming that history was lost. A confirmed non-member result discards pending events for that channel. A matching `member_left_channel` event also removes that channel's retained local items; a matching `member_joined_channel` event permits pending activity immediately. Channel names are local presentation metadata and are not checkpointed.

### Authorization limits

The app-level token cannot call `auth.test` as the installed human, so bsbctl cannot preflight the granted user scopes. A missing subscription or scope can remain silent until you check the manifest and real event delivery.

A delivered matching `tokens_revoked` or `app_uninstalled` event invalidates authorization immediately. An omitted or lost lifecycle subscription cannot be detected from an otherwise healthy idle WebSocket pong.

### Local actions and controls

`Open` launches the documented `slack://channel?team=...&id=...` conversation target once. Successful opening immediately removes the selected episode from local pending counts and lists. If saving that change fails, the app displays “Slack opened; local pending state is not saved yet” and retries only the checkpoint. It never opens Slack again automatically. A failed opener leaves the item pending.

`Dismiss` removes only the selected local episode after its checkpoint is saved. Neither action calls a Slack read-state API; the desktop client controls its own read behavior. The `status` query reports `pending_count`, current `connected` state, persistent `coverage_incomplete`, and actual local-result `truncated` state. The `items` query returns only pending local episodes without a provider request; its `truncated` field reports local result truncation, not historical provider coverage.

Play/Pause on an attention card opens its selected conversation. Turning the knob on that card opens the action selector with Open in Slack selected; later turns change the selected action.

In the manual list, turn the knob to select an item and press Play/Pause to open its details. In details, turn the knob to select Open in Slack, Dismiss, or Read message. Read message is available only when rear details are enabled and the item has message content. Press Play/Pause to apply the selected action, or BACK to return to the list. Open and Dismiss require the host execution grant and complete the session once.

In the message reader, turn the knob to scroll pages and press BACK to return to the actions. Pressing the knob has no effect in any Slack view.

If a manual panel opens empty, new arrivals show `TURN TO SELECT`. Turn the knob to select an item before pressing Play/Pause. Background updates do not choose a target for that panel.

## Live checks before relying on the app

The device-captured mock preview does not certify a Slack workspace or live provider behavior. Verify these manually with the actual internal app:

- The manifest imports, the user subscriptions deliver each enabled conversation type, and both lifecycle subscriptions reach the Socket Mode connection.
- Socket Mode `hello` names the configured `app_id`. Every delivered message envelope names that app and the configured workspace and includes the configured human as a non-bot authorization. Test wrong `app_id`, `workspace_id`, and `user_id` values locally and confirm bsbctl rejects those connections or events.
- Each enabled conversation type delivers only after its matching user scope is granted. Confirm that `channels:read` and `groups:read` resolve full names for public and private channels. Reinstall the app after adding scopes; bsbctl cannot prove the installed scope set with its app-level token.
- Revoking the user token and uninstalling the app move the instance to `auth_required` when their events are delivered.
- `Open` reaches the intended conversation in the installed Slack desktop client; verify its read behavior there.
- Front and private rear scenes are legible on the physical device, selectors behave as documented, and stale observations expire at the device boundary.

Primary Slack references:

- Event delivery and authorization: [Events API](https://docs.slack.dev/apis/events-api/), [message events](https://docs.slack.dev/reference/events/message/), and [Slack token types](https://docs.slack.dev/authentication/tokens/).
- Socket lifecycle: [Socket Mode](https://docs.slack.dev/apis/events-api/using-socket-mode/) and [`apps.connections.open`](https://docs.slack.dev/reference/methods/apps.connections.open/).
- App configuration: [app manifests](https://docs.slack.dev/reference/app-manifest/) and [`connections:write`](https://docs.slack.dev/reference/scopes/connections.write/).
- Channel metadata and history: [`conversations.info`](https://docs.slack.dev/reference/methods/conversations.info/), [`conversations.history`](https://docs.slack.dev/reference/methods/conversations.history/), [`channels:read`](https://docs.slack.dev/reference/scopes/channels.read/), and [`groups:read`](https://docs.slack.dev/reference/scopes/groups.read/).
- Authorization lifecycle: [`tokens_revoked`](https://docs.slack.dev/reference/events/tokens_revoked/) and [`app_uninstalled`](https://docs.slack.dev/reference/events/app_uninstalled/).
- Desktop launch targets: [native deep links](https://docs.slack.dev/interactivity/deep-linking/).
