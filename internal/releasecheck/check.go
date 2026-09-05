// Package releasecheck evaluates machine-checkable public-release inputs.
package releasecheck

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/lxdb/bsbctl/internal/releaseartifact"
	"github.com/lxdb/bsbctl/internal/releasekeys"
)

const busylibModule = "github.com/lxdb/busylib-go"

const maxDependencyClosureBytes = 2 << 20

var findingIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Finding is one release-blocking condition and its exact operator action.
type Finding struct {
	ID             string `json:"id"`
	Message        string `json:"message"`
	OperatorAction string `json:"operator_action"`
}

// Report contains all independently evaluated blockers in stable order.
type Report struct {
	Findings []Finding `json:"findings"`
}

type blockerDocument struct {
	SchemaVersion int        `json:"schema_version"`
	Blockers      *[]Finding `json:"blockers"`
}

type dependencyClosureLister func(string, releaseartifact.Component, string, string) ([]releaseartifact.ModuleVersion, error)

// Check evaluates tracked legal, module-resolution, and catalog-trust inputs.
func Check(root string) (Report, error) {
	return check(root, listReleaseComponentDependencies)
}

func check(root string, listDependencies dependencyClosureLister) (Report, error) {
	if strings.TrimSpace(root) == "" {
		return Report{}, errors.New("release root is required")
	}
	if listDependencies == nil {
		return Report{}, errors.New("release dependency closure lister is required")
	}
	blockers, err := loadBlockers(filepath.Join(root, "release", "blockers.json"))
	if err != nil {
		return Report{}, err
	}
	dependencyDocument, err := os.ReadFile(filepath.Join(root, "release", "dependencies.json"))
	if err != nil {
		return Report{}, fmt.Errorf("read dependency license inventory: %w", err)
	}
	dependencies, err := releaseartifact.DecodeDependencyInventory(dependencyDocument)
	if err != nil {
		return Report{}, err
	}
	blockerIDs := make(map[string]struct{}, len(blockers))
	for _, blocker := range blockers {
		blockerIDs[blocker.ID] = struct{}{}
	}
	for _, dependency := range dependencies.Modules {
		if !dependency.ReleaseBlocked {
			continue
		}
		if _, exists := blockerIDs[dependency.ReleaseBlockerID]; !exists {
			return Report{}, errors.New("release-blocked dependency has no matching release blocker")
		}
	}
	planDocument, err := os.ReadFile(filepath.Join(root, "release", "versions.json"))
	if err != nil {
		return Report{}, fmt.Errorf("read release version metadata: %w", err)
	}
	plan, err := releaseartifact.DecodePlan(planDocument)
	if err != nil {
		return Report{}, err
	}
	if err := checkDependencyClosure(root, plan, dependencies, listDependencies); err != nil {
		return Report{}, err
	}
	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return Report{}, fmt.Errorf("read go.mod: %w", err)
	}
	keyDocument, err := os.ReadFile(filepath.Join(root, "internal", "releasekeys", "catalog_public_keys.json"))
	if err != nil {
		return Report{}, fmt.Errorf("read catalog public keys: %w", err)
	}
	keyring, err := releasekeys.DecodeCatalogKeyring(keyDocument)
	if err != nil {
		return Report{}, err
	}
	findings := slices.Clone(blockers)
	if hasLocalBusylibReplace(string(goMod)) {
		findings = append(findings, Finding{
			ID: "busylib-local-replace", Message: "go.mod replaces github.com/lxdb/busylib-go with a local filesystem path",
			OperatorAction: "publish an immutable reachable busylib-go version and remove the local replacement",
		})
	}
	if len(keyring) == 0 {
		findings = append(findings, Finding{
			ID: "catalog-public-key", Message: "no authorized Ed25519 catalog public key is provisioned",
			OperatorAction: "add the authorized public key to internal/releasekeys/catalog_public_keys.json and review its provenance",
		})
	}
	return Report{Findings: findings}, nil
}

func checkDependencyClosure(root string, plan releaseartifact.Plan, inventory releaseartifact.DependencyInventory, listDependencies dependencyClosureLister) error {
	components := slices.Clone(plan.Components)
	slices.SortFunc(components, func(left, right releaseartifact.Component) int { return cmp.Compare(left.ID, right.ID) })
	var reachable []releaseartifact.ModuleVersion
	for _, goarch := range []string{"arm64", "amd64"} {
		for _, component := range components {
			modules, err := listDependencies(root, component, "darwin", goarch)
			if err != nil {
				return fmt.Errorf("release component dependency closure failed: %w", err)
			}
			reachable = append(reachable, modules...)
			if len(reachable) > 4096 {
				return errors.New("release component dependency closure exceeds module limit")
			}
		}
	}
	selected, err := releaseartifact.SelectDependencies(inventory, reachable)
	if err != nil {
		return err
	}
	if len(selected.Modules) != len(inventory.Modules) {
		return errors.New("dependency license inventory contains a module outside the exact release component closures")
	}
	return nil
}

func listReleaseComponentDependencies(root string, component releaseartifact.Component, goos, goarch string) ([]releaseartifact.ModuleVersion, error) {
	goCache, err := os.MkdirTemp("", "bsbctl-preflight-gocache-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(goCache)
	const format = `{{with .Module}}{{if not .Main}}{{.Path}}{{"\t"}}{{.Version}}{{"\n"}}{{end}}{{end}}`
	command := exec.Command("go", "list", "-mod=readonly", "-deps", "-f", format, component.Package)
	command.Dir = root
	command.Env = dependencyClosureEnvironment(goos, goarch, goCache)
	output := &boundedDependencyOutput{remaining: maxDependencyClosureBytes}
	command.Stdout = output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil || output.truncated {
		return nil, errors.New("bounded offline component dependency listing failed")
	}
	seen := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(output.String()))
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) != 2 || fields[0] == "" || fields[1] == "" || strings.TrimSpace(fields[0]) != fields[0] || strings.TrimSpace(fields[1]) != fields[1] {
			return nil, errors.New("component dependency listing is invalid")
		}
		if version, exists := seen[fields[0]]; exists && version != fields[1] {
			return nil, errors.New("component dependency listing contains multiple versions")
		}
		seen[fields[0]] = fields[1]
		if len(seen) > 4096 {
			return nil, errors.New("component dependency listing exceeds module limit")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("component dependency listing scan failed: %w", err)
	}
	modules := make([]releaseartifact.ModuleVersion, 0, len(seen))
	for module, version := range seen {
		modules = append(modules, releaseartifact.ModuleVersion{Module: module, Version: version})
	}
	slices.SortFunc(modules, func(left, right releaseartifact.ModuleVersion) int { return cmp.Compare(left.Module, right.Module) })
	return modules, nil
}

func dependencyClosureEnvironment(goos, goarch, goCache string) []string {
	blocked := map[string]struct{}{
		"CGO_ENABLED": {}, "CGO_CFLAGS": {}, "CGO_LDFLAGS": {}, "GOARCH": {}, "GOAUTH": {}, "GOCACHE": {}, "GOENV": {}, "GOFLAGS": {}, "GOINSECURE": {}, "GONOPROXY": {}, "GONOSUMDB": {}, "GOOS": {}, "GOPRIVATE": {}, "GOPROXY": {}, "GOSUMDB": {}, "GOTOOLCHAIN": {}, "GOVCS": {}, "GOWORK": {},
	}
	environment := make([]string, 0, len(os.Environ())+12)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, exists := blocked[name]; !exists {
			environment = append(environment, entry)
		}
	}
	environment = append(environment,
		"CGO_ENABLED=1", "GOARCH="+goarch, "GOAUTH=off", "GOCACHE="+goCache, "GOENV=off", "GOINSECURE=", "GONOPROXY=none", "GONOSUMDB=none", "GOOS="+goos,
		"GOPRIVATE=", "GOPROXY=off", "GOSUMDB=off", "GOTOOLCHAIN=local", "GOVCS=*:off", "GOWORK=off",
	)
	if runtime.GOOS == "darwin" && goos == "darwin" && runtime.GOARCH != goarch {
		architecture := goarch
		if goarch == "amd64" {
			architecture = "x86_64"
		}
		environment = append(environment, "CC=clang", "CGO_CFLAGS=-arch "+architecture, "CGO_LDFLAGS=-arch "+architecture)
	}
	return environment
}

type boundedDependencyOutput struct {
	bytes.Buffer
	remaining int
	truncated bool
}

func (output *boundedDependencyOutput) Write(data []byte) (int, error) {
	written := len(data)
	if len(data) > output.remaining {
		data = data[:output.remaining]
		output.truncated = true
	}
	_, _ = output.Buffer.Write(data)
	output.remaining -= len(data)
	return written, nil
}

func loadBlockers(path string) ([]Finding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read release blockers: %w", err)
	}
	document := blockerDocument{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("release blocker document is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("release blocker document is invalid")
	}
	if document.SchemaVersion != 1 || document.Blockers == nil {
		return nil, errors.New("release blocker document is invalid")
	}
	seen := make(map[string]struct{}, len(*document.Blockers))
	for _, finding := range *document.Blockers {
		if !findingIDPattern.MatchString(finding.ID) || strings.TrimSpace(finding.Message) == "" || strings.TrimSpace(finding.Message) != finding.Message || strings.TrimSpace(finding.OperatorAction) == "" || strings.TrimSpace(finding.OperatorAction) != finding.OperatorAction {
			return nil, errors.New("release blocker entry is invalid")
		}
		if _, exists := seen[finding.ID]; exists {
			return nil, fmt.Errorf("release blocker id %q is duplicated", finding.ID)
		}
		seen[finding.ID] = struct{}{}
	}
	return append([]Finding(nil), (*document.Blockers)...), nil
}

func hasLocalBusylibReplace(goMod string) bool {
	inReplaceBlock := false
	for _, raw := range strings.Split(goMod, "\n") {
		line := strings.TrimSpace(strings.SplitN(raw, "//", 2)[0])
		if line == "" {
			continue
		}
		if line == "replace (" {
			inReplaceBlock = true
			continue
		}
		if inReplaceBlock && line == ")" {
			inReplaceBlock = false
			continue
		}
		fields := strings.Fields(line)
		if !inReplaceBlock {
			if len(fields) < 4 || fields[0] != "replace" {
				continue
			}
			fields = fields[1:]
		}
		if len(fields) < 3 || fields[0] != busylibModule {
			continue
		}
		arrow := -1
		for index, field := range fields {
			if field == "=>" {
				arrow = index
				break
			}
		}
		if arrow < 0 || arrow+1 >= len(fields) {
			continue
		}
		target := fields[arrow+1]
		return filepath.IsAbs(target) || strings.HasPrefix(target, ".") || strings.HasPrefix(target, "~")
	}
	return false
}
