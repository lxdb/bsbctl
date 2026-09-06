# Third-party notices

`bsbctl` distributes the following third-party material. `go.mod` and `go.sum` own the selected dependency versions. `release/dependencies.json` is the machine-readable dependency and license inventory used by release verification and SBOM generation.

| Material | License | Complete terms |
| --- | --- | --- |
| `github.com/coder/websocket` | ISC | [`LICENSES/coder-websocket-ISC.txt`](LICENSES/coder-websocket-ISC.txt) |
| `github.com/lxdb/busylib-go` | MIT | [`LICENSES/busylib-go-MIT.txt`](LICENSES/busylib-go-MIT.txt) |
| `github.com/santhosh-tekuri/jsonschema/v6` | Apache-2.0 | [`LICENSES/jsonschema-Apache-2.0.txt`](LICENSES/jsonschema-Apache-2.0.txt) |
| `golang.org/x/sys` | BSD-3-Clause | [`LICENSES/x-sys-BSD-3-Clause.txt`](LICENSES/x-sys-BSD-3-Clause.txt) |
| `golang.org/x/text` | BSD-3-Clause | [`LICENSES/x-text-BSD-3-Clause.txt`](LICENSES/x-text-BSD-3-Clause.txt) |
| `google.golang.org/protobuf` | BSD-3-Clause | [`LICENSES/protobuf-BSD-3-Clause.txt`](LICENSES/protobuf-BSD-3-Clause.txt) |
| BUSY Bar protobuf inputs used by `busylib-go` | MIT | [`LICENSES/busybar-protobuf-MIT.txt`](LICENSES/busybar-protobuf-MIT.txt) |
| BUSY Bar firmware frame used by the preview generator | GPL-2.0-or-later | [`LICENSES/busybar-firmware-GPL-2.0-or-later.txt`](LICENSES/busybar-firmware-GPL-2.0-or-later.txt) |
| LobeHub `lobe-icons` Codex artwork | MIT; OpenAI trademark terms also apply | [`LICENSES/lobehub-icons-MIT.txt`](LICENSES/lobehub-icons-MIT.txt) |

Release archives include this notice and the complete `LICENSES/` directory. The development-only preview generator uses the BUSY Bar firmware frame, and generated files under `docs/previews/` incorporate that frame. The bundled transparent Codex artwork is derived from LobeHub commit `f07e9be35aef452ce735f95ea8204a14ecc513f7`; the [derived SVG](third_party/lobehub/codex-color-transparent.svg) records the removal of the white background path. OpenAI owns the Codex trademark and may impose separate brand-use conditions; neither the MIT license nor the bsbctl license grants trademark rights.

## GitHub identity asset

`plugins/githubnotifications/assets/github-mark.png` is a 16x16 raster canvas with the official white GitHub Invertocat supplied in the local GitHub logo package centered at 14x14. The mark retains its original proportions and colors. GitHub owns its trademarks; this asset identifies the integration and does not imply endorsement. It is not covered by this repository's code license.

## Slack mark

`plugins/slack/assets/slack-mark.png` is a 16-pixel raster of the official Slack mark supplied for this project. Slack and the Slack logo are trademarks of Slack Technologies, LLC. The mark identifies the Slack integration and does not imply endorsement.
