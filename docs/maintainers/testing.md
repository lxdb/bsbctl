# Testing

Run the check that covers the changed behavior. For shared code, also run the broader deterministic suite.

| Change | Start with |
| --- | --- |
| Documentation | `./scripts/verify.sh docs` |
| One Go package | `go test ./path/to/package` |
| Public SDK or protocol | SDK/protocol tests and the external example |
| Shell script | Its targeted tests and `shellcheck` |
| Release inputs or packaging | `./scripts/verify.sh release` on macOS |
| Device presentation | Scene tests and physical BUSY Bar verification |

## Shared changes

```sh
./scripts/verify.sh quick
./scripts/verify.sh all
```

`quick` runs formatting, tests, preview tests, and vet. `all` adds module, race, coverage, dead-code, documentation, and repository checks. Neither contacts a physical device.

Before release, also run security, fuzz, randomized depth, and artifact verification as described in [Release](../release.md#verify-the-source-and-artifacts). Run `./scripts/verify.sh` without arguments to see individual phases.

## Documentation checks

The docs phase validates local links and anchors, app IDs and operations, configuration fields and defaults, CLI help parity, referenced SDK APIs, protocol methods, and package contents. It decodes the complete app examples and compiles the external plugin example.

It does not judge whether prose is useful. Review the reader's task separately: prerequisites, commands, expected result, failure recovery, and the next relevant link.

## Report verification accurately

Name the check, result, and environment. Compilation does not prove execution; package verification does not prove catalog trust; a loopback test does not prove hardware behavior. Report skipped checks and the behavior that remains unverified.
