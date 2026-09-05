//go:build preview

package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lxdb/bsbctl/plugins/codex/internal/appserver"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	previewThreadID   = "preview-thread"
	previewTurnID     = "preview-turn"
	previewProjectDir = "/Users/example/Documents/bsbctl"
)

// PreviewScenes drives public-safe mock events through the production reducer,
// interaction, and publication paths. It does not connect to a Codex app server.
func PreviewScenes(now time.Time) ([]protocol.Scene, error) {
	scenes := make([]protocol.Scene, 0, 15)
	appendState := func(reducer *Reducer, state string) error {
		for _, card := range reducer.Cards() {
			if card.StateWord == state {
				scenes = append(scenes, cardPresentation(card))
				return nil
			}
		}
		return fmt.Errorf("production reducer did not publish preview state %q", state)
	}

	running := newPreviewReducer(now)
	if err := appendState(running, "RUN"); err != nil {
		return nil, err
	}
	running.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "preview-thread-2", Name: "Preview fixture review", CWD: previewProjectDir,
		Status: appserver.ThreadStatus{Type: "active"},
	}})
	if err := appendState(running, "2 ACT"); err != nil {
		return nil, err
	}

	pinned := newPreviewReducer(now)
	if !pinned.PinThread(previewThreadID) {
		return nil, errors.New("production reducer rejected the preview pin state")
	}
	if err := appendState(pinned, "PIN"); err != nil {
		return nil, err
	}

	plan := newPreviewReducer(now)
	plan.Apply(previewNotification("turn/plan/updated", `{"threadId":"preview-thread","turnId":"preview-turn","plan":[{"status":"completed"},{"status":"inProgress"},{"status":"pending"}]}`))
	if err := appendState(plan, "PLAN 1/3"); err != nil {
		return nil, err
	}
	plan.Apply(previewNotification("item/completed", fmt.Sprintf(`{"threadId":"preview-thread","turnId":"preview-turn","completedAtMs":%d,"item":{"id":"preview-plan","type":"plan"}}`, now.UnixMilli())))
	if err := appendState(plan, "PLAN READY"); err != nil {
		return nil, err
	}

	for _, request := range []struct {
		id     string
		method string
		state  string
		params string
	}{
		{id: "preview-command", method: "item/commandExecution/requestApproval", state: "WAIT CMD", params: `{"threadId":"preview-thread","turnId":"preview-turn","itemId":"preview-command"}`},
		{id: "preview-file", method: "item/fileChange/requestApproval", state: "WAIT FILE", params: `{"threadId":"preview-thread","turnId":"preview-turn","itemId":"preview-file"}`},
		{id: "preview-permission", method: "item/permissions/requestApproval", state: "WAIT PERM", params: `{"threadId":"preview-thread","turnId":"preview-turn","itemId":"preview-permission","permissions":{"network":{"enabled":true}}}`},
	} {
		reducer := newPreviewReducer(now)
		if err := applyPreviewRequest(reducer, request.id, request.method, request.params); err != nil {
			return nil, err
		}
		if err := appendState(reducer, request.state); err != nil {
			return nil, err
		}
	}

	question := newPreviewReducer(now)
	if err := applyPreviewRequest(question, "preview-question", "item/tool/requestUserInput", `{"threadId":"preview-thread","turnId":"preview-turn","itemId":"preview-question","isBlocking":true,"questions":[{"id":"previews","header":"Previews","question":"Which previews should be refreshed?","options":[{"label":"Codex and Calendar","description":"Update both feature tours"},{"label":"Codex only","description":"Update only the Codex tour"}]}]}`); err != nil {
		return nil, err
	}
	questionCard, err := previewCard(question, "ASK")
	if err != nil {
		return nil, err
	}
	scenes = append(scenes, cardPresentation(questionCard))
	pending, ok := question.PendingRequest(questionCard.Key)
	if !ok {
		return nil, errors.New("production reducer did not retain the preview question")
	}
	detail := (&interactionSession{
		card: questionCard, request: &pending, requestKey: pending.Key, answers: make(map[string]string),
	}).detailCard(now)
	scenes = append(scenes, cardPresentation(detail))

	compaction := newPreviewReducer(now)
	compaction.Apply(previewNotification("item/started", fmt.Sprintf(`{"threadId":"preview-thread","turnId":"preview-turn","startedAtMs":%d,"item":{"id":"preview-compaction","type":"contextCompaction"}}`, now.UnixMilli())))
	if err := appendState(compaction, "COMPACT"); err != nil {
		return nil, err
	}
	compaction.Apply(previewNotification("item/completed", fmt.Sprintf(`{"threadId":"preview-thread","turnId":"preview-turn","completedAtMs":%d,"item":{"id":"preview-compaction","type":"contextCompaction"}}`, now.Add(time.Second).UnixMilli())))
	if err := appendState(compaction, "COMPACTED"); err != nil {
		return nil, err
	}

	for _, outcome := range []struct {
		status string
		state  string
	}{
		{status: "completed", state: "DONE"},
		{status: "interrupted", state: "STOP"},
		{status: "failed", state: "FAIL"},
	} {
		reducer := newPreviewReducer(now)
		reducer.Apply(previewNotification("turn/completed", fmt.Sprintf(`{"threadId":"preview-thread","turn":{"id":"preview-turn","status":%q,"items":[],"completedAt":%d}}`, outcome.status, now.Unix())))
		if err := appendState(reducer, outcome.state); err != nil {
			return nil, err
		}
	}
	return scenes, nil
}

func newPreviewReducer(now time.Time) *Reducer {
	startedAt := now.Add(-time.Second).Unix()
	reducer := NewReducerWithQuota(func() time.Time { return now }, QuotaOptions{})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: previewThreadID, Name: "Preview GIF refresh", CWD: previewProjectDir,
		Status: appserver.ThreadStatus{Type: "active"},
		LatestTurn: &appserver.TurnSnapshot{
			ID: previewTurnID, Status: "inProgress", StartedAt: &startedAt,
		},
	}})
	return reducer
}

func applyPreviewRequest(reducer *Reducer, id, method, params string) error {
	requestID, err := appserver.ParseRawID(json.RawMessage(fmt.Sprintf("%q", id)))
	if err != nil {
		return fmt.Errorf("build preview request ID: %w", err)
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingServerRequest, ID: requestID, Method: method, Params: json.RawMessage(params),
	}})
	return nil
}

func previewNotification(method, params string) appserver.ManagerEvent {
	return appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: method, Params: json.RawMessage(params),
	}}
}

func previewCard(reducer *Reducer, state string) (Card, error) {
	for _, card := range reducer.Cards() {
		if card.StateWord == state {
			return card, nil
		}
	}
	return Card{}, fmt.Errorf("production reducer did not publish preview state %q", state)
}
