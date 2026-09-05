package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVerifyLocalScriptDelegatesToCanonicalReleaseVerification(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	temporary := t.TempDir()
	scriptDirectory := filepath.Join(temporary, "scripts")
	if err := os.Mkdir(scriptDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(temporary, "capture")
	wrapperSource, err := os.ReadFile(filepath.Join(root, "scripts", "verify-local.sh"))
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(scriptDirectory, "verify-local.sh")
	if err := os.WriteFile(wrapper, wrapperSource, 0o755); err != nil {
		t.Fatal(err)
	}
	verify := filepath.Join(scriptDirectory, "verify.sh")
	fake := "#!/bin/sh\n" +
		"printf '%s\\n%s\\n' \"$PWD\" \"$*\" > \"$BSBCTL_VERIFY_CAPTURE\"\n"
	if err := os.WriteFile(verify, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(wrapper)
	command.Dir = temporary
	command.Env = append(os.Environ(),
		"BSBCTL_VERIFY_CAPTURE="+capture,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("verify-local.sh: %v: %s", err, output)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	workingDirectory, err := filepath.EvalSymlinks(temporary)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{workingDirectory, "release", ""}, "\n")
	if string(data) != want {
		t.Fatalf("capture = %q, want %q", data, want)
	}
}

func TestVerifyScriptIgnoresAmbientGoFlags(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	temporary := t.TempDir()
	fakeGo := filepath.Join(temporary, "go")
	fake := `#!/bin/sh
set -eu
test "${GOFLAGS-}" = ""
test "${GOENV-}" = "off"
test "${GOWORK-}" = "off"
test "${GOTOOLCHAIN-}" = "go1.26.6"
printf '%s\n' "$*" > "$BSBCTL_GO_CAPTURE"
`
	if err := os.WriteFile(fakeGo, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(temporary, "capture")
	command := exec.Command(filepath.Join(root, "scripts", "verify.sh"), "test")
	command.Dir = root
	command.Env = append(os.Environ(),
		"PATH="+temporary+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BSBCTL_GO_CAPTURE="+capture,
		"GOENV="+filepath.Join(temporary, "ambient-go-env"),
		"GOFLAGS=-run=^$",
		"GOTOOLCHAIN=local",
		"GOWORK="+filepath.Join(temporary, "ambient.work"),
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("verify.sh test: %v: %s", err, output)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != "test -mod=readonly ./... -p 2 -shuffle=on -count=1" {
		t.Fatalf("go arguments = %q", got)
	}
}

func TestVerifyScriptRejectsUnpinnedShellCheck(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	temporary := t.TempDir()
	fakeGo := filepath.Join(temporary, "go")
	if err := os.WriteFile(fakeGo, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeShellCheck := filepath.Join(temporary, "shellcheck")
	shellcheck := `#!/bin/sh
if test "${1-}" = "--version"; then
  printf 'ShellCheck - shell script analysis tool\nversion: 0.10.0\n'
fi
exit 0
`
	if err := os.WriteFile(fakeShellCheck, []byte(shellcheck), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(root, "scripts", "verify.sh"), "repository")
	command.Dir = root
	command.Env = append(os.Environ(), "PATH="+temporary+string(os.PathListSeparator)+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "ShellCheck 0.11.0 is required") {
		t.Fatalf("verify.sh repository err=%v output=%q", err, output)
	}
}
