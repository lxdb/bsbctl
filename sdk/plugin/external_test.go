package plugin_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExternalModuleCanImplementHandlerUsingOnlyPublicSDKPackages(t *testing.T) {
	t.Parallel()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate external fixture test")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", ".."))
	moduleRoot := t.TempDir()
	goMod := "module example.com/external-plugin\n\ngo 1.26\n\nrequire github.com/lxdb/bsbctl v0.0.0\n\nreplace github.com/lxdb/bsbctl => " + repositoryRoot + "\n"
	mainGo := `package main

import (
	"context"

	"github.com/lxdb/bsbctl/sdk/plugin"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

type handler struct{}

func (handler) ReplaceInstances(context.Context, []protocol.Instance) error { return nil }
func (handler) Shutdown(context.Context) error { return nil }

var _ plugin.Plugin = handler{}
var _ plugin.Shutdowner = handler{}

func main() {
	_ = plugin.Definition{
		ID: "example.external", Version: "1",
		Contract: plugin.Contract{
			ExecutionModes: []protocol.ExecutionMode{protocol.ExecutionModeResident},
			Channels: []protocol.Channel{{ID: "main"}},
		},
		New: func(*plugin.Host) plugin.Plugin { return handler{} },
	}
}
`
	if err := os.WriteFile(filepath.Join(moduleRoot, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moduleRoot, "main.go"), []byte(mainGo), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("go", "build", "./...")
	command.Dir = moduleRoot
	command.Env = append(os.Environ(), "GOWORK=off")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("external plugin build failed: %v\n%s", err, output)
	}
	command = exec.Command("go", "list", "-deps", "github.com/lxdb/bsbctl/sdk/plugin")
	command.Dir = moduleRoot
	command.Env = append(os.Environ(), "GOWORK=off")
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("list external plugin dependencies: %v\n%s", err, output)
	}
	for _, dependency := range strings.Fields(string(output)) {
		if strings.HasPrefix(dependency, "github.com/lxdb/bsbctl/internal/") {
			t.Fatalf("public SDK depends on repository-internal package %q", dependency)
		}
	}
}
