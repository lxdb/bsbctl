// Package appsetup defines the host contract for first-party app setup.
// Provider-specific setup policy belongs to the plugin that implements Runner.
package appsetup

import (
	"context"
	"errors"
	"io"

	"github.com/lxdb/bsbctl/internal/config"
	"github.com/lxdb/bsbctl/internal/presentation"
)

// Kind classifies a setup failure without coupling plugins to CLI exit codes.
type Kind uint8

const (
	Usage Kind = iota + 1
	Rejected
	Operational
	Partial
)

// Error is a public-safe setup failure.
type Error struct {
	Kind    Kind
	Message string
}

func (err *Error) Error() string { return err.Message }

// Failure constructs a classified, public-safe setup error.
func Failure(kind Kind, message string) error {
	return &Error{Kind: kind, Message: message}
}

// ErrSecretNotFound lets the host hide its secret-store implementation.
var ErrSecretNotFound = errors.New("app setup secret not found")

// MutationOutcome records whether a missing daemon response can still have
// committed the requested configuration.
type MutationOutcome uint8

const (
	MutationKnown MutationOutcome = iota
	MutationUnknown
)

// Mutation is the host's observation of one configuration transaction.
type Mutation struct {
	Result       ConfigResult
	Outcome      MutationOutcome
	CloseWarning bool
	Err          error
}

const (
	MutationUpdated             = "updated"
	MutationUnchanged           = "unchanged"
	MutationPartial             = "partial"
	MutationDurabilityUncertain = "durability_uncertain"
)

// ConfigResult is the provider-independent result of replacing app configuration.
type ConfigResult struct {
	Status     string
	AppID      string
	Generation uint64
}

// AppStatus contains only the runtime identity needed to reject stale setup.
type AppStatus struct {
	AppID             string
	PluginID          string
	RuntimeGeneration uint64
}

// Status is the daemon state needed to preflight an app setup transaction.
type Status struct {
	Generation uint64
	Apps       []AppStatus
}

// ReplaceConfigRequest contains the complete generic app transaction.
type ReplaceConfigRequest struct {
	AppID              string
	ExpectedGeneration uint64
	Config             []byte
	Secrets            map[string]string
	Policies           map[string]presentation.PolicyConfig
	LaunchAction       string
}

// Configuration is the validated app configuration envelope supplied by the CLI.
type Configuration struct {
	Config               []byte
	Secrets              map[string]string
	Policies             map[string]presentation.PolicyConfig
	LaunchAction         string
	LaunchActionProvided bool
}

// Host provides setup implementations with generic configuration and secret
// operations. It contains no provider policy.
type Host interface {
	ReadConfiguration(string, io.Reader) (Configuration, error)
	LoadDocument() (config.Document, error)
	DaemonStatus(context.Context) (Status, error)
	ResolveSecret(context.Context, string) (string, error)
	StoreSecret(context.Context, string, string) error
	ReplaceConfiguration(context.Context, ReplaceConfigRequest) Mutation
}

// Runner owns one plugin's setup syntax and policy.
type Runner func(context.Context, []string, io.Reader, io.Writer, io.Writer, Host) error
