# Contributing

Keep changes focused, testable, and compatible with the daemon's protocol, lifecycle, privacy, and release contracts.

## Before you start

Open an issue before a large protocol, plugin lifecycle, storage, or release-format change. Do not include credentials, device identifiers, calendar details, Codex session content, or private configuration. Read the [protocol specification](docs/protocol/v1/spec.md) before changing the plugin wire contract and the [release runbook](docs/release.md) before changing artifacts or publication.

## Submit a change

1. Create a focused branch.
2. Add tests for observable behavior when the risk justifies them.
3. Run the relevant `scripts/verify.sh` phase, then `scripts/verify.sh quick`.
4. Run `scripts/verify.sh all` before requesting review when the change affects shared daemon behavior.
5. Update operator, plugin-author, protocol, or release documentation when that contract changes.
6. Use a Conventional Commit pull-request title.
7. Describe risks, verification evidence, and every check that was not run.

Keep unrelated formatting, generated output, and cleanup out of the change. Physical BUSY Bar verification is required only when a change affects device behavior; name the hardware and firmware used when reporting that evidence.

## Commit and pull-request titles

Use the standard types `build`, `chore`, `ci`, `docs`, `feat`, `fix`, `perf`, `refactor`, `revert`, `style`, and `test`. Add `!` or a `BREAKING CHANGE:` trailer only for an intentional incompatible change. GitHub Actions validates pull-request titles; the custom release workflow continues to use the component versions and tags in `release/versions.json`.

## Documentation style

Document what readers need to complete a task: prerequisites, actions, expected results, and recovery from likely failures.

- Give each topic one home. Link to it instead of repeating it.
- Keep implementation detail in the protocol, source comments, or Go API docs unless it changes a reader's decision.
- Use short, direct technical English and consistent terms. Cut details that do not help the task.
- Keep optional complete examples separate from the main instructions.
- Update relevant documentation checks when a public contract changes.
- Keep historical investigations separate from current instructions.
- Do not hard-wrap Markdown paragraphs or list items.

Use ASD-STE100-inspired clarity without claiming formal compliance.

## Project colors

Use the existing project palette for all project-authored UI, diagrams, artwork, and previews. Preserve its dark colors and semantic roles: teal for signature, emerald for success, amber for waiting or warning, and coral for danger. Color must accompany a label, icon, or other visible cue.

Calendar and the Codex mark are the visual exceptions. Preserve Calendar’s current colors, assets, and previews to match the CORE Calendar integration. Preserve the Codex mark’s original colors in its SVG, packaged PNGs, and previews; the surrounding Codex UI uses the project palette. Protocol test values, capture synchronization markers, and image encoding palettes are technical data rather than visual identity.

## Licensing

By submitting a contribution, you confirm that you can license it to this project. Contributions use GPL-3.0-or-later unless a file states otherwise.
