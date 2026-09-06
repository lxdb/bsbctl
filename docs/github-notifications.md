# GitHub Notifications

GitHub Notifications shows unread notifications from selected GitHub.com repositories. Collection uses read requests. START opens the exact comment or subject, then marks that GitHub notification thread read. Dismiss marks it read without opening.

## Before setup

Install the built-in app through `bsbctl setup`, or add it later:

```sh
bsbctl setup --apps github-notifications
```

`bsbctl app setup` requires a classic GitHub personal access token with the `notifications` or `repo` scope. Use `repo` when private repository notification subjects require repository access. Fine-grained personal access tokens and GitHub App tokens are not supported.

By default, bsbctl collects the authenticated user's unread notification inbox across all repositories. An optional repository list limits local collection. The repository filter is not a permission boundary. The token can retain access to other resources. Grant only the access needed for the account and store the credential in macOS Keychain.

## Configure the app

Copy [`examples/github-notifications.json`](../examples/github-notifications.json). Its empty `repositories` array selects all repositories. The file contains app settings only; it must never contain the token.

```json
{
  "config": {
    "repositories": [],
    "label": "GH",
    "rear_details": false,
    "poll_interval_seconds": 60
  }
}
```

Run dedicated setup against the installed app:

```sh
bsbctl app setup github-notifications --file examples/github-notifications.json
```

Credential sources have this precedence:

1. A `secrets.token` reference explicitly supplied in the setup JSON.
2. The app's existing valid configured reference.
3. The result of `gh auth token --hostname github.com`.
4. A token entered at the hidden terminal prompt.

When bsbctl imports a `gh` token or accepts a manual token, it validates the authenticated identity and notification capability. When a repository filter is present, it also validates every configured repository. It then copies the token into a new bsbctl-owned Keychain entry and saves a `keychain://bsbctl/...` reference in the app configuration. It does not change the GitHub CLI login or reuse the `gh` credential in place.

For automation, keep the JSON in a real file and provide the token through standard input:

```sh
security find-generic-password -w -s github-notifications-bootstrap |
  bsbctl app setup github-notifications \
    --file examples/github-notifications.json \
    --token-stdin
```

Do not combine `--token-stdin` with `--file -` or an explicit `secrets.token` reference. The token is accepted only through secure input; do not put it in command arguments, environment configuration, JSON, or logs.

To use an existing Keychain entry, add this top-level object to the setup JSON:

```json
{
  "secrets": {
    "token": "keychain://bsbctl/github-notifications-example"
  }
}
```

An explicit reference is authoritative. If it is missing, rejected, or lacks configured repository access, setup reports the error and does not substitute another account.

## Settings

| Field | Meaning | Default |
| --- | --- | --- |
| `repositories` | Optional local filter of up to 32 GitHub.com repositories. An empty array collects notifications from all repositories. Required as an explicit field in a configured app. | `[]` |
| `label` | Public-safe label shown on the device, up to eight ASCII characters. | `GH` |
| `rear_details` | Also show repository names and notification titles on the rear display. Front notification cards always show the subject and full repository name. | `false` |
| `notification_reasons` | `"actionable"`, `"all"`, or a nonempty array of unique known reasons. Filters attention only; the manual list keeps every collected unread thread. | `"actionable"` |
| `poll_interval_seconds` | Requested collection interval from 60 through 900 seconds. GitHub can require a longer interval. | `60` |

Each optional `repositories` entry has these fields:

| Field | Meaning | Default |
| --- | --- | --- |
| `name` | GitHub.com repository in `owner/repo` form. | None |
| `alias` | Unique, public-safe device label of up to eight printable ASCII characters. | None |

The empty configuration `{}` is the installed, unconfigured state. Use `app setup` to supply a complete configuration and credential together. Running setup again replaces the complete provider configuration while preserving omitted app policies and the launch action.

## Display and controls

The initial complete result creates one attention episode for each current unread thread whose reason matches the configured reason filter. This lets pending actionable notifications interrupt normal rotation after setup or restart. A new thread, reason change, or later `updated_at` creates a new episode for that thread. Repeated responses do not create duplicate episodes. The episode remains eligible until the thread is read or handled. There is no ambient quiet card or persistent GitHub summary.

| View or input | Result |
| --- | --- |
| Notification card + START/PAUSE | Opens the latest comment when available, otherwise the exact subject, then marks that notification thread read. |
| Notification card + encoder | Shows **Dismiss this GitHub notification?** for that exact thread. |
| Dismiss + START/PAUSE | Marks the thread read without opening GitHub. |
| Dismiss + BACK | Cancels dismissal and returns to the card. |
| APPS launcher | Opens the current unread list, including threads excluded by the attention filter. |
| Manual list + encoder / OK | Selects a thread / opens its detail. |
| Manual list or detail + START/PAUSE | Opens and marks the selected thread read. |

The fixed 16x16 GitHub icon occupies x=0..15. Pixels x=16..17 stay empty; only the main text in x=18..71 scrolls. Cards show a full reason phrase and subject title, with `owner/repository` in the small row. This front-display content is visible by default; `rear_details` continues to control only the rear display.

`notification_reasons` defaults to `"actionable"`: `approval_requested`, `assign`, `invitation`, `mention`, `review_requested`, `security_alert`, and `team_mention`. An explicit array may also select `author`, `ci_activity`, `comment`, `manual`, `member_feature_requested`, `security_advisory_credit`, `state_change`, or `subscribed`. `"all"` includes unrecognized provider reasons using the generic phrase `GitHub update`. Duplicate, empty, and unknown explicit values are rejected.

Open resolves `latest_comment_url` first and then the subject API URL. Both must belong to the captured repository, and the returned browser URL must be a safe GitHub HTTPS URL in that repository. There is no generic inbox fallback. If neither target is available, the app leaves the item available and explains that Dismiss remains possible.

**Dismiss means mark read**, not mark done, unsubscribe, or close the issue or pull request. Both actions send one `PATCH /notifications/threads/{thread_id}`; `205` and `304` confirm the outcome. Opening and marking read are ordered external effects under one execution grant, and are not atomic. A failed opener sends no PATCH. A rejected PATCH after successful Open reports that GitHub opened but the notification could not be marked read.

If the PATCH response is lost or ambiguous, the app checkpoints a reconciliation marker, withdraws that attention episode, and checks GitHub again using read requests. It never automatically repeats the write or browser launch. A complete fresh result showing the same thread unread allows another explicit user action; a read or absent thread removes the card. Partial results and `304` alone do not settle an ambiguous write. GitHub provides no `updated_at` compare-and-set for this endpoint, so activity arriving during a PATCH can also become read.

Setup, token, rate-limit, target and incomplete-coverage cards explain the situation in full text. Successful Open and Dismiss remove the card without a persistent success banner.

## Inspect collection

The plugin exposes read-only `status` and `items` queries:

```sh
bsbctl app query github-notifications status
bsbctl app query github-notifications items --file request.json
```

For `items`, the request is a JSON object with an optional `limit` from 1 through 50, for example `{"limit":20}`. Results expose stable local IDs, reason, update time, and locally handled state. They omit account identity, repository names, titles, URLs, and credentials.

A connection card or a `degraded` status means the inbox may be stale or partial. bsbctl retains known items rather than treating an incomplete response as proof that a notification disappeared. The `truncated` field means either provider coverage or the bounded local result is incomplete. Correct authentication, repository access, throttling, or connectivity before relying on the displayed count.

## Rotate or recover a credential

Create or select the replacement token, then rerun `app setup` with `--token-stdin` or an explicit Keychain reference. A rejected existing credential can fall through to `gh` or the hidden prompt. Network, throttling, canceled storage, or an unreadable Keychain does not prove the credential is invalid, so setup stops instead of silently changing identity.

Successful output includes `status`, `app_id`, `generation`, and the saved `secret_reference`. Record that reference before deleting an old Keychain item.

Exit code `6` means storage or configuration had a partial or uncertain outcome. The JSON output includes the saved `secret_reference` and `configuration_status`, such as `not_attempted`, `not_applied`, `partial`, `durability_uncertain`, or `unknown`. Keep the new Keychain entry. Check `bsbctl app status github-notifications` and the local configuration, then retry with the explicit saved reference after the current state is known. Do not delete either credential while the configuration outcome is uncertain.

See [partial-result recovery](reference/errors.md#partial-and-uncertain-results) and [Operations](operations.md) for daemon and Keychain failures.

Primary GitHub references:

- Authentication and endpoint behavior: [REST API endpoints for notifications](https://docs.github.com/en/rest/activity/notifications).
- Classic personal access token scopes: [Scopes for OAuth apps](https://docs.github.com/en/apps/oauth-apps/building-oauth-apps/scopes-for-oauth-apps).
