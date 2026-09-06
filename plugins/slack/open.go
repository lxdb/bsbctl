package slack

import (
	"context"
	"errors"
	"net/url"
	"os/exec"
	"runtime"
)

var errOpen = errors.New("Slack native open failed")

func nativeTarget(team string, item activity) (string, error) {
	if !validID(team, "T") || !validID(item.ChannelID, "CDG") || !timestampPattern.MatchString(item.MessageTS) {
		return "", errStaleActivity
	}
	return "slack://channel?" + url.Values{"team": {team}, "id": {item.ChannelID}}.Encode(), nil
}

// openNative invokes exactly one platform handler, with no shell or fallback.
func openNative(ctx context.Context, target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "/usr/bin/open", target)
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", target)
	default:
		return errOpen
	}
	if cmd.Run() != nil {
		return errOpen
	}
	return nil
}
