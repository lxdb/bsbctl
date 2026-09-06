package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/lxdb/bsbctl/plugins/githubnotifications"
	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
)

var version = githubnotifications.PluginVersion

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := pluginsdk.Run(ctx, githubnotifications.DefinitionForVersion(version)); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
