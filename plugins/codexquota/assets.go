package codexquota

import "github.com/lxdb/bsbctl/internal/assets"

const (
	codexMarkSource = "assets/codex-mark.png"
	codexMarkSHA256 = "965cce3d5fe4ea1f11a507f2ea5e1474b11894239b88e48268a223be780f708c"
	codexMarkSize   = int64(423)
)

// AssetDeclarations returns the immutable static asset owned by the Codex
// quota plugin.
func AssetDeclarations() []assets.Declaration {
	return []assets.Declaration{{
		Source: codexMarkSource,
		SHA256: codexMarkSHA256, Size: codexMarkSize, MediaType: "image/png",
	}}
}
