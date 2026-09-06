package slack

import "github.com/lxdb/bsbctl/internal/assets"

// AssetDeclarations describes the official Slack mark packaged with this plugin.
func AssetDeclarations() []assets.Declaration {
	return []assets.Declaration{{Source: "assets/slack-mark.png", SHA256: "dfafa32140dde35ac98f614fe0f1ca432be53fffb433626ad8e65dc7068fe813", Size: 850, MediaType: "image/png"}}
}
