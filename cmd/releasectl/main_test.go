package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(args []string, stdout, stderr io.Writer) int {
	return runWithInput(context.Background(), args, strings.NewReader(""), stdout, stderr)
}

func runWithContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runWithInput(ctx, args, strings.NewReader(""), stdout, stderr)
}

func TestRunPreflightReturnsEveryBlockerAndAction(t *testing.T) {
	t.Parallel()
	root := releaseRoot(t, true, false, true)
	var stdout, stderr bytes.Buffer
	code := run([]string{"preflight", "--root", root}, &stdout, &stderr)
	if code != exitBlocked {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitBlocked, stderr.String())
	}
	want := strings.Join([]string{
		"BLOCKER busylib-protobuf-license: unresolved license",
		"ACTION busylib-protobuf-license: obtain permission",
		"BLOCKER busylib-local-replace: go.mod replaces github.com/lxdb/busylib-go with a local filesystem path",
		"ACTION busylib-local-replace: publish an immutable reachable busylib-go version and remove the local replacement",
		"BLOCKER catalog-public-key: no authorized Ed25519 catalog public key is provisioned",
		"ACTION catalog-public-key: add the authorized public key to internal/releasekeys/catalog_public_keys.json and review its provenance",
		"release preflight: blocked by 3 finding(s)",
		"",
	}, "\n")
	if stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("stdout=%q want=%q stderr=%q", stdout.String(), want, stderr.String())
	}
}

func TestRunPreflightReportsReadyOnlyWithResolvedInputs(t *testing.T) {
	t.Parallel()
	root := releaseRoot(t, false, true, false)
	var stdout, stderr bytes.Buffer
	code := run([]string{"preflight", "--root", root}, &stdout, &stderr)
	if code != exitSuccess || stdout.String() != "release preflight: ready\n" || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunRejectsInvalidCommandsAndMachineInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{name: "missing command", args: nil},
		{name: "unknown command", args: []string{"publish"}},
		{name: "unknown flag", args: []string{"preflight", "--network"}},
		{name: "positional", args: []string{"preflight", "extra"}},
		{name: "missing root", args: []string{"preflight", "--root", filepath.Join(t.TempDir(), "missing")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := run(test.args, &stdout, &stderr); code != exitFailure || stdout.Len() != 0 || strings.TrimSpace(stderr.String()) == "" {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunHelpAliasesDocumentCommandsAndExitCodes(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"help", "--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if code := run([]string{arg}, &stdout, &stderr); code != exitSuccess || stderr.Len() != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			for _, text := range []string{"usage: releasectl <command>", "Commands:\n", "preflight", "publish-releases", "Exit codes:\n", "  0  Success", "  2  Invalid input or operation failure"} {
				if !strings.Contains(stdout.String(), text) {
					t.Errorf("help does not contain %q\n%s", text, stdout.String())
				}
			}
		})
	}
}

func releaseRoot(t *testing.T, legalBlocker, keyProvisioned, localReplace bool) string {
	t.Helper()
	root := t.TempDir()
	goMod := "module github.com/lxdb/bsbctl\n\ngo 1.26\n\nrequire example.com/reviewed v1.0.0\nreplace example.com/reviewed => ./reviewed\n"
	imports := "_ \"example.com/reviewed/dependency\""
	keys := `{"schema_version":1,"keys":[]}`
	if keyProvisioned {
		key := base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))
		keys = `{"schema_version":1,"keys":[{"id":"stable","algorithm":"ed25519","public_key_base64":"` + key + `"}]}`
	}
	blockers := `{"schema_version":1,"blockers":[]}`
	dependencies := `{"schema_version":1,"modules":[{"module":"example.com/reviewed","version":"v1.0.0","license":"MIT","license_sha256":"b7ec5c1207ecb19fc7d4ed4436fe50caed09068f9fbd68bbe86f1065811c05be"}]}`
	if legalBlocker {
		goMod += "require github.com/lxdb/busylib-go v0.0.0\nreplace github.com/lxdb/busylib-go => ../busylib/busylib-go\n"
		imports += "\n_ \"github.com/lxdb/busylib-go/dependency\""
		blockers = `{"schema_version":1,"blockers":[{"id":"busylib-protobuf-license","message":"unresolved license","operator_action":"obtain permission"}]}`
		dependencies = `{"schema_version":1,"modules":[{"module":"example.com/reviewed","version":"v1.0.0","license":"MIT","license_sha256":"b7ec5c1207ecb19fc7d4ed4436fe50caed09068f9fbd68bbe86f1065811c05be"},{"module":"github.com/lxdb/busylib-go","version":"v0.0.0","license":"MIT","license_sha256":"b7ec5c1207ecb19fc7d4ed4436fe50caed09068f9fbd68bbe86f1065811c05be","release_blocked":true,"release_blocker_id":"busylib-protobuf-license"}]}`
	} else if localReplace {
		goMod += "require github.com/lxdb/busylib-go v0.0.0\nreplace github.com/lxdb/busylib-go => ../busylib/busylib-go\n"
	}
	writeReleaseFile(t, filepath.Join(root, "go.mod"), goMod)
	writeReleaseFile(t, filepath.Join(root, "reviewed", "go.mod"), "module example.com/reviewed\n\ngo 1.26\n")
	writeReleaseFile(t, filepath.Join(root, "reviewed", "dependency", "dependency.go"), "package dependency\n")
	if legalBlocker || localReplace {
		busylibRoot := filepath.Clean(filepath.Join(root, "..", "busylib", "busylib-go"))
		writeReleaseFile(t, filepath.Join(busylibRoot, "go.mod"), "module github.com/lxdb/busylib-go\n\ngo 1.26\n")
		writeReleaseFile(t, filepath.Join(busylibRoot, "dependency", "dependency.go"), "package dependency\n")
	}
	for _, packagePath := range []string{"cmd/bsbctl", "cmd/bsbctl-plugin-calendar", "cmd/bsbctl-plugin-codex", "cmd/bsbctl-plugin-codex-quota", "cmd/bsbctl-plugin-mac-resources"} {
		writeReleaseFile(t, filepath.Join(root, packagePath, "main.go"), "package main\n\nimport (\n"+imports+"\n)\n\nfunc main() {}\n")
	}
	writeReleaseFile(t, filepath.Join(root, "internal", "releasekeys", "catalog_public_keys.json"), keys)
	writeReleaseFile(t, filepath.Join(root, "release", "blockers.json"), blockers)
	writeReleaseFile(t, filepath.Join(root, "release", "dependencies.json"), dependencies)
	writeReleaseFile(t, filepath.Join(root, "release", "versions.json"), `{"schema_version":1,"components":[{"id":"bsbctl","kind":"core","version":"0.1.0","tag":"v0.1.0","package":"./cmd/bsbctl","binary":"bsbctl"},{"id":"dev.bsbctl.calendar","kind":"plugin","version":"0.1.0","tag":"plugin/calendar/v0.1.0","package":"./cmd/bsbctl-plugin-calendar","binary":"bsbctl-plugin-calendar"},{"id":"dev.bsbctl.codex","kind":"plugin","version":"0.1.0","tag":"plugin/codex/v0.1.0","package":"./cmd/bsbctl-plugin-codex","binary":"bsbctl-plugin-codex"},{"id":"dev.bsbctl.codex-quota","kind":"plugin","version":"0.1.0","tag":"plugin/codex-quota/v0.1.0","package":"./cmd/bsbctl-plugin-codex-quota","binary":"bsbctl-plugin-codex-quota"},{"id":"dev.bsbctl.mac-resources","kind":"plugin","version":"0.1.0","tag":"plugin/mac-resources/v0.1.0","package":"./cmd/bsbctl-plugin-mac-resources","binary":"bsbctl-plugin-mac-resources"}]}`)
	return root
}

func writeReleaseFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
