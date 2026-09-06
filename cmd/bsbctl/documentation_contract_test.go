package main

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
)

func TestDocumentationCLIReferenceMatchesHelp(t *testing.T) {
	var output bytes.Buffer
	if err := usage(&output); err != nil {
		t.Fatal(err)
	}
	reference, err := os.ReadFile("../../docs/reference/cli.md")
	if err != nil {
		t.Fatal(err)
	}
	helpInvocations := commandInvocations(output.String())
	referenceInvocations := commandInvocations(string(reference))
	for invocation := range helpInvocations {
		if _, exists := referenceInvocations[invocation]; !exists {
			t.Errorf("CLI reference is missing help invocation %q", invocation)
		}
	}
	for invocation := range referenceInvocations {
		if _, exists := helpInvocations[invocation]; !exists {
			t.Errorf("CLI reference contains obsolete invocation %q", invocation)
		}
	}
}

func TestDocumentationAppConfigurationExamplesDecode(t *testing.T) {
	guides := map[string]string{
		"../../docs/calendar.md":         "calendar",
		"../../docs/codex-app-server.md": "codex",
		"../../docs/codex-quota.md":      "codex-quota",
		"../../docs/mac-resources.md":    "mac-resources",
		"../../docs/slack.md":            "slack",
	}
	for path, appID := range guides {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		examples, err := fencedExamples(string(data), "json")
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(examples) != 1 {
			t.Fatalf("%s contains %d JSON examples; want exactly one app configuration", path, len(examples))
		}
		input, err := readAppConfiguration("-", strings.NewReader(examples[0]))
		if err != nil {
			t.Errorf("%s contains an invalid app configuration: %v", path, err)
			continue
		}
		descriptor, exists := firstpartyplugins.LookupAppID(appID)
		if !exists {
			t.Fatalf("built-in app %q is unavailable", appID)
		}
		if !reflect.DeepEqual(input.Policies, descriptor.DefaultApp.Policies) {
			t.Errorf("%s does not retain the registry-owned policies for %s", path, appID)
		}
		if input.LaunchAction != descriptor.DefaultApp.LaunchAction {
			t.Errorf("%s documents launch action %q; want %q", path, input.LaunchAction, descriptor.DefaultApp.LaunchAction)
		}
	}
}

func commandInvocations(text string) map[string]struct{} {
	result := map[string]struct{}{}
	for line := range strings.Lines(text) {
		invocation := strings.TrimSpace(line)
		if strings.HasPrefix(invocation, "bsbctl ") {
			result[invocation] = struct{}{}
		}
	}
	return result
}

func fencedExamples(text, language string) ([]string, error) {
	opening := "```" + language
	var examples []string
	var current strings.Builder
	inExample := false
	for line := range strings.Lines(text) {
		trimmed := strings.TrimSpace(line)
		if !inExample {
			if trimmed == opening {
				inExample = true
			}
			continue
		}
		if trimmed == "```" {
			examples = append(examples, current.String())
			current.Reset()
			inExample = false
			continue
		}
		current.WriteString(line)
	}
	if inExample {
		return nil, fmt.Errorf("unclosed %s code fence", language)
	}
	return examples, nil
}
