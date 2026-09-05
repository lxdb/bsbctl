package main

import (
	"os"
	"strings"
	"testing"
)

func TestDocumentationPluginArchiveListsRequiredMembers(t *testing.T) {
	document, err := os.ReadFile("../../docs/plugin-packaging.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(document)
	members := expectedArchiveMembers(archiveComponentContract{
		Binary:       "executable",
		MetadataName: "manifest.json",
	})
	for name := range members {
		if strings.HasPrefix(name, "LICENSES/") {
			name = "LICENSES/"
		}
		if !strings.Contains(text, name) {
			t.Errorf("plugin packaging documentation does not list required archive member %q", name)
		}
	}
}
