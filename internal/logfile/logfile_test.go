package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenCreatesPrivateDirectoryAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "daemon.jsonl")
	writer, err := Open(path, 1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("record\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	directoryInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("modes directory=%#o file=%#o", directoryInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
}

func TestOpenPreservesExistingParentPermissions(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(filepath.Join(parent, "daemon.jsonl"), 1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("existing parent mode = %#o, want %#o", info.Mode().Perm(), 0o755)
	}
}

func TestOpenRejectsSymlinkWithoutMutatingTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "daemon.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(root, "daemon.jsonl"), 1024, 1); err == nil {
		t.Fatal("Open accepted a symlinked active log")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "keep" || info.Mode().Perm() != 0o644 {
		t.Fatalf("target = %q mode %#o, want unchanged", data, info.Mode().Perm())
	}
}

func TestWriterRotatesWithinConfiguredArchiveBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.jsonl")
	writer, err := Open(path, 11, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"one\n", "two\n", "three\n", "four\n", "five\n"} {
		if _, err := writer.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		path:        "five\n",
		path + ".1": "three\nfour\n",
		path + ".2": "one\ntwo\n",
	}
	for name, expected := range want {
		data, err := os.ReadFile(name)
		if err != nil || string(data) != expected {
			t.Fatalf("%s = %q, %v; want %q", name, data, err, expected)
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatalf("unexpected fourth archive: %v", err)
	}
}

func TestWriterKeepsOversizedRecordIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.jsonl")
	writer, err := Open(path, 4, 1)
	if err != nil {
		t.Fatal(err)
	}
	record := strings.Repeat("x", 9) + "\n"
	if _, err := writer.Write([]byte(record)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != record {
		t.Fatalf("active = %q, %v", data, err)
	}
}

func TestOpenRejectsInvalidBounds(t *testing.T) {
	for _, test := range []struct {
		maxBytes int64
		archives int
	}{{0, 1}, {1, -1}} {
		if _, err := Open(filepath.Join(t.TempDir(), "daemon.jsonl"), test.maxBytes, test.archives); err == nil {
			t.Fatalf("Open(%d, %d) accepted invalid bounds", test.maxBytes, test.archives)
		}
	}
}
