# Local Apple Calendar integration

Calendar shows reminders and active events from calendars synchronized to macOS. It requires macOS 14 or later and full Calendar access. It does not use a cloud calendar service.

## Install and enable

```sh
bsbctl setup --apps calendar
bsbctl app status calendar
```

Allow Calendar access when macOS prompts. If access is denied or revoked, restore it under System Settings > Privacy & Security > Calendars. The plugin recovers without a configuration change.

Defaults enable all discovered calendars, five-minute reminders, event names, and reminder and active-event sounds. No configuration file is needed.

## Use Calendar

Reminders show a countdown to the start; active events show a countdown to the end. All-day, canceled, ended, invalid-duration, and events lasting 24 hours or longer are ignored.

If events overlap, Calendar shows the most recently started active event or the upcoming event that starts soonest. Critical attention and a current user interaction take precedence.

Open Calendar from APPS for a read-only view. It shows the selected active event, the next reminder, or `NO EVENTS`. Press BACK to close it.

## Event options and attendance

While a reminder or active event is displayed, press START and rotate the encoder to choose:

| Choice | Effect |
| --- | --- |
| `JOIN` | Open the meeting URL and record attendance. |
| `ATTEND` | Record attendance without opening a URL. |
| `SKIP` | Hide this occurrence until it ends. |

Press OK to confirm or BACK to cancel. Automatic event display does not record attendance. These choices are available from the event card, not the APPS launcher.

Calendar rechecks the event and meeting URL before applying a choice. If saving the choice fails, it reports degraded health and retries without opening the URL again.

JOIN accepts structured EventKit URLs for Google Meet and Zoom. URLs must use HTTPS, the default port, no userinfo, and a non-root path. Links in notes, titles, descriptions, or locations are not scanned.

| Provider | Accepted hosts |
| --- | --- |
| Google Meet | `meet.google.com` |
| Zoom | `zoom.us`, its subdomains, `zoomgov.com`, and its subdomains |

## Configuration

Use these settings only to change the defaults:

| Field | Type | Default | Constraint |
| --- | --- | --- | --- |
| `reminder_enabled` | boolean | `true` | Enable the upcoming-event phase |
| `reminder_lead_minutes` | integer | `5` | 1-60 |
| `reminder_sound` | boolean | `true` | Play the reminder entry sound |
| `reminder_show_event_name` | boolean | `true` | Use `CALENDAR EVENT` when false |
| `active_enabled` | boolean | `true` | Enable the active-event phase |
| `active_sound` | boolean | `true` | Play the active entry sound |
| `active_display` | string | `event_name` | `event_name` or `theme` |
| `active_theme` | string | `meeting` | Safe lowercase firmware theme name |
| `calendars` | array | `[]` | At most 256 overrides; each requires an opaque `key` |

To select a calendar, get its configuration key:

```sh
bsbctl app query calendar calendars
```

The local query shows calendar titles, sources, priority, and settings. Configuration uses the returned opaque key. Omitted calendar overrides inherit global settings.

<details>
<summary>Per-calendar settings</summary>

| Calendar override field | Type | Default | Constraint |
| --- | --- | --- | --- |
| `key` | string | Required | Opaque `calendar-<sha256>` key returned by the `calendars` query |
| `enabled` | boolean | Inherited | Set to `false` to suppress the calendar entirely |
| `reminder_enabled` | boolean | Inherited | Override the upcoming-event phase |
| `reminder_lead_minutes` | integer | Inherited | 1-60 |
| `reminder_sound` | boolean | Inherited | Override the reminder entry sound |
| `reminder_show_event_name` | boolean | Inherited | Use `CALENDAR EVENT` when false |
| `active_enabled` | boolean | Inherited | Override the active-event phase |
| `active_sound` | boolean | Inherited | Override the active entry sound |
| `active_display` | string | Inherited | `event_name` or `theme` |
| `active_theme` | string | Inherited | Safe lowercase firmware theme name |

</details>

<details>
<summary>Complete configuration example</summary>

```json
{
  "config": {
    "reminder_enabled": true,
    "reminder_lead_minutes": 5,
    "reminder_sound": true,
    "reminder_show_event_name": true,
    "active_enabled": true,
    "active_sound": true,
    "active_display": "event_name",
    "active_theme": "meeting",
    "calendars": [
      {
        "key": "calendar-<64 lowercase hex characters>",
        "reminder_lead_minutes": 15,
        "reminder_show_event_name": false,
        "active_display": "theme",
        "active_theme": "meeting"
      }
    ]
  },
  "launch_action": "open",
  "policies": {
    "upcoming": {
      "policy": "when_relevant",
      "device_priority": 100,
      "activation_action": "calendar_event_options"
    },
    "active": {
      "policy": "when_relevant",
      "device_priority": 100,
      "activation_action": "calendar_event_options"
    },
    "interaction": {
      "policy": "interactive"
    }
  }
}
```

</details>

Replace the placeholder calendar key with one from the query, save the object as `calendar.json`, and apply it:

```sh
bsbctl app config calendar --file calendar.json
```

This replaces the full app definition. Retain every policy and launch action you need. See [configuration replacement](apps.md#change-settings-or-add-an-instance).

## Privacy and limits

The device can show event titles. Set `reminder_show_event_name=false` to show `CALENDAR EVENT` during reminders, and use `active_display=theme` during active events.

Calendar names, meeting URLs, raw EventKit identifiers, and raw errors are excluded from observations, scenes, checkpoints, and logs. Calendar names and sources appear only in the explicit local query. Event editing, calendar writes, Microsoft Teams, and cloud synchronization are unsupported.

For source builds, see [Development](maintainers/development.md#build-optional-plugins).
