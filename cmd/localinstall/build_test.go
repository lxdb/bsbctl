package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/lxdb/bsbctl/internal/assets"
	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/configschema"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
)

func TestLocalBuildRegistersExecutableSchemaAndAssetsFromCheckout(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and executes checkout binaries")
	}
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
	stage := t.TempDir()
	descriptor, _ := firstpartyplugins.LookupAppID("codex-quota")
	document, version, err := buildLocalPackages(t.Context(), root, stage, []firstpartyplugins.Descriptor{descriptor}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	plugin := document.Plugins["dev.bsbctl.codex-quota"]
	if version == "" || len(document.Plugins) != 1 || plugin.Executable != filepath.Join(stage, "codex-quota/bsbctl-plugin-codex-quota") || plugin.PackageRoot != filepath.Dir(plugin.Executable) {
		t.Fatalf("wrong local package registration: version=%q, plugin=%#v", version, plugin)
	}
	data, err := os.ReadFile(plugin.Executable)
	if err != nil {
		t.Fatal(err)
	}
	if plugin.SHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) {
		t.Fatal("executable digest does not match built bytes")
	}
	if plugin.ConfigSchema == nil {
		t.Fatal("built package has no schema")
	}
	if _, err := configschema.Load(plugin.PackageRoot, *plugin.ConfigSchema); err != nil {
		t.Fatal(err)
	}
	if len(plugin.Assets) == 0 {
		t.Fatal("Codex Quota package lost its assets")
	}
	if err := document.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestLocalAssetVerificationRejectsChangedOrMissingPackageBytes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "icon.png")
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("good")))
	document := config.Document{Plugins: map[string]config.Plugin{"example": {
		PackageRoot: root, Assets: []assets.Declaration{{Source: "icon.png", SHA256: digest, Size: 4}},
	}}}
	for _, contents := range []string{"bad", "evil", "good"} {
		writeLocalTestFile(t, path, contents, 0o644)
		err := verifyLocalAssets(document)
		if (err == nil) != (contents == "good") {
			t.Fatalf("contents %q: verification error = %v", contents, err)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := verifyLocalAssets(document); err == nil {
		t.Fatal("missing asset accepted")
	}
}
