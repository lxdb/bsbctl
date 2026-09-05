package main

import (
	"context"
	"io"
	"strconv"
	"time"

	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/control"
)

func runAttention(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return commandFailure(exitUsage, "attention command requires status, explain, acknowledge, or history")
	}
	command := args[0]
	allowed := []string{"socket"}
	if command == "history" {
		allowed = append(allowed, "limit", "since")
	}
	options, positionals, err := parseOptions(args[1:], allowed...)
	if err != nil {
		return commandFailure(exitUsage, "invalid attention flags")
	}
	socketPath, err := resolveStatePath(options, "socket", "ctl.sock")
	if err != nil {
		return err
	}
	switch command {
	case "status":
		if len(positionals) != 0 {
			return commandFailure(exitUsage, "attention status does not accept arguments")
		}
		var result attention.Trace
		if err := callDaemon(ctx, socketPath, "attention.snapshot", nil, &result); err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "explain":
		if len(positionals) != 1 {
			return commandFailure(exitUsage, "attention explain requires one observation id")
		}
		var result attention.Evaluation
		if err := callDaemon(ctx, socketPath, "attention.explain", control.AttentionExplainRequest{ObservationID: positionals[0]}, &result); err != nil {
			return err
		}
		return writeJSON(stdout, result)
	case "acknowledge":
		if len(positionals) != 1 {
			return commandFailure(exitUsage, "attention acknowledge requires one observation id")
		}
		if err := callDaemon(ctx, socketPath, "attention.acknowledge", control.AttentionAcknowledgeRequest{ObservationID: positionals[0]}, nil); err != nil {
			return err
		}
		return writeJSON(stdout, struct {
			Status        string `json:"status"`
			ObservationID string `json:"observation_id"`
		}{Status: "acknowledged", ObservationID: positionals[0]})
	case "history":
		if len(positionals) != 0 {
			return commandFailure(exitUsage, "attention history does not accept positional arguments")
		}
		limit := 50
		if options["limit"] != "" {
			limit, err = strconv.Atoi(options["limit"])
			if err != nil || limit < 1 || limit > 1000 {
				return commandFailure(exitUsage, "attention history limit is invalid")
			}
		}
		request := control.AttentionHistoryRequest{Limit: limit}
		if options["since"] != "" {
			since, parseErr := time.ParseDuration(options["since"])
			if parseErr != nil || since <= 0 {
				return commandFailure(exitUsage, "attention history duration is invalid")
			}
			request.Since = time.Now().UTC().Add(-since)
		}
		var result control.AttentionHistoryResult
		if err := callDaemon(ctx, socketPath, "attention.history", request, &result); err != nil {
			return err
		}
		return writeJSON(stdout, result)
	default:
		return commandFailure(exitUsage, "invalid attention command")
	}
}
