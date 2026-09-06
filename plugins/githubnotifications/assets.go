package githubnotifications

import "github.com/lxdb/bsbctl/internal/assets"

// AssetDeclarations describes the official GitHub mark packaged with the plugin.
func AssetDeclarations() []assets.Declaration {
	return []assets.Declaration{{Source: "assets/github-mark.png", SHA256: "1f7ef0c666e09d5792c9e81b4a042f46c270223e2dc72521e6a0ff86e0151f66", Size: 397, MediaType: "image/png"}}
}
