package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/lxdb/bsbctl/plugins/codex"
	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
)

var version = codex.PluginVersion

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := pluginsdk.Run(ctx, codex.DefinitionForVersion(version)); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
