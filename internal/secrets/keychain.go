// Package secrets resolves durable opaque references without placing values in
// configuration, process arguments, or environment variables.
package secrets

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxAccountBytes         = 256
	maxSecurityCommandBytes = 4096
)

var (
	ErrInvalidReference = errors.New("invalid keychain reference")
	ErrItemNotFound     = errors.New("macOS Keychain item not found")
	servicePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// Reference is a validated macOS Keychain generic-password identity.
type Reference struct {
	Service string
	Account string
}

type commandRunner func(context.Context, string, []string, io.Reader) ([]byte, error)

type exitCoder interface {
	error
	ExitCode() int
}

type Keychain struct {
	run commandRunner
}

func NewKeychain(run commandRunner) *Keychain {
	if run == nil {
		run = func(ctx context.Context, name string, args []string, input io.Reader) ([]byte, error) {
			command := exec.CommandContext(ctx, name, args...)
			command.Stdin = input
			return command.Output()
		}
	}
	return &Keychain{run: run}
}

// Resolve reads one macOS generic-password item. The reference format is
// keychain://<service>/<account>; the returned value never appears in errors.
func (k *Keychain) Resolve(ctx context.Context, reference string) (string, error) {
	parsed, err := ParseReference(reference)
	if err != nil {
		return "", err
	}
	output, err := k.run(ctx, "/usr/bin/security", []string{"find-generic-password", "-w", "-s", parsed.Service, "-a", parsed.Account}, nil)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if code, ok := errors.AsType[exitCoder](err); ok && code.ExitCode() == 44 {
			return "", ErrItemNotFound
		}
		return "", fmt.Errorf("read macOS Keychain item: %w", err)
	}
	value := strings.TrimSuffix(string(output), "\n")
	value = strings.TrimSuffix(value, "\r")
	if value == "" {
		return "", errors.New("macOS Keychain item is empty")
	}
	return value, nil
}

// Store creates one macOS generic-password item without exposing value in
// process arguments or environment variables. It refuses to replace an item.
func (k *Keychain) Store(ctx context.Context, reference, value string) error {
	parsed, err := ParseReference(reference)
	if err != nil {
		return err
	}
	if value == "" {
		return errors.New("macOS Keychain item is empty")
	}
	// A bare -w prompts on /dev/tty; it does not read a password from stdin.
	// The interactive command parser accepts one bounded command followed by
	// EOF. Hex input keeps arbitrary password bytes out of that parser's syntax.
	command := "add-generic-password -a " + quoteSecurityArgument(parsed.Account) +
		" -s " + quoteSecurityArgument(parsed.Service) + " -X " + hex.EncodeToString([]byte(value)) + "\n"
	if len(command) >= maxSecurityCommandBytes {
		return errors.New("macOS Keychain value exceeds secure input limit")
	}
	if _, err := k.run(ctx, "/usr/bin/security", []string{"-i", "-q"}, strings.NewReader(command)); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("write macOS Keychain item: %w", err)
	}
	return nil
}

func quoteSecurityArgument(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

// ParseReference validates a keychain://service/account reference without
// retaining or echoing the original input in errors.
func ParseReference(reference string) (Reference, error) {
	parsed, err := url.Parse(reference)
	if err != nil || !strings.HasPrefix(reference, "keychain:") || parsed.Scheme != "keychain" || parsed.Opaque != "" || parsed.Host == "" ||
		parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(reference, "#") ||
		parsed.Host != parsed.Hostname() || !servicePattern.MatchString(parsed.Host) {
		return Reference{}, ErrInvalidReference
	}
	escapedPath := parsed.EscapedPath()
	if len(escapedPath) < 2 || escapedPath[0] != '/' {
		return Reference{}, ErrInvalidReference
	}
	escapedSegments := strings.Split(escapedPath[1:], "/")
	segments := make([]string, len(escapedSegments))
	for index, escaped := range escapedSegments {
		if escaped == "" {
			return Reference{}, ErrInvalidReference
		}
		lower := strings.ToLower(escaped)
		if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
			return Reference{}, ErrInvalidReference
		}
		segment, err := url.PathUnescape(escaped)
		if err != nil || segment == "." || segment == ".." || strings.ContainsAny(segment, `/\`) || !utf8.ValidString(segment) {
			return Reference{}, ErrInvalidReference
		}
		for _, value := range segment {
			if unicode.IsControl(value) {
				return Reference{}, ErrInvalidReference
			}
		}
		segments[index] = segment
	}
	account := strings.Join(segments, "/")
	if len(account) > maxAccountBytes {
		return Reference{}, ErrInvalidReference
	}
	return Reference{Service: parsed.Host, Account: account}, nil
}
