# Protobuf dependency and bsbctl release licensing

This note defines the redistribution terms for the BUSY Bar protobuf inputs used by `busylib-go v0.3.1`. It is an engineering compliance record based on primary sources, not legal advice.

## Decision

The selected BUSY Bar protobuf inputs have compatible public redistribution terms. The BUSY Bar protobuf project added an MIT license in commit [`376ecf7a4bbef7d68451a479398673b0bcc0bfca`](https://github.com/busy-app/busybar-protobuf/commit/376ecf7a4bbef7d68451a479398673b0bcc0bfca).

Published [`busylib-go v0.3.1`](https://github.com/lxdb/busylib-go/releases/tag/v0.3.1) preserves that license as `LICENSES/busybar-protobuf-MIT.txt` and records the licensed source revision and schema digests. `bsbctl` selects that immutable module version, links the required notices from `THIRD_PARTY_NOTICES.md`, and preserves complete terms under `LICENSES/`.

## Evidence

| Evidence | Verified property | Release consequence |
| --- | --- | --- |
| [`busy-app/busybar-protobuf` license](https://github.com/busy-app/busybar-protobuf/blob/376ecf7a4bbef7d68451a479398673b0bcc0bfca/LICENSE.md) | Copyright 2026 BUSY App; MIT License | The selected protobuf inputs have explicit redistribution terms. |
| [Upstream license commit](https://github.com/busy-app/busybar-protobuf/commit/376ecf7a4bbef7d68451a479398673b0bcc0bfca) | Adds `LICENSE.md` at revision `376ecf7a4bbef7d68451a479398673b0bcc0bfca` | Provides the immutable source boundary used by `busylib-go`. |
| [`busylib-go v0.3.1`](https://github.com/lxdb/busylib-go/releases/tag/v0.3.1) | Published module version | Supplies the selected immutable dependency boundary. |
| [`busylib-go` preserved BUSY license](https://github.com/lxdb/busylib-go/blob/v0.3.1/LICENSES/busybar-protobuf-MIT.txt) | Exact upstream MIT text is retained in the released module | Supplies the notice required for the generated packages. |
| [`busylib-go` source record](https://github.com/lxdb/busylib-go/blob/v0.3.1/scripts/protobuf-source.env) | Records upstream revision `376ecf7a4bbef7d68451a479398673b0bcc0bfca` | Connects the generated package inputs to the licensed revision. |
| [`protobuf-go v1.36.12` license](https://github.com/protocolbuffers/protobuf-go/blob/v1.36.12/LICENSE) | BSD-3-Clause | The Google runtime remains separately redistributable when its notice is reproduced. |

The core [Protocol Buffers license](https://github.com/protocolbuffers/protobuf/blob/main/LICENSE) distinguishes generated code from the support library: rights in generated code follow the input owner, while Google's runtime remains under Google's terms. The upstream BUSY App MIT license supplies the required permission for these inputs.

## Release contract

The release path must keep all of the following conditions true:

1. `go.mod` selects `github.com/lxdb/busylib-go v0.3.1` without a committed filesystem replacement.
2. Release verification runs with `GOWORK=off` so a developer's local workspace cannot mask the committed dependency graph.
3. `THIRD_PARTY_NOTICES.md` identifies `busylib-go`, the BUSY Bar protobuf inputs, and `google.golang.org/protobuf`, then links their complete terms under `LICENSES/`.
4. `release/dependencies.json` records `busylib-go v0.3.1` and the verified root-license digest.
5. A source revision, generated-package replacement, or module-version change requires fresh evidence. This decision applies only to the selected v0.3.1 release boundary.

Developers may use an ignored local `go.work` file to test a sibling `busylib-go` checkout. That local convenience is not part of the committed or published dependency contract.

## Decision boundary

This decision applies only to `busylib-go v0.3.1` and its recorded protobuf source revision. Re-evaluate the redistribution terms when the selected dependency or preserved evidence changes. `releasectl preflight` evaluates the release inputs independently of this licensing decision.
