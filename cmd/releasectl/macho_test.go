package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateCodesignToolRejectsMissingExecutable(t *testing.T) {
	previous := codesignExecutable
	codesignExecutable = filepath.Join(t.TempDir(), "missing-codesign")
	t.Cleanup(func() { codesignExecutable = previous })
	if err := validateCodesignTool(); !errors.Is(err, errCodesignUnavailable) {
		t.Fatalf("validateCodesignTool error = %v, want unavailable", err)
	}
}

func TestCodesignFailureDiagnosticIsBounded(t *testing.T) {
	tool := filepath.Join(t.TempDir(), "codesign")
	script := "#!/bin/sh\nprintf '%05000d' 0 >&2\nexit 1\n"
	if err := os.WriteFile(tool, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := codesignExecutable
	codesignExecutable = tool
	t.Cleanup(func() { codesignExecutable = previous })
	err := adHocSignDarwinComponent(t.Context(), filepath.Join(t.TempDir(), "component"))
	if err == nil || !strings.Contains(err.Error(), "ad-hoc sign") || len(err.Error()) > 4200 {
		t.Fatalf("codesign diagnostic = %v", err)
	}
}
