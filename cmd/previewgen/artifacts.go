//go:build preview

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

const maximumPreviewBytes = 1 << 20

var previewArtifactNames = []string{
	"calendar-front.gif",
	"codex-front.gif",
	"codex-quota-front.gif",
	"github-notifications-front.gif",
	"mac-resources-front.gif",
}

func writeArtifacts(output string, artifacts map[string][]byte) error {
	if err := validateArtifacts(artifacts); err != nil {
		return err
	}
	return writeFileSet(output, artifacts)
}

func validateArtifacts(artifacts map[string][]byte) error {
	if len(artifacts) != len(previewArtifactNames) {
		return errors.New("preview artifact set is incomplete")
	}
	for _, name := range previewArtifactNames {
		content, exists := artifacts[name]
		if !exists || len(content) == 0 {
			return fmt.Errorf("preview artifact %q is missing", name)
		}
		if len(content) > maximumPreviewBytes {
			return fmt.Errorf("preview artifact %q exceeds 1 MiB", name)
		}
	}
	return nil
}

func writeCapturedFixtures(output string, fixtures []mockFixture) error {
	files, err := capturedFixtureFiles(fixtures)
	if err != nil {
		return err
	}
	return writeFileSet(output, files)
}

func capturedFixtureFiles(fixtures []mockFixture) (map[string][]byte, error) {
	if len(fixtures) != len(capturedPreviewArtifactNames) {
		return nil, errors.New("captured preview fixture set is incomplete")
	}
	files := make(map[string][]byte, len(fixtures)*2)
	for _, fixture := range fixtures {
		if !isCapturedPreviewArtifact(fixture.name) {
			return nil, fmt.Errorf("captured preview fixture %q is unexpected", fixture.name)
		}
		if len(fixture.data) == 0 || len(fixture.data) > maximumPreviewBytes {
			return nil, fmt.Errorf("captured preview fixture %q has invalid size", fixture.name)
		}
		if _, exists := files[fixture.name]; exists {
			return nil, fmt.Errorf("captured preview fixture %q is duplicated", fixture.name)
		}
		files[fixture.name] = fixture.data
		files[fixture.name+".sha256"] = []byte(fixture.sha256 + "\n")
	}
	return files, nil
}

func writeFileSet(output string, files map[string][]byte) error {
	return writeFileSets([]fileSet{{output: output, files: files}}, os.Rename)
}

func writeCaptureOutputs(fixtureOutput string, fixtures []mockFixture, artifactOutput string, artifacts map[string][]byte) error {
	fixtureFiles, err := capturedFixtureFiles(fixtures)
	if err != nil {
		return err
	}
	if err := validateArtifacts(artifacts); err != nil {
		return err
	}
	return writeFileSets([]fileSet{
		{output: fixtureOutput, files: fixtureFiles},
		{output: artifactOutput, files: artifacts},
	}, os.Rename)
}

type fileSet struct {
	output string
	files  map[string][]byte
}

type stagedFile struct {
	temporary   string
	destination string
	backup      string
	committed   bool
}

func writeFileSets(sets []fileSet, rename func(string, string) error) error {
	if rename == nil {
		return errors.New("preview file replacement is unavailable")
	}
	staged := make([]stagedFile, 0)
	defer func() {
		for _, file := range staged {
			if file.temporary != "" {
				_ = os.Remove(file.temporary)
			}
		}
	}()
	for _, set := range sets {
		if err := prepareFileSetDirectory(set.output); err != nil {
			return err
		}
		names := make([]string, 0, len(set.files))
		for name := range set.files {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			file, err := os.CreateTemp(set.output, ".preview-*")
			if err != nil {
				return fmt.Errorf("stage preview artifact: %w", err)
			}
			entry := stagedFile{temporary: file.Name(), destination: filepath.Join(set.output, name)}
			staged = append(staged, entry)
			if _, err := file.Write(set.files[name]); err != nil {
				_ = file.Close()
				return fmt.Errorf("stage preview artifact: %w", err)
			}
			if err := file.Chmod(0o644); err != nil {
				_ = file.Close()
				return fmt.Errorf("set preview artifact permissions: %w", err)
			}
			if err := file.Sync(); err != nil {
				_ = file.Close()
				return fmt.Errorf("sync preview artifact: %w", err)
			}
			if err := file.Close(); err != nil {
				return fmt.Errorf("close preview artifact: %w", err)
			}
		}
	}
	for index := range staged {
		if err := commitStagedFile(&staged[index], rename); err != nil {
			return errors.Join(err, rollbackStagedFiles(staged[:index+1], rename))
		}
	}
	for index := range staged {
		if staged[index].backup != "" {
			if err := os.Remove(staged[index].backup); err != nil {
				return fmt.Errorf("remove preview artifact backup: %w", err)
			}
			staged[index].backup = ""
		}
	}
	return nil
}

func prepareFileSetDirectory(output string) error {
	if err := os.MkdirAll(output, 0o755); err != nil {
		return fmt.Errorf("create preview output directory: %w", err)
	}
	info, err := os.Lstat(output)
	if err != nil {
		return fmt.Errorf("inspect preview output directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("preview output must be a directory, not a symlink")
	}
	return nil
}

func commitStagedFile(file *stagedFile, rename func(string, string) error) error {
	if info, err := os.Lstat(file.destination); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("preview artifact destination must be a regular file")
		}
		backup, createErr := os.CreateTemp(filepath.Dir(file.destination), ".preview-backup-*")
		if createErr != nil {
			return fmt.Errorf("stage preview artifact backup: %w", createErr)
		}
		file.backup = backup.Name()
		if closeErr := backup.Close(); closeErr != nil {
			return fmt.Errorf("close preview artifact backup: %w", closeErr)
		}
		if removeErr := os.Remove(file.backup); removeErr != nil {
			return fmt.Errorf("prepare preview artifact backup: %w", removeErr)
		}
		if renameErr := rename(file.destination, file.backup); renameErr != nil {
			return fmt.Errorf("back up preview artifact: %w", renameErr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect preview artifact: %w", err)
	}
	if err := rename(file.temporary, file.destination); err != nil {
		return fmt.Errorf("replace preview artifact: %w", err)
	}
	file.temporary = ""
	file.committed = true
	return nil
}

func rollbackStagedFiles(files []stagedFile, rename func(string, string) error) error {
	var rollbackErr error
	for index := len(files) - 1; index >= 0; index-- {
		file := files[index]
		if file.committed {
			if err := os.Remove(file.destination); err != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("remove replaced preview artifact (backup retained at %q): %w", file.backup, err))
				continue
			}
		}
		if file.backup != "" {
			if err := rename(file.backup, file.destination); err != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore preview artifact (backup retained at %q): %w", file.backup, err))
			}
		}
	}
	return rollbackErr
}
