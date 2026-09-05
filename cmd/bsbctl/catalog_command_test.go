package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/lxdb/bsbctl/internal/control"
	"github.com/lxdb/bsbctl/internal/installer"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogCommandExitMatrix(t *testing.T) {
	directory := t.TempDir()
	catalogPath := filepath.Join(directory, "catalog.json")
	signaturePath := filepath.Join(directory, "catalog.sig")
	if err := os.WriteFile(catalogPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signaturePath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	responseCode := installer.Code("")
	client := &fakeCLIClient{call: func(_ context.Context, method string, params, result any) error {
		switch method {
		case "plugin.install", "plugin.update", "plugin.rollback":
			*(result.(*control.CatalogOperationResponse)) = control.CatalogOperationResponse{ErrorCode: responseCode}
		case "plugin.status":
			*(result.(*installer.Snapshot)) = installer.Snapshot{CatalogSequence: 9, Plugins: []installer.PluginSnapshot{}}
		default:
			return errors.New("unexpected method")
		}
		return nil
	}}
	restore := installCLIClient(t, client)
	defer restore()
	base := []string{"plugin", "--catalog", catalogPath, "--signature", signaturePath, "--version", "1"}
	tests := []struct {
		name     string
		args     []string
		code     installer.Code
		wantExit int
	}{
		{name: "conflict", args: append([]string{"plugin", "install"}, base...), code: installer.CodeInstallConflict, wantExit: exitRejected},
		{name: "not installed", args: []string{"plugin", "rollback", "plugin"}, code: installer.CodeNotInstalled, wantExit: exitRejected},
		{name: "recovery", args: append([]string{"plugin", "update"}, base...), code: installer.CodeRecoveryRequired, wantExit: exitPartial},
		{name: "dependency", args: append([]string{"plugin", "install"}, base...), code: control.CatalogDependencyFailed, wantExit: exitOperational},
		{name: "status", args: []string{"plugin", "status", "plugin"}, wantExit: exitSuccess},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			responseCode = test.code
			var stdout, stderr bytes.Buffer
			code := execute(context.Background(), test.args, strings.NewReader(""), &stdout, &stderr)
			if code != test.wantExit {
				t.Fatalf("exit = %d, want %d; stdout=%q stderr=%q", code, test.wantExit, stdout.String(), stderr.String())
			}
			if test.wantExit == 0 && (!json.Valid(stdout.Bytes()) || stderr.Len() != 0) {
				t.Fatalf("success streams stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if test.wantExit != 0 && (stdout.Len() != 0 || stderr.Len() == 0) {
				t.Fatalf("failure streams stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestCatalogDurabilityUncertainPromotionReturnsRedactedResult(t *testing.T) {
	var stdout bytes.Buffer
	err := finishCatalogOperation(&stdout, control.CatalogOperationResponse{
		Result: installer.Result{
			Release:   installer.ReleaseRef{ID: "plugin", Version: "1", OS: "darwin", Arch: "arm64"},
			Promotion: installer.PromotionInstalledDurabilityUncertain,
		},
		ErrorCode: installer.CodeStateFailed,
	})
	code, _ := classifyCommandError(err)
	if code != exitPartial {
		t.Fatalf("exit = %d, want %d", code, exitPartial)
	}
	if !json.Valid(stdout.Bytes()) || !strings.Contains(stdout.String(), `"promotion":"installed_durability_uncertain"`) {
		t.Fatalf("stdout = %q, want redacted installer result", stdout.String())
	}
	if strings.Contains(stdout.String(), "/") || strings.Contains(stdout.String(), "token") || strings.Contains(stdout.String(), "url") {
		t.Fatalf("unsafe stdout = %q", stdout.String())
	}
}

func TestCatalogMutationRejectsMalformedSuccessResponseWithoutOutput(t *testing.T) {
	tests := []control.CatalogOperationResponse{
		{},
		{Result: installer.Result{Status: "unknown", Release: installer.ReleaseRef{ID: "plugin", Version: "1", OS: "darwin", Arch: "arm64"}}},
		{Result: installer.Result{Status: installer.StatusInstalled, Release: installer.ReleaseRef{ID: "plugin", Version: "1", OS: "darwin"}}},
	}
	for index, response := range tests {
		var stdout bytes.Buffer
		err := finishCatalogOperation(&stdout, response)
		code, _ := classifyCommandError(err)
		if code != exitOperational || stdout.Len() != 0 {
			t.Fatalf("response %d = exit %d stdout %q, want operational/empty", index, code, stdout.String())
		}
	}
}
