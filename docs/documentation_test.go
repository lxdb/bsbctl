package docs_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/lxdb/bsbctl/internal/firstpartyplugins"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

var markdownLink = regexp.MustCompile(`\[[^]]*\]\(([^)]+)\)`)

func TestWithoutFencedCode(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "backticks", input: "before\n```text\nhidden\n```\nafter\n", want: "before\nafter\n"},
		{name: "tildes", input: "before\n~~~\nhidden\n~~~~\nafter\n", want: "before\nafter\n"},
		{name: "embedded shorter run", input: "````\n```\nhidden\n````\nafter\n", want: "after\n"},
		{name: "indented by three spaces", input: "   ```go\nhidden\n   ```\nafter\n", want: "after\n"},
		{name: "indented by four spaces is prose", input: "    ```\nvisible\n", want: "    ```\nvisible\n"},
		{name: "unclosed", input: "before\n```\nhidden\n", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := withoutFencedCode(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("withoutFencedCode() error = %v, wantErr %v", err, test.wantErr)
			}
			if got != test.want {
				t.Errorf("withoutFencedCode() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDocumentationHubRoutesHumanTasks(t *testing.T) {
	text := readFile(t, "README.md")
	for _, target := range []string{
		"getting-started.md", "apps.md", "device-and-attention.md", "operations.md",
		"reference/cli.md", "reference/errors.md", "plugin-authoring.md",
		"reference/plugin-sdk.md", "plugin-packaging.md", "protocol/v1/spec.md",
		"reference/architecture.md", "reference/compatibility.md", "maintainers/development.md",
		"maintainers/testing.md", "release.md",
	} {
		if !strings.Contains(text, "("+target+")") {
			t.Errorf("documentation hub does not route to %s", target)
		}
	}
}

func TestBuiltInAppDocumentationMatchesRegistry(t *testing.T) {
	text := readFile(t, "apps.md")
	for _, descriptor := range firstpartyplugins.All() {
		appRow := fmt.Sprintf("| %s | `%s` | `%s` |", descriptor.DisplayName, descriptor.DefaultApp.ID, descriptor.ID)
		if !strings.Contains(text, appRow) || !strings.Contains(text, descriptor.Requirement) {
			t.Errorf("apps.md does not bind %s to app %q, plugin %q, and requirement %q", descriptor.DisplayName, descriptor.DefaultApp.ID, descriptor.ID, descriptor.Requirement)
		}
		definition := descriptor.DefinitionForVersion(descriptor.DevelopmentVersion)
		for _, operation := range definition.Contract.Operations {
			kind := "Query"
			if operation.Kind == protocol.OperationCommand {
				kind = "Command"
			}
			operationRow := fmt.Sprintf("| %s | %s | `%s` |", descriptor.DisplayName, kind, operation.ID)
			if !strings.Contains(text, operationRow) {
				t.Errorf("apps.md does not bind %s operation %q to kind %s", descriptor.DisplayName, operation.ID, kind)
			}
		}
	}
}

func TestAppGuidesCoverConfigurationSchemas(t *testing.T) {
	guides := map[string]string{
		"plugins/calendar/config.schema.json":     "calendar.md",
		"plugins/codex/config.schema.json":        "codex-app-server.md",
		"plugins/codexquota/config.schema.json":   "codex-quota.md",
		"plugins/macresources/config.schema.json": "mac-resources.md",
	}
	for schemaPath, guidePath := range guides {
		schemaData := readFile(t, "../"+schemaPath)
		var schema map[string]any
		if err := json.Unmarshal([]byte(schemaData), &schema); err != nil {
			t.Fatalf("decode %s: %v", schemaPath, err)
		}
		guide := readFile(t, guidePath)
		rows := markdownTableRows(guide)
		for _, field := range schemaFields(schema) {
			row, exists := rows["`"+field.name+"`"]
			if !exists {
				t.Errorf("%s does not document schema field %q in a table row", guidePath, field.name)
				continue
			}
			if field.hasDefault {
				want := "`" + field.defaultValue + "`"
				if len(row) < 3 || row[2] != want {
					t.Errorf("%s documents schema field %q with default %q; want %s", guidePath, field.name, tableCell(row, 2), want)
				}
			}
		}
	}
}

func TestPluginSDKReferenceNamesPublicAPIsAndRoutesToCompleteDocs(t *testing.T) {
	reference := readFile(t, "reference/plugin-sdk.md")
	for packageName, directory := range map[string]string{
		"plugin":   "../sdk/plugin",
		"protocol": "../sdk/protocol",
		"rpc":      "../sdk/rpc",
	} {
		command := "go doc -all github.com/lxdb/bsbctl/sdk/" + packageName
		if !strings.Contains(reference, command) {
			t.Errorf("plugin SDK reference does not route to the complete API with %q", command)
		}
		symbols := publicPackageSymbols(t, directory)
		methods := publicPackageMethods(t, directory)
		pattern := regexp.MustCompile("`" + packageName + `\.([A-Za-z0-9_.]+)` + "`")
		for _, match := range pattern.FindAllStringSubmatch(reference, -1) {
			_, isSymbol := symbols[match[1]]
			_, isMethod := methods[match[1]]
			if !isSymbol && !isMethod {
				t.Errorf("plugin SDK reference names unavailable public API %s.%s", packageName, match[1])
			}
		}
	}
}

func TestProtocolDocumentationMatchesPublicSDK(t *testing.T) {
	want := "exactly `" + protocol.Version + "`"
	for _, path := range []string{"reference/compatibility.md", "reference/plugin-sdk.md", "protocol/v1/spec.md"} {
		text := readFile(t, path)
		if !strings.Contains(text, protocol.Version) {
			t.Errorf("%s does not mention protocol %s", path, protocol.Version)
		}
		if path == "reference/compatibility.md" && !strings.Contains(text, want) {
			t.Errorf("%s does not state the exact protocol contract", path)
		}
		if !strings.Contains(text, "JSON-RPC 2.0") {
			t.Errorf("%s does not identify JSON-RPC 2.0 as framing", path)
		}
	}
	methods := rpcMethodNames(t, "../sdk/plugin/plugin.go", "../internal/pluginhost/process.go", "../sdk/rpc/rpc.go")
	spec := readFile(t, "protocol/v1/spec.md")
	for method := range methods {
		if !strings.Contains(spec, "`"+method+"`") {
			t.Errorf("protocol specification does not document registered method %q", method)
		}
	}
}

func TestMarkdownLinksAndFragmentsResolveOutsideCodeFences(t *testing.T) {
	paths, err := documentationMarkdownPaths()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		text, fenceErr := withoutFencedCode(readFile(t, path))
		if fenceErr != nil {
			t.Errorf("%s: %v", path, fenceErr)
			continue
		}
		for _, match := range markdownLink.FindAllStringSubmatch(text, -1) {
			target := strings.Trim(strings.TrimSpace(match[1]), "<>")
			if target == "" || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			fileTarget, fragment, _ := strings.Cut(target, "#")
			resolved := path
			if fileTarget != "" {
				resolved = filepath.Clean(filepath.Join(filepath.Dir(path), filepath.FromSlash(fileTarget)))
			}
			info, statErr := os.Stat(resolved)
			if statErr != nil {
				t.Errorf("%s links to unavailable %s: %v", path, target, statErr)
				continue
			}
			if fragment == "" || info.IsDir() || filepath.Ext(resolved) != ".md" {
				continue
			}
			hasAnchor, anchorErr := documentHasAnchor(readFile(t, resolved), fragment)
			if anchorErr != nil {
				t.Errorf("%s: %v", resolved, anchorErr)
				continue
			}
			if !hasAnchor {
				t.Errorf("%s links to missing fragment %s in %s", path, fragment, resolved)
			}
		}
	}
}

func documentationMarkdownPaths() ([]string, error) {
	paths, err := filepath.Glob("../*.md")
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		paths = append(paths, path)
		return nil
	})
	return paths, err
}

func TestThirdPartyNoticeLinksEveryPreservedLicense(t *testing.T) {
	notices := readFile(t, "../THIRD_PARTY_NOTICES.md")
	entries, err := os.ReadDir("../LICENSES")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".txt" {
			t.Fatalf("LICENSES contains unexpected entry %q", entry.Name())
		}
		target := "LICENSES/" + entry.Name()
		if !strings.Contains(notices, "]("+target+")") {
			t.Errorf("third-party notices do not link %s", target)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

type schemaField struct {
	name         string
	defaultValue string
	hasDefault   bool
}

func schemaFields(schema map[string]any) []schemaField {
	var fields []schemaField
	var visit func(map[string]any)
	visit = func(node map[string]any) {
		if properties, ok := node["properties"].(map[string]any); ok {
			for name, raw := range properties {
				property, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				field := schemaField{name: name}
				if value, exists := property["default"]; exists {
					data, _ := json.Marshal(value)
					field.defaultValue = strings.Trim(string(data), `"`)
					field.hasDefault = true
				}
				fields = append(fields, field)
				visit(property)
				if items, ok := property["items"].(map[string]any); ok {
					visit(items)
				}
			}
		}
	}
	visit(schema)
	return fields
}

func markdownTableRows(text string) map[string][]string {
	rows := map[string][]string{}
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for index := range cells {
			cells[index] = strings.TrimSpace(cells[index])
		}
		if len(cells) > 0 {
			if _, exists := rows[cells[0]]; !exists {
				rows[cells[0]] = cells
			}
		}
	}
	return rows
}

func tableCell(row []string, index int) string {
	if index >= len(row) {
		return "<missing>"
	}
	return row[index]
}

func publicPackageSymbols(t *testing.T, directory string) map[string]struct{} {
	t.Helper()
	symbols := map[string]struct{}{}
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, declaration := range file.Decls {
			switch value := declaration.(type) {
			case *ast.FuncDecl:
				if value.Recv == nil && value.Name.IsExported() {
					symbols[value.Name.Name] = struct{}{}
				}
			case *ast.GenDecl:
				for _, spec := range value.Specs {
					switch spec := spec.(type) {
					case *ast.ValueSpec:
						for _, name := range spec.Names {
							if name.IsExported() {
								symbols[name.Name] = struct{}{}
							}
						}
					case *ast.TypeSpec:
						if spec.Name.IsExported() {
							symbols[spec.Name.Name] = struct{}{}
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return symbols
}

func publicPackageMethods(t *testing.T, directory string) map[string]struct{} {
	t.Helper()
	methods := map[string]struct{}{}
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, declaration := range file.Decls {
			switch value := declaration.(type) {
			case *ast.FuncDecl:
				if value.Recv == nil || !value.Name.IsExported() {
					continue
				}
				receiver := receiverTypeName(value.Recv.List[0].Type)
				if ast.IsExported(receiver) {
					methods[receiver+"."+value.Name.Name] = struct{}{}
				}
			case *ast.GenDecl:
				for _, declarationSpec := range value.Specs {
					typeSpec, ok := declarationSpec.(*ast.TypeSpec)
					if !ok || !typeSpec.Name.IsExported() {
						continue
					}
					iface, ok := typeSpec.Type.(*ast.InterfaceType)
					if !ok {
						continue
					}
					for _, field := range iface.Methods.List {
						for _, name := range field.Names {
							if name.IsExported() {
								methods[typeSpec.Name.Name+"."+name.Name] = struct{}{}
							}
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return methods
}

func receiverTypeName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.StarExpr:
		return receiverTypeName(value.X)
	default:
		return ""
	}
}

func rpcMethodNames(t *testing.T, paths ...string) map[string]struct{} {
	t.Helper()
	methods := map[string]struct{}{}
	methodPattern := regexp.MustCompile(`^(host|plugin|rpc)(\.[a-z]+)+$`)
	for _, path := range paths {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err == nil && methodPattern.MatchString(value) {
				methods[value] = struct{}{}
			}
			return true
		})
	}
	return methods
}

type markdownFence struct {
	marker byte
	length int
}

func withoutFencedCode(text string) (string, error) {
	var output strings.Builder
	var fence markdownFence
	for line := range strings.Lines(text) {
		marker, length, rest, ok := fenceMarker(line)
		if fence.length == 0 && ok {
			fence = markdownFence{marker: marker, length: length}
			continue
		}
		if fence.length != 0 && ok && marker == fence.marker && length >= fence.length && strings.TrimSpace(rest) == "" {
			fence = markdownFence{}
			continue
		}
		if fence.length == 0 {
			output.WriteString(line)
		}
	}
	if fence.length != 0 {
		return "", errors.New("unclosed fenced code block")
	}
	return output.String(), nil
}

func fenceMarker(line string) (byte, int, string, bool) {
	line = strings.TrimRight(line, "\r\n")
	spaces := 0
	for spaces < len(line) && line[spaces] == ' ' {
		spaces++
	}
	if spaces > 3 || spaces == len(line) || (line[spaces] != '`' && line[spaces] != '~') {
		return 0, 0, "", false
	}
	marker := line[spaces]
	end := spaces
	for end < len(line) && line[end] == marker {
		end++
	}
	if end-spaces < 3 {
		return 0, 0, "", false
	}
	return marker, end - spaces, line[end:], true
}

func documentHasAnchor(text, want string) (bool, error) {
	want = strings.ToLower(want)
	visible, err := withoutFencedCode(text)
	if err != nil {
		return false, err
	}
	for line := range strings.Lines(visible) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") {
			continue
		}
		heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
		if markdownAnchor(heading) == want {
			return true, nil
		}
	}
	return false, nil
}

func markdownAnchor(heading string) string {
	var output strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			output.WriteRune(r)
			lastHyphen = r == '-'
		case unicode.IsSpace(r):
			if output.Len() > 0 && !lastHyphen {
				output.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	return strings.Trim(output.String(), "-")
}
