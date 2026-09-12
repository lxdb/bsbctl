# Device and attention

bsbctl selects one presentation from the enabled apps. Plugins report state; the daemon decides what to show and handles device input. Do not run another device writer alongside it.

## Controls

| Input | Effect |
| --- | --- |
| Encoder rotation | Select a visible item or action, or scroll the message reader. Rotation never executes an action. |
| OK / encoder press | No action in the launcher or bundled apps. |
| Play/Pause | Open the selected item or execute the displayed action. Release before pressing again. Plugin input identifies this button as `start`. |
| BACK | Let the app handle navigation first; otherwise close the current view. |

The APPS selector lists enabled, ready apps with an interactive policy and launch action. Turn to select an app and press Play/Pause to open it. Calendar and Codex launcher panels are read-only; their actions start from an exact event or request card. Slack and GitHub notification lists let you open an item and select its action. BACK returns to the list without executing an action.

Each action applies to the selected item. Background updates must not substitute another target. Codex approvals require a separate confirmation press; question answers are submitted from a final review. External plugins can still receive the protocol's `ok` input and define their own controls.

When an app does not consume BACK, the daemon closes the current view and suppresses non-critical presentation for 30 seconds. Critical actionable attention can still appear.

## Inspect the current decision

```sh
bsbctl attention status
bsbctl attention explain <observation-id>
bsbctl attention history --limit 20
bsbctl attention history --since 30m
```

`status` identifies the selected observation. `explain` reports why an observation is eligible, suppressed, or outranked. `history` shows recent decisions in chronological order and marks truncated results; it is not an unlimited audit log.

To acknowledge one known observation:

```sh
bsbctl attention acknowledge <observation-id>
```

Acknowledgement follows that observation's policy. It does not disable the app or resolve other observations.

## Diagnose a missing scene

1. Run `bsbctl status` and check device readiness.
2. Run `bsbctl attention status` to see what was selected.
3. Use `bsbctl attention explain <observation-id>` for the expected observation.
4. Check the device switch and current foreground session.
5. If `presentation_cooldown.active` is true, wait for the cooldown to expire.

Use [Operations](operations.md) for service failures. Plugin authors can find identity, revision, and input rules in the [protocol specification](protocol/v1/spec.md).
