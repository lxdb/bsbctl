package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallScriptVerifiesAndInstallsLatestRelease(t *testing.T) {
	fixture := newInstallScriptFixture(t, true)

	command := exec.CommandContext(t.Context(), "sh", fixture.script, "--apps", "none", "--no-path-update")
	command.Env = fixture.environment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh: %v: %s", err, output)
	}

	installed := filepath.Join(fixture.home, ".local", "bin", "bsbctl")
	info, err := os.Stat(installed)
	if err != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("installed binary is not executable: info=%v err=%v", info, err)
	}
	setupArgs, err := os.ReadFile(filepath.Join(fixture.home, "setup.args"))
	if err != nil {
		t.Fatal(err)
	}
	if string(setupArgs) != "setup --apps none\n" {
		t.Fatalf("setup arguments = %q", setupArgs)
	}
}

func TestInstallScriptLocalUsesCheckoutWithoutReleaseDownloads(t *testing.T) {
	fixture := newInstallScriptFixture(t, true)
	writeExecutable(t, filepath.Join(fixture.tools, "curl"), "#!/bin/sh\nexit 99\n")
	writeExecutable(t, filepath.Join(fixture.tools, "go"), "#!/bin/sh\ntest -f go.mod && test -f cmd/localinstall/main.go || exit 98\nprintf '%s\\n' \"$@\"\n")
	command := exec.CommandContext(t.Context(), "sh", fixture.script, "--local", "--apps", "codex,mac-resources", "--no-path-update")
	command.Dir = t.TempDir()
	command.Env = fixture.environment
	output, err := command.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "run\n./cmd/localinstall\n--apps\ncodex,mac-resources\n") {
		t.Fatalf("local install did not dispatch to checkout builder: %v: %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(fixture.home, ".local", "bin", "bsbctl")); !os.IsNotExist(err) {
		t.Fatalf("release installer ran during local dispatch: %v", err)
	}
}

func TestInstallScriptDoesNotReplaceBinaryWhenChecksumFails(t *testing.T) {
	fixture := newInstallScriptFixture(t, false)
	installDirectory := filepath.Join(fixture.home, ".local", "bin")
	if err := os.MkdirAll(installDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(installDirectory, "bsbctl")
	if err := os.WriteFile(installed, []byte("existing\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	command := exec.CommandContext(t.Context(), "sh", fixture.script, "--apps", "none", "--no-path-update")
	command.Env = fixture.environment
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("install.sh accepted an invalid checksum: %s", output)
	}
	data, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing\n" {
		t.Fatalf("existing binary was replaced: %q", data)
	}
}

func TestInstallScriptUsesControllingTerminalWhenStandardOutputIsRedirected(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the supported installer runtime and pseudo-terminal command are macOS-specific")
	}
	fixture := newInstallScriptFixture(t, true)
	redirectedOutput := filepath.Join(t.TempDir(), "installer.out")
	command := exec.CommandContext(t.Context(), "/usr/bin/script", "-q", "/dev/null", "sh", "-c",
		`printf '' | sh "$BSBCTL_TEST_SCRIPT" --no-path-update >"$BSBCTL_TEST_OUTPUT"`)
	command.Env = append(fixture.environment,
		"BSBCTL_TEST_SCRIPT="+fixture.script,
		"BSBCTL_TEST_OUTPUT="+redirectedOutput,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("install.sh under pseudo-terminal: %v: %s", err, output)
	}
	stdinKind, err := os.ReadFile(filepath.Join(fixture.home, "setup.stdin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stdinKind) != "tty\n" {
		t.Fatalf("setup stdin = %q, want controlling terminal", stdinKind)
	}
}

type installScriptFixture struct {
	script      string
	home        string
	tools       string
	environment []string
}

func newInstallScriptFixture(t *testing.T, validChecksum bool) installScriptFixture {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	temporary := t.TempDir()
	home := filepath.Join(temporary, "home")
	tools := filepath.Join(temporary, "tools")
	fixture := filepath.Join(temporary, "fixture")
	for _, directory := range []string{home, tools, fixture} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	archiveName := "bsbctl_1.2.3_darwin_arm64.tar.gz"
	archivePath := filepath.Join(fixture, archiveName)
	writeInstallArchive(t, archivePath)
	archiveData, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(archiveData)
	checksum := hex.EncodeToString(digest[:])
	if !validChecksum {
		checksum = strings.Repeat("0", sha256.Size*2)
	}
	if err := os.WriteFile(filepath.Join(fixture, "SHA256SUMS-darwin-arm64"), []byte(checksum+"  "+archiveName+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	writeExecutable(t, filepath.Join(tools, "uname"), "#!/bin/sh\ncase \"$1\" in -s) echo Darwin;; -m) echo arm64;; *) exit 1;; esac\n")
	writeExecutable(t, filepath.Join(tools, "curl"), `#!/bin/sh
output=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) output=$2; shift 2 ;;
    -w) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
case "$url" in
  */releases/latest) printf '%s' 'https://github.com/lxdb/bsbctl/releases/tag/v1.2.3' ;;
  */bsbctl_1.2.3_darwin_arm64.tar.gz) cp "$BSBCTL_TEST_FIXTURE/bsbctl_1.2.3_darwin_arm64.tar.gz" "$output" ;;
  */SHA256SUMS-darwin-arm64) cp "$BSBCTL_TEST_FIXTURE/SHA256SUMS-darwin-arm64" "$output" ;;
  *) exit 22 ;;
esac
`)

	return installScriptFixture{
		script: filepath.Join(root, "install.sh"),
		home:   home,
		tools:  tools,
		environment: append(os.Environ(),
			"HOME="+home,
			"PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"),
			"BSBCTL_TEST_FIXTURE="+fixture,
		),
	}
}

func writeInstallArchive(t *testing.T, path string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	binary := []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$HOME/setup.args\"\nif [ -t 0 ]; then printf 'tty\\n'; else printf 'notty\\n'; fi > \"$HOME/setup.stdin\"\n")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "bsbctl", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}
