# Release

Release from a reviewed commit on macOS with Go 1.26.6 and `/usr/bin/codesign`. The protected workflow builds Darwin arm64 and amd64 on `macos-15`. Local checks do not publish anything.

## Verify the source and artifacts

```sh
./scripts/verify.sh security
./scripts/verify.sh fuzz
./scripts/verify.sh depth
./scripts/verify.sh release
```

The release phase runs all deterministic source checks, preflight, and repeated artifact builds for both architectures. Security, fuzz, and randomized depth are separate required checks. Review the complete output and resolve failures before publication.

For a quick check of release inputs, run `./scripts/verify.sh preflight`. Exit `0` means the inputs passed; exit `1` means findings remain. Use `go run ./cmd/releasectl help` for individual packaging, inspection, and catalog commands.

Release commands set `GOWORK=off` and verify the committed dependency graph. A sibling checkout cannot replace a release dependency. See [dependency metadata](../release/dependencies.json) and [licensing evidence](research/protobuf-release-licensing.md).

## Prepare versions and trust inputs

| Input | Requirement |
| --- | --- |
| [`release/versions.json`](../release/versions.json) | Canonical component versions. Core tags use `vX.Y.Z`; plugin tags use `plugin/<name>/vX.Y.Z`. |
| [`release/catalog-predecessor.json`](../release/catalog-predecessor.json) | First release uses `first_release` and sequence `1`. Later releases record the prior core tag and SHA-256 values of its published catalog and signature. |
| [`internal/releasekeys/catalog_public_keys.json`](../internal/releasekeys/catalog_public_keys.json) | Production Ed25519 verification keys, including `stable-2026`. |
| GitHub `release` environment | Required approval and `CATALOG_SIGNING_PRIVATE_KEY_B64` secret. |

The secret must contain canonical base64 for the raw 64-byte Ed25519 private key. The signing job reads it through standard input and requires its public key to match the selected tracked key ID. Never commit the private key or pass it as a command-line argument.

Use a UTC catalog generation time and a strictly increasing sequence. Catalogs do not expire. The installer rejects different catalog bytes at the same or a lower sequence.

## Publish

The [release workflow](../.github/workflows/release.yml) is manually dispatched. Before dispatch, an authorized operator must:

1. Review the passing checks above.
2. Confirm that every component tag exists remotely and points to the exact reviewed commit.
3. Confirm the predecessor record, next catalog sequence, UTC generation time, and signing key ID.
4. Obtain the required `release` environment approval.
5. Dispatch with `catalog_sequence`, `catalog_generated_at`, and `catalog_key_id`.

Repository administrators must protect release tag creation and updates. The workflow never creates or moves tags. It accepts lightweight or annotated tags and does not verify GPG or SSH tag signatures.

Only the publish job has write permission. It uses artifacts from its own workflow run, reinspects both architectures, signs and verifies the catalog, then publishes the four plugin releases before core.

## Verify the published result

Each component archive contains its binary, metadata or manifest, license and notice files, and CycloneDX SBOM. Plugin schemas and assets are authenticated by their manifests. See [package contents](plugin-packaging.md#package-contents).

The core release must contain:

| Asset | Purpose |
| --- | --- |
| `bsbctl_<version>_darwin_arm64.tar.gz` | Apple silicon binary |
| `bsbctl_<version>_darwin_amd64.tar.gz` | Intel binary |
| `SHA256SUMS-darwin-arm64`, `SHA256SUMS-darwin-amd64` | Archive checksums |
| `catalog.json`, `catalog.sig` | Signed plugin inventory |
| `install.sh` | Installer |

Repeated local builds must be byte-identical when Apple and Go toolchains match. Inspection verifies archive bytes and internal timestamps; downloaded files may have different outer modification times.

Binaries are ad-hoc signed, not Developer ID signed or notarized. Artifact checks do not establish physical-device behavior, Gatekeeper acceptance, or GitHub protection settings.

## Recover a partial publication

GitHub cannot publish all five releases atomically. If publication fails, inspect the visible releases and rerun the same workflow with the same authenticated artifact set. It creates only missing drafts, uploads only missing exact assets, and verifies complete drafts before publishing.

Core is published last so its catalog cannot point to plugin releases that are still private. Do not move tags or replace published assets to bypass a mismatch. The installed binary fetches the catalog from its own core tag, not a moving latest URL.

## Record performance evidence

On Darwin, use a new output path outside the repository:

```sh
soak_directory="$(mktemp -d)"
go run ./cmd/releasectl soak --root . --output "$soak_directory/soak.jsonl"
```

The soak uses the production daemon with Mac Resources and Codex Quota against loopback dependencies. After 10 seconds of warmup, it takes 12 samples at five-second intervals. Each must stay at or below 1.0 percent aggregate CPU and 100 MiB aggregate RSS. Invalid or missing telemetry fails the check; cleanup must remove the process tree and control socket.

Keep the JSONL, its SHA-256, source revision, toolchain, host, workload, and binary hashes. This verifies only that loopback run, not physical-device behavior, sleep/wake recovery, provider compatibility, or long-duration stability.
