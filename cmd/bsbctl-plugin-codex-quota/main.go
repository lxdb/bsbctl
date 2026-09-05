package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/lxdb/bsbctl/plugins/codexquota"
	pluginsdk "github.com/lxdb/bsbctl/sdk/plugin"
)

var version = codexquota.PluginVersion

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := pluginsdk.Run(ctx, codexquota.DefinitionForVersion(version)); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
