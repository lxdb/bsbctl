package secrets

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestParseReferenceAcceptsSafeHierarchicalAccount(t *testing.T) {
	t.Parallel()
	reference, err := ParseReference("keychain://bsbctl/github-actions/token")
	if err != nil {
		t.Fatalf("ParseReference: %v", err)
	}
	if reference.Service != "bsbctl" || reference.Account != "github-actions/token" {
		t.Fatalf("reference = %#v", reference)
	}
}

func TestParseReferenceRejectsUnsafeStructureAndAccountSmuggling(t *testing.T) {
	t.Parallel()
	longAccount := strings.Repeat("a", 257)
	tests := map[string]string{
		"wrong case scheme":        "KEYCHAIN://bsbctl/account",
		"user info":                "keychain://user@bsbctl/account",
		"port":                     "keychain://bsbctl:443/account",
		"empty query":              "keychain://bsbctl/account?",
		"empty fragment":           "keychain://bsbctl/account#",
		"empty query and fragment": "keychain://bsbctl/account?#",
		"query":                    "keychain://bsbctl/account?token=secret-canary",
		"fragment":                 "keychain://bsbctl/account#fragment",
		"unsafe service":           "keychain://busy$ctl/account",
		"empty account":            "keychain://bsbctl/",
		"empty segment":            "keychain://bsbctl/one//two",
		"trailing empty segment":   "keychain://bsbctl/one/",
		"dot segment":              "keychain://bsbctl/one/./two",
		"encoded dot segment":      "keychain://bsbctl/one/%2e%2e/two",
		"encoded slash":            "keychain://bsbctl/one%2Ftwo",
		"encoded backslash":        "keychain://bsbctl/one%5Ctwo",
		"literal backslash":        `keychain://bsbctl/one\two`,
		"nul":                      "keychain://bsbctl/one%00two",
		"control":                  "keychain://bsbctl/one%0Atwo",
		"account over byte limit":  "keychain://bsbctl/" + longAccount,
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseReference(value)
			if err == nil {
				t.Fatalf("ParseReference accepted %q", value)
			}
			if strings.Contains(err.Error(), value) || strings.Contains(err.Error(), "secret-canary") {
				t.Fatalf("error echoed reference: %v", err)
			}
		})
	}
}

func TestKeychainResolveUsesReferenceAsServiceAndAccount(t *testing.T) {
	t.Parallel()
	var name string
	var args []string
	keychain := NewKeychain(func(_ context.Context, command string, commandArgs []string, input io.Reader) ([]byte, error) {
		name = command
		args = append([]string(nil), commandArgs...)
		if input != nil {
			t.Fatal("Resolve supplied command input")
		}
		return []byte("top-secret\n"), nil
	})
	value, err := keychain.Resolve(context.Background(), "keychain://bsbctl/github-actions/token")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if value != "top-secret" {
		t.Fatalf("value = %q", value)
	}
	if name != "/usr/bin/security" || !reflect.DeepEqual(args, []string{"find-generic-password", "-w", "-s", "bsbctl", "-a", "github-actions/token"}) {
		t.Fatalf("command = %q %v", name, args)
	}
}

func TestKeychainRejectsMalformedReferenceBeforeCommand(t *testing.T) {
	t.Parallel()
	called := false
	keychain := NewKeychain(func(context.Context, string, []string, io.Reader) ([]byte, error) {
		called = true
		return nil, nil
	})
	if _, err := keychain.Resolve(context.Background(), "plaintext"); err == nil {
		t.Fatal("Resolve accepted plaintext")
	}
	if called {
		t.Fatal("security command was called")
	}
}

func TestKeychainStoreSuppliesSecretOnlyThroughStdin(t *testing.T) {
	t.Parallel()
	var name string
	var args []string
	var stdin string
	keychain := NewKeychain(func(_ context.Context, command string, commandArgs []string, input io.Reader) ([]byte, error) {
		name = command
		args = append([]string(nil), commandArgs...)
		data, err := io.ReadAll(input)
		if err != nil {
			t.Fatal(err)
		}
		stdin = string(data)
		return nil, nil
	})
	if err := keychain.Store(t.Context(), "keychain://bsbctl/device/access-token", "token-secret"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if name != "/usr/bin/security" || !reflect.DeepEqual(args, []string{"-i", "-q"}) {
		t.Fatalf("command = %q %v", name, args)
	}
	if stdin != "add-generic-password -a \"device/access-token\" -s \"bsbctl\" -X 746f6b656e2d736563726574\n" {
		t.Fatalf("stdin = %q", stdin)
	}
	if strings.Contains(strings.Join(args, " "), "token-secret") {
		t.Fatal("token was supplied in command arguments")
	}
}

func TestKeychainStoreFramesQuotedAccountAndArbitraryValueAsOneCommand(t *testing.T) {
	value := "canary\nhelp\x00\""
	keychain := NewKeychain(func(_ context.Context, _ string, args []string, input io.Reader) ([]byte, error) {
		data, err := io.ReadAll(input)
		if err != nil {
			t.Fatal(err)
		}
		line := string(data)
		want := "add-generic-password -a \"work \\\"quoted\\\"/token\" -s \"bsbctl\" -X " + hex.EncodeToString([]byte(value)) + "\n"
		if line != want || strings.Count(line, "\n") != 1 || strings.Contains(strings.Join(args, " "), "canary") {
			t.Fatalf("unsafe command framing: args=%q, stdin=%q", args, line)
		}
		return nil, nil
	})
	if err := keychain.Store(t.Context(), "keychain://bsbctl/work%20%22quoted%22/token", value); err != nil {
		t.Fatal(err)
	}
	keychain = NewKeychain(func(context.Context, string, []string, io.Reader) ([]byte, error) {
		t.Fatal("oversized command reached the native parser")
		return nil, nil
	})
	if err := keychain.Store(t.Context(), "keychain://bsbctl/token", strings.Repeat("canary", maxSecurityCommandBytes)); err == nil {
		t.Fatal("oversized secure input was accepted")
	}
}

func TestSecurityInteractiveArgumentFramingWithReadOnlyHelp(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS SecurityTool parser")
	}
	argument := `audit "quoted" account; list-keychains`
	command := exec.CommandContext(t.Context(), "/usr/bin/security", "-i", "-q")
	// Only help is executed. This probes the real parser without reading or
	// changing a Keychain item or requesting an authorization prompt.
	command.Stdin = strings.NewReader("help " + quoteSecurityArgument(argument) + "\n")
	output, err := command.CombinedOutput()
	if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 1 || !strings.Contains(string(output), argument) {
		t.Fatalf("SecurityTool did not preserve one quoted help argument: %v\n%s", err, output)
	}
}

func TestKeychainResolveClassifiesMissingItem(t *testing.T) {
	t.Parallel()
	keychain := NewKeychain(func(context.Context, string, []string, io.Reader) ([]byte, error) {
		return nil, exitCodeError(44)
	})
	_, err := keychain.Resolve(t.Context(), "keychain://bsbctl/device/access-token")
	if !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("Resolve error = %v, want ErrItemNotFound", err)
	}
}

func TestKeychainPreservesContextCancellationFromKilledSecurityProcess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  func(context.Context, *Keychain) error
	}{
		{
			name: "resolve",
			run: func(ctx context.Context, keychain *Keychain) error {
				_, err := keychain.Resolve(ctx, "keychain://bsbctl/device/access-token")
				return err
			},
		},
		{
			name: "store",
			run: func(ctx context.Context, keychain *Keychain) error {
				return keychain.Store(ctx, "keychain://bsbctl/device/access-token", "token-secret")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			keychain := NewKeychain(func(context.Context, string, []string, io.Reader) ([]byte, error) {
				return nil, exitCodeError(1)
			})
			if err := test.run(ctx, keychain); !errors.Is(err, context.Canceled) {
				t.Fatalf("error=%v, want context.Canceled", err)
			}
		})
	}
}

type exitCodeError int

func (e exitCodeError) Error() string { return "command failed" }
func (e exitCodeError) ExitCode() int { return int(e) }
