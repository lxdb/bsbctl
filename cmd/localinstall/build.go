package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/configschema"
	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
)

// Use the normal build cache and the checkout's pinned toolchain. Registration
// belongs to the newly built core: it verifies local package metadata without
// involving the signed catalog or modifying the installed configuration.
func buildLocalPackages(ctx context.Context, root, stage string, selected []firstpartyplugins.Descriptor, progress io.Writer) (config.Document, string, error) {
	build := func(binary, commandPackage string) error {
		fmt.Fprintf(progress, "Building %s\n", filepath.Base(binary))
		command := exec.CommandContext(ctx, "sh", filepath.Join(root, "scripts/go.sh"), "build", "-o", binary, commandPackage)
		command.Dir, command.Stdout, command.Stderr = root, progress, progress
		if err := command.Run(); err != nil {
			return fmt.Errorf("build %s: %w", commandPackage, err)
		}
		if runtime.GOOS == "darwin" {
			command := exec.CommandContext(ctx, "/usr/bin/codesign", "--force", "--sign", "-", binary)
			command.Stdout, command.Stderr = progress, progress
			if err := command.Run(); err != nil {
				return fmt.Errorf("ad-hoc sign %s: %w", filepath.Base(binary), err)
			}
		}
		return nil
	}
	core := filepath.Join(stage, "bsbctl")
	if err := build(core, "./cmd/bsbctl"); err != nil {
		return config.Document{}, "", err
	}
	registration := filepath.Join(stage, "registration.json")
	args := []string{"init", "--config", registration}
	for _, descriptor := range selected {
		packageRoot := filepath.Join(stage, descriptor.DefaultApp.ID)
		binary := filepath.Join(packageRoot, descriptor.Binary)
		if err := build(binary, descriptor.CommandPackage); err != nil {
			return config.Document{}, "", err
		}
		if err := copyLocalFile(filepath.Join(root, descriptor.SchemaPath), filepath.Join(packageRoot, configschema.FileName), 0o644); err != nil {
			return config.Document{}, "", err
		}
		for _, asset := range descriptor.Assets {
			if err := copyLocalFile(filepath.Join(root, descriptor.AssetRoot, filepath.FromSlash(asset.Source)), filepath.Join(packageRoot, filepath.FromSlash(asset.Source)), 0o644); err != nil {
				return config.Document{}, "", err
			}
		}
		args = append(args, "--plugin", binary)
	}
	command := exec.CommandContext(ctx, core, args...)
	command.Stdout, command.Stderr = io.Discard, progress
	if err := command.Run(); err != nil {
		return config.Document{}, "", fmt.Errorf("validate built packages: %w", err)
	}
	document, err := config.NewStore(registration).Load()
	if err != nil {
		return config.Document{}, "", err
	}
	if err := verifyLocalAssets(document); err != nil {
		return config.Document{}, "", err
	}
	var version bytes.Buffer
	command = exec.CommandContext(ctx, core, "version")
	command.Stdout, command.Stderr = &version, progress
	if err := command.Run(); err != nil {
		return config.Document{}, "", err
	}
	value := strings.TrimSpace(version.String())
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return config.Document{}, "", errors.New("built core returned an invalid version")
	}
	return document, value, nil
}

func verifyLocalAssets(document config.Document) error {
	for _, plugin := range document.Plugins {
		for _, asset := range plugin.Assets {
			path := filepath.Join(plugin.PackageRoot, filepath.FromSlash(asset.Source))
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || info.Size() != asset.Size {
				return fmt.Errorf("local package asset is missing or has the wrong size: %s", path)
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			digest := sha256.New()
			count, copyErr := io.Copy(digest, io.LimitReader(file, asset.Size+1))
			if err := errors.Join(copyErr, file.Close()); err != nil {
				return err
			}
			if count != asset.Size || fmt.Sprintf("%x", digest.Sum(nil)) != asset.SHA256 {
				return fmt.Errorf("local package asset checksum does not match its declaration: %s", path)
			}
		}
	}
	return nil
}
