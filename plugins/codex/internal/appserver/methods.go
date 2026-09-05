package appserver

import (
	"context"
	"encoding/json"
	"errors"
)

type InitializeResponse struct {
	UserAgent      string `json:"userAgent"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOs"`
}

type RateLimitWindow struct {
	UsedPercent           int   `json:"usedPercent"`
	WindowDurationMinutes int64 `json:"windowDurationMins,omitempty"`
	ResetsAt              int64 `json:"resetsAt,omitempty"`
}

type RateLimitSnapshot struct {
	LimitID   string           `json:"limitId,omitempty"`
	Primary   *RateLimitWindow `json:"primary,omitempty"`
	Secondary *RateLimitWindow `json:"secondary,omitempty"`
}

type RateLimitsResponse struct {
	RateLimitsByLimitID map[string]RateLimitSnapshot `json:"rateLimitsByLimitId,omitempty"`
}

func (s *Session) ReadRateLimits(ctx context.Context) (RateLimitsResponse, error) {
	var response RateLimitsResponse
	if err := s.Call(ctx, "account/rateLimits/read", nil, &response); err != nil {
		return RateLimitsResponse{}, err
	}
	return response, nil
}

func (response RateLimitsResponse) CodexSnapshot() (RateLimitSnapshot, bool) {
	if snapshot, exists := response.RateLimitsByLimitID["codex"]; exists {
		return cloneRateLimits(snapshot), true
	}
	return RateLimitSnapshot{}, false
}

func MergeRateLimits(current, update RateLimitSnapshot) RateLimitSnapshot {
	result := cloneRateLimits(current)
	if update.LimitID != "" {
		result.LimitID = update.LimitID
	}
	result.Primary = mergeRateLimitWindow(result.Primary, update.Primary)
	result.Secondary = mergeRateLimitWindow(result.Secondary, update.Secondary)
	return result
}

func mergeRateLimitWindow(current, update *RateLimitWindow) *RateLimitWindow {
	if update == nil {
		return cloneRateLimitWindow(current)
	}
	result := RateLimitWindow{UsedPercent: update.UsedPercent}
	if current != nil {
		result.WindowDurationMinutes = current.WindowDurationMinutes
		result.ResetsAt = current.ResetsAt
	}
	if update.WindowDurationMinutes != 0 {
		result.WindowDurationMinutes = update.WindowDurationMinutes
	}
	if update.ResetsAt != 0 {
		result.ResetsAt = update.ResetsAt
	}
	return &result
}

func cloneRateLimits(value RateLimitSnapshot) RateLimitSnapshot {
	value.Primary = cloneRateLimitWindow(value.Primary)
	value.Secondary = cloneRateLimitWindow(value.Secondary)
	return value
}

func cloneRateLimitWindow(value *RateLimitWindow) *RateLimitWindow {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (s *Session) Initialize(ctx context.Context, rateLimitsEnabled bool) (InitializeResponse, error) {
	optOut := []string{
		"item/agentMessage/delta",
		"item/plan/delta",
		"item/reasoning/summaryTextDelta",
		"item/reasoning/summaryPartAdded",
		"item/reasoning/textDelta",
		"item/commandExecution/outputDelta",
		"item/fileChange/outputDelta",
		"turn/diff/updated",
		"thread/tokenUsage/updated",
	}
	if !rateLimitsEnabled {
		optOut = append(optOut, "account/rateLimits/updated")
	}
	params := map[string]any{
		"clientInfo": map[string]string{
			"name": "bsbctl_plugin_codex", "title": "bsbctl Codex", "version": "0.1.0",
		},
		"capabilities": map[string]any{
			"experimentalApi":           true,
			"optOutNotificationMethods": optOut,
		},
	}
	var response InitializeResponse
	if err := s.Call(ctx, "initialize", params, &response); err != nil {
		return InitializeResponse{}, err
	}
	if err := s.Notify(ctx, "initialized", nil); err != nil {
		return InitializeResponse{}, err
	}
	return response, nil
}

func (s *Session) ListLoadedThreads(ctx context.Context) ([]string, error) {
	type listParams struct {
		Cursor string `json:"cursor,omitempty"`
	}
	type listResponse struct {
		Data       []string `json:"data"`
		NextCursor *string  `json:"nextCursor"`
	}
	var result []string
	var cursor string
	for {
		var response listResponse
		if err := s.Call(ctx, "thread/loaded/list", listParams{Cursor: cursor}, &response); err != nil {
			return nil, err
		}
		result = append(result, response.Data...)
		if response.NextCursor == nil || *response.NextCursor == "" {
			return result, nil
		}
		cursor = *response.NextCursor
	}
}

type ThreadStatus struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags,omitempty"`
}

type ItemSnapshot struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status,omitempty"`
}

type TurnSnapshot struct {
	ID          string          `json:"id"`
	Status      string          `json:"status"`
	Items       []ItemSnapshot  `json:"items"`
	StartedAt   *int64          `json:"startedAt,omitempty"`
	CompletedAt *int64          `json:"completedAt,omitempty"`
	DurationMS  *int64          `json:"durationMs,omitempty"`
	Error       json.RawMessage `json:"error,omitempty"`
}

type ThreadSnapshot struct {
	ID         string
	Name       string
	Preview    string
	CWD        string
	Status     ThreadStatus
	LatestTurn *TurnSnapshot
}

func (s *Session) ResumeThreadSnapshot(ctx context.Context, threadID string) (ThreadSnapshot, error) {
	if threadID == "" {
		return ThreadSnapshot{}, errors.New("thread id must not be empty")
	}
	params := struct {
		ThreadID         string `json:"threadId"`
		ExcludeTurns     bool   `json:"excludeTurns"`
		InitialTurnsPage struct {
			Limit         int    `json:"limit"`
			SortDirection string `json:"sortDirection"`
			ItemsView     string `json:"itemsView"`
		} `json:"initialTurnsPage"`
	}{ThreadID: threadID, ExcludeTurns: true}
	params.InitialTurnsPage.Limit = 1
	params.InitialTurnsPage.SortDirection = "desc"
	params.InitialTurnsPage.ItemsView = "summary"
	var response struct {
		Thread struct {
			ID      string       `json:"id"`
			Name    string       `json:"name"`
			Preview string       `json:"preview"`
			CWD     string       `json:"cwd"`
			Status  ThreadStatus `json:"status"`
		} `json:"thread"`
		InitialTurnsPage struct {
			Data []TurnSnapshot `json:"data"`
		} `json:"initialTurnsPage"`
	}
	if err := s.Call(ctx, "thread/resume", params, &response); err != nil {
		return ThreadSnapshot{}, err
	}
	result := ThreadSnapshot{
		ID: response.Thread.ID, Name: response.Thread.Name, Preview: response.Thread.Preview,
		CWD: response.Thread.CWD, Status: response.Thread.Status,
	}
	if len(response.InitialTurnsPage.Data) != 0 {
		turn := response.InitialTurnsPage.Data[0]
		result.LatestTurn = &turn
	}
	return result, nil
}

func (s *Session) UnsubscribeThread(ctx context.Context, threadID string) error {
	if threadID == "" {
		return errors.New("thread id must not be empty")
	}
	return s.Call(ctx, "thread/unsubscribe", struct {
		ThreadID string `json:"threadId"`
	}{ThreadID: threadID}, nil)
}

func (s *Session) InterruptTurn(ctx context.Context, threadID, turnID string) error {
	if threadID == "" || turnID == "" {
		return errors.New("thread and turn ids must not be empty")
	}
	return s.Call(ctx, "turn/interrupt", struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}{ThreadID: threadID, TurnID: turnID}, nil)
}
