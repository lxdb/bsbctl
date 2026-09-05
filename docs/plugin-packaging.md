# Plugin packaging and installation

Production installation requires an authorized signed catalog. bsbctl supervises plugins but does not sandbox hostile code. The repository packager builds only reviewed first-party components listed in `release/versions.json`.

## Package contents

```text
plugin archive
|-- executable
|-- manifest.json
|-- config.schema.json        optional, when declared
|-- assets/                   optional, when declared
|-- LICENSE
|-- NOTICE
|-- THIRD_PARTY_NOTICES.md
|-- LICENSES/
`-- sbom.cdx.json
```

The manifest must match the executable's identity, protocol, modes, channels, and operations. It authenticates the executable, optional configuration schema, and assets by size and SHA-256. Other regular files must be declared or be one of the permitted documentation files shown above.

Documentation files are authenticated by the archive digest, must be between 1 byte and 1 MiB, and cannot be executable entrypoints or device assets. License files are direct `LICENSES/*.txt` children.

| Value | Must match |
| --- | --- |
| SDK definition version | Package manifest version |
| Catalog entry version | Package manifest version |
| Protocol version | Exact string `1.0` |

## Verify a package

Before publication, run:

```sh
bsbctl plugin verify \
  --manifest /path/to/manifest.json \
  --fixture /path/to/conformance.json
```

Use `--executable PATH` if the executable is not next to the manifest. The verifier checks package metadata and files, initialization, instance replacement, declared operations, health, cancellation of a pre-canceled call, and shutdown.

This checks the fixture's package contract. It does not install the package, contact BUSY Bar, verify catalog trust, or prove cancellation of provider work already in flight.

## Install from a catalog

Use setup for normal [first-party installation](apps.md). For an authorized catalog, run:

```sh
bsbctl plugin install <plugin-id> --catalog catalog.json --signature catalog.sig --version <version>
bsbctl plugin update <plugin-id> --catalog catalog.json --signature catalog.sig --version <version>
bsbctl plugin status <plugin-id>
bsbctl plugin rollback <plugin-id>
```

Installation verifies the catalog signature and sequence, archive, manifest, assets, and live handshake before activation. Updates and rollback preserve app configuration. Rollback selects a retained verified version.

A failure before commit leaves the active version unchanged. If a command reports uncertain durability or exit code `6`, inspect plugin status before another change. See [partial-result recovery](reference/errors.md#partial-and-uncertain-results).

Use [Release](release.md) for first-party tags, artifacts, catalog signing, and publication.
