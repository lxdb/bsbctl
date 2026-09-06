package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/attention"
	"github.com/lxdb/bsbctl/internal/codexusage"
	"github.com/lxdb/bsbctl/internal/observation"
	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/plugins/codex/internal/appserver"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func NewReducer(now func() time.Time) *Reducer {
	return NewReducerWithQuota(now, QuotaOptions{})
}

func TestReducerPublishesOptInQuotaAndMergesSparseUpdates(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	reducer := NewReducerWithQuota(func() time.Time { return now }, QuotaOptions{
		Enabled: true, AssetPath: "assets/codex-mark.png",
		Presentation: codexusage.PresentationConfig{Label: "MAIN", WarningRemainingPercent: 20, CriticalRemainingPercent: 5},
	})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerRateLimitsSnapshot, RateLimits: &appserver.RateLimitSnapshot{
		LimitID:   "codex",
		Primary:   &appserver.RateLimitWindow{UsedPercent: 13, WindowDurationMinutes: 300, ResetsAt: now.Add(4 * time.Hour).Unix()},
		Secondary: &appserver.RateLimitWindow{UsedPercent: 96, WindowDurationMinutes: 10080, ResetsAt: now.Add(6 * 24 * time.Hour).Unix()},
	}})
	cards := reducer.Cards()
	if !hasCard(cards, ChannelQuotaSummary, "quota-5h") || !hasCard(cards, ChannelQuotaSummary, "quota-1w") || !hasCard(cards, ChannelQuotaPressure, "quota") {
		t.Fatalf("quota cards = %#v", cards)
	}
	pressure := findCard(t, cards, ChannelQuotaPressure, "quota")
	if pressure.Disposition != protocol.DispositionActionable || pressure.Impact != protocol.ImpactCritical || pressure.ReasonCode != "codex_quota_critical" {
		t.Fatalf("critical quota card = %#v", pressure)
	}
	mark := sceneElement(t, pressure.Scene, "front-codex-mark")
	if mark.Image == nil || mark.Image.Asset.PackagePath != "assets/codex-mark.png" {
		t.Fatalf("quota mark = %#v", mark)
	}
	weekly := findCard(t, cards, ChannelQuotaSummary, "quota-1w")
	if reset := sceneElement(t, weekly.Scene, "back-window-1-reset"); reset.Text == nil || reset.Text.Value != "RESET IN 6D" {
		t.Fatalf("weekly reset text = %#v", reset)
	}
	if countdown := sceneElement(t, weekly.Scene, "back-window-1-reset-countdown"); countdown.Countdown == nil || countdown.Countdown.Color != codexusage.CanvasColor {
		t.Fatalf("weekly hidden countdown = %#v", countdown)
	}

	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "account/rateLimits/updated",
		Params: json.RawMessage(`{"rateLimits":{"limitId":"codex","secondary":{"usedPercent":50}}}`),
	}})
	cards = reducer.Cards()
	if hasCard(cards, ChannelQuotaPressure, "quota") || !hasCard(cards, ChannelQuotaSummary, "quota-1w") {
		t.Fatalf("recovered sparse quota cards = %#v", cards)
	}
	weekly = findCard(t, cards, ChannelQuotaSummary, "quota-1w")
	if value := sceneElement(t, weekly.Scene, "front-window-value").Text.Value; value != "50%" {
		t.Fatalf("weekly remaining = %q, want 50%%", value)
	}

	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerDisconnected})
	for _, card := range reducer.Cards() {
		if card.Channel == ChannelQuotaSummary || card.Channel == ChannelQuotaPressure {
			t.Fatalf("quota survived disconnect: %#v", reducer.Cards())
		}
	}
}

func TestReducerQuotaPressureIsBriefAndRearmsOnlyOnSignalChange(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	reducer := NewReducerWithQuota(func() time.Time { return now }, QuotaOptions{
		Enabled: true,
		Presentation: codexusage.PresentationConfig{
			WarningRemainingPercent:  20,
			CriticalRemainingPercent: 5,
		},
	})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerRateLimitsSnapshot, RateLimits: &appserver.RateLimitSnapshot{
		LimitID: "codex",
		Primary: &appserver.RateLimitWindow{UsedPercent: 85, WindowDurationMinutes: 300, ResetsAt: now.Add(time.Hour).Unix()},
	}})
	pressure := findCard(t, reducer.Cards(), ChannelQuotaPressure, "quota")
	if pressure.Impact != protocol.ImpactLow {
		t.Fatalf("low quota impact = %q, want low", pressure.Impact)
	}
	firstUntil := pressure.ValidUntil

	now = now.Add(10 * time.Second)
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "account/rateLimits/updated",
		Params: json.RawMessage(`{"rateLimits":{"limitId":"codex","primary":{"usedPercent":86}}}`),
	}})
	if got := findCard(t, reducer.Cards(), ChannelQuotaPressure, "quota").ValidUntil; !got.Equal(firstUntil) {
		t.Fatalf("same low signal renewed pressure until %v, want %v", got, firstUntil)
	}
	now = firstUntil
	if hasCard(reducer.Cards(), ChannelQuotaPressure, "quota") {
		t.Fatal("unchanged low quota remained continuously eligible")
	}

	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "account/rateLimits/updated",
		Params: json.RawMessage(`{"rateLimits":{"limitId":"codex","primary":{"usedPercent":96}}}`),
	}})
	critical := findCard(t, reducer.Cards(), ChannelQuotaPressure, "quota")
	if critical.Disposition != protocol.DispositionActionable || critical.Impact != protocol.ImpactCritical || !critical.ValidUntil.After(now) {
		t.Fatalf("critical transition = %#v", critical)
	}
}

func TestNormalAppServerOutcomeOutranksLowQuotaEpisode(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	reducer := NewReducerWithQuota(func() time.Time { return now }, QuotaOptions{
		Enabled: true,
		Presentation: codexusage.PresentationConfig{
			WarningRemainingPercent:  20,
			CriticalRemainingPercent: 5,
		},
	})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerRateLimitsSnapshot, RateLimits: &appserver.RateLimitSnapshot{
		LimitID: "codex",
		Primary: &appserver.RateLimitWindow{UsedPercent: 85, WindowDurationMinutes: 300, ResetsAt: now.Add(time.Hour).Unix()},
	}})
	pressure := findCard(t, reducer.Cards(), ChannelQuotaPressure, "quota")
	outcome := Card{
		Channel: ChannelOutcome, Key: "outcome", Disposition: protocol.DispositionNotable, Impact: protocol.ImpactNormal,
		ReasonCode: "codex_completed", ObservedAt: now, ValidUntil: now.Add(30 * time.Second), Scene: protocol.Scene{Elements: []protocol.Element{{
			ID: "state", Display: protocol.DisplayFront, Text: &protocol.TextElement{Value: "DONE", Font: "tiny"},
		}}},
	}
	records := make([]observation.Record, 0, 2)
	for index, card := range []Card{pressure, outcome} {
		records = append(records, observation.Record{PluginID: PluginID, Generation: 1, AdmissionSequence: uint64(index + 1), Observation: protocol.Observation{
			Instance: protocol.InstanceRef{ID: AppID, Generation: 1}, Channel: card.Channel, Key: card.Key, Revision: 1,
			Disposition: card.Disposition, Impact: card.Impact, ReasonCode: card.ReasonCode,
			ObservedAt: card.ObservedAt, UpdatedAt: now, ValidUntil: card.ValidUntil, Scene: new(cardScene(card)),
		}})
	}
	decision := attention.Select(records, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyWhenRelevant}, true
	}, presentation.History{LastShown: map[string]time.Time{}}, now)
	if decision.Candidate == nil || decision.Candidate.Channel != ChannelOutcome {
		t.Fatalf("selected = %#v, evaluations = %#v", decision.Candidate, decision.Evaluations)
	}
}

func TestQuotaReadFailuresAndUnrelatedEventsDoNotRenewFreshness(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	reducer := NewReducerWithQuota(func() time.Time { return now }, QuotaOptions{Enabled: true})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerRateLimitsSnapshot, RateLimits: &appserver.RateLimitSnapshot{
		Primary: &appserver.RateLimitWindow{UsedPercent: 40, WindowDurationMinutes: 300, ResetsAt: now.Add(time.Hour).Unix()},
	}})
	expires := findCard(t, reducer.Cards(), ChannelQuotaSummary, "quota-5h").ValidUntil
	for range 2 {
		now = now.Add(2 * time.Minute)
		reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerRateLimitsReadFailed})
		reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadsReconciled})
		if card := findCard(t, reducer.Cards(), ChannelQuotaSummary, "quota-5h"); !card.ValidUntil.Equal(expires) {
			t.Fatalf("failed refresh renewed stale quota until %v (original %v)", card.ValidUntil, expires)
		}
	}
	now = expires
	if hasCard(reducer.Cards(), ChannelQuotaSummary, "quota-5h") {
		t.Fatal("expired quota remains publishable")
	}
}

func TestReducerClassifiesTypedPendingRequests(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		method string
		params string
		word   string
	}{
		{method: "item/commandExecution/requestApproval", params: `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1770000000000}`, word: "WAIT CMD"},
		{method: "item/fileChange/requestApproval", params: `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1770000000000}`, word: "WAIT FILE"},
		{method: "item/permissions/requestApproval", params: `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1770000000000,"cwd":"/hidden","permissions":{}}`, word: "WAIT PERM"},
		{method: "item/tool/requestUserInput", params: `{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","isBlocking":true,"questions":[{"id":"choice","header":"Choice","question":"Choose","isSecret":false,"isOther":false,"options":[{"label":"A","description":"First"}]}]}`, word: "ASK"},
	}
	for index, test := range tests {
		t.Run(test.word, func(t *testing.T) {
			reducer := NewReducer(func() time.Time { return now })
			reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
			id, err := appserver.ParseRawID(json.RawMessage(`"request-` + string(rune('1'+index)) + `"`))
			if err != nil {
				t.Fatal(err)
			}
			reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
				Kind: appserver.IncomingServerRequest, ID: id, Method: test.method, Params: json.RawMessage(test.params),
			}})
			if !hasStateWord(reducer.Cards(), test.word) {
				t.Fatalf("cards = %#v, want %q", reducer.Cards(), test.word)
			}
		})
	}
}

func TestReducerRemovesOnlyTheResolvedServerRequest(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	for _, requestID := range []string{"first", "second"} {
		id, err := appserver.ParseRawID(json.RawMessage(`"` + requestID + `"`))
		if err != nil {
			t.Fatal(err)
		}
		reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
			Kind: appserver.IncomingServerRequest, ID: id, Method: "item/fileChange/requestApproval",
			Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1770000000000}`),
		}})
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "serverRequest/resolved",
		Params: json.RawMessage(`{"threadId":"thread-1","requestId":"first"}`),
	}})

	count := 0
	for _, card := range reducer.Cards() {
		if card.StateWord == "WAIT FILE" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("pending file cards = %d, want 1; cards = %#v", count, reducer.Cards())
	}
}

func TestReducerUsesExactThreadStateAndActiveFlagFallbacks(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		flags []string
		word  string
	}{
		{name: "running", word: "RUN"},
		{name: "approval fallback", flags: []string{"waitingOnApproval"}, word: "WAIT"},
		{name: "question fallback", flags: []string{"waitingOnUserInput"}, word: "OPEN CODEX"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			startedAt := now.Unix()
			reducer := NewReducer(func() time.Time { return now })
			reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
			reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
				ID: "thread-1", Name: "Safe title", CWD: "/private/hidden/project",
				Status:     appserver.ThreadStatus{Type: "active", ActiveFlags: test.flags},
				LatestTurn: &appserver.TurnSnapshot{ID: "turn-1", Status: "inProgress", StartedAt: &startedAt},
			}})
			var selected *Card
			for _, card := range reducer.Cards() {
				if card.StateWord == test.word {
					copy := card
					selected = &copy
				}
			}
			if selected == nil {
				t.Fatalf("cards = %#v, want %q", reducer.Cards(), test.word)
			}
			if selected.ContextLine != "Safe title" || selected.ContextLine == "/private/hidden/project" {
				t.Fatalf("context = %q", selected.ContextLine)
			}
		})
	}
}

func TestReducerSeparatesStatusOnlyOpenCodexFromExactTypedAsk(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Name: "Safe session", CWD: "/private/Safe project",
		Status: appserver.ThreadStatus{Type: "active", ActiveFlags: []string{"waitingOnUserInput"}},
	}})

	statusOnly := findStateCard(t, reducer.Cards(), "OPEN CODEX")
	if statusOnly.Channel != "guidance" || statusOnly.Disposition != protocol.DispositionNotable || statusOnly.Impact != protocol.ImpactNotable || statusOnly.DetailLine != "Use Codex" {
		t.Fatalf("status-only card = %#v", statusOnly)
	}

	id, _ := appserver.ParseRawID(json.RawMessage(`"question-1"`))
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingServerRequest, ID: id, Method: "item/tool/requestUserInput",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","isBlocking":true,"questions":[{"id":"choice","header":"Choice","question":"Choose","isSecret":false,"isOther":false,"options":[{"label":"A","description":"First"}]}]}`),
	}})
	exact := findStateCard(t, reducer.Cards(), "ASK")
	if exact.Channel != ChannelAttention || exact.Disposition != protocol.DispositionActionable || exact.DetailLine != "START TO ANSWER" {
		t.Fatalf("exact ASK card = %#v", exact)
	}
}

func TestReducerStatusGuidanceCrossesWhenRelevantSelectorBoundary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Name: "Safe session", CWD: "/private/Safe project",
		Status: appserver.ThreadStatus{Type: "active", ActiveFlags: []string{"waitingOnUserInput"}},
	}})
	card := findStateCard(t, reducer.Cards(), "OPEN CODEX")
	record := observation.Record{PluginID: PluginID, Generation: 1, Observation: protocol.Observation{
		Instance: protocol.InstanceRef{ID: AppID, Generation: 1}, Channel: card.Channel, Key: card.Key, Revision: 1,
		Disposition: card.Disposition, Impact: card.Impact, ReasonCode: card.ReasonCode,
		ObservedAt: card.ObservedAt, UpdatedAt: now, ValidUntil: card.ValidUntil, Scene: new(cardScene(card)),
	}}
	decision := attention.Select([]observation.Record{record}, func(observation.Record) (attention.Rule, bool) {
		return attention.Rule{Enabled: true, AssetsReady: true, Policy: presentation.PolicyWhenRelevant}, true
	}, presentation.History{LastShown: map[string]time.Time{}}, now)
	if decision.Candidate == nil || decision.Candidate.Channel != "guidance" {
		t.Fatalf("selected guidance = %#v, evaluations = %#v", decision.Candidate, decision.Evaluations)
	}
}

func TestReducerDoesNotInventRunFromThreadStatusChanges(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Name: "Safe title", Status: appserver.ThreadStatus{Type: "idle"},
	}})
	if hasStateWord(reducer.Cards(), "RUN") {
		t.Fatal("idle thread published RUN")
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "thread/status/changed",
		Params: json.RawMessage(`{"threadId":"thread-1","status":{"type":"active","activeFlags":[]}}`),
	}})
	if hasStateWord(reducer.Cards(), "RUN") {
		t.Fatalf("status-only active cards = %#v", reducer.Cards())
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/started",
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"inProgress","items":[]}}`),
	}})
	if !hasStateWord(reducer.Cards(), "RUN") {
		t.Fatalf("turn-start cards = %#v", reducer.Cards())
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "thread/status/changed",
		Params: json.RawMessage(`{"threadId":"thread-1","status":{"type":"idle"}}`),
	}})
	if !hasStateWord(reducer.Cards(), "RUN") {
		t.Fatalf("status change overrode canonical in-progress turn: %#v", reducer.Cards())
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/completed",
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[]}}`),
	}})
	if hasStateWord(reducer.Cards(), "RUN") {
		t.Fatalf("terminal-turn cards = %#v", reducer.Cards())
	}
}

func TestReducerPublishesPlanProgressAndCompletedPlanItem(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Name: "Safe title", Status: appserver.ThreadStatus{Type: "active"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-1", Status: "inProgress"},
	}})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/plan/updated",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","plan":[{"step":"secret text one","status":"completed"},{"step":"secret text two","status":"inProgress"},{"step":"secret text three","status":"pending"}]}`),
	}})
	if !hasStateWord(reducer.Cards(), "PLAN 1/3") {
		t.Fatalf("plan progress cards = %#v", reducer.Cards())
	}
	progress := findStateCard(t, reducer.Cards(), "PLAN 1/3")
	if progress.Channel != ChannelProgress || progress.Disposition != protocol.DispositionNotable {
		t.Fatalf("plan progress policy card = %#v", progress)
	}
	for _, card := range reducer.Cards() {
		if card.ContextLine == "secret text one" || card.DetailLine == "secret text two" {
			t.Fatal("plan text leaked into a card")
		}
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "item/completed",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":1770000000000,"item":{"id":"plan-1","type":"plan","text":"secret plan"}}`),
	}})
	if !hasStateWord(reducer.Cards(), "PLAN READY") {
		t.Fatalf("plan ready cards = %#v", reducer.Cards())
	}
}

func TestReducerKeepsPlanReadyOverDoneForCompletedPlanTurn(t *testing.T) {
	now := time.Date(2026, 9, 3, 22, 43, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Name: "Safe title", Status: appserver.ThreadStatus{Type: "active"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-1", Status: "inProgress"},
	}})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "item/completed",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":1788494580000,"item":{"id":"plan-1","type":"plan","text":"secret plan"}}`),
	}})
	completedAt := now.Unix()
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/completed",
		Params: json.RawMessage(fmt.Sprintf(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[{"id":"plan-1","type":"plan","status":"completed"}],"completedAt":%d}}`, completedAt)),
	}})

	ready := findStateCard(t, reducer.Cards(), "PLAN READY")
	if hasStateWord(reducer.Cards(), "DONE") {
		t.Fatalf("completed plan exposed DONE alongside PLAN READY: %#v", reducer.Cards())
	}
	if !ready.ObservedAt.Equal(now) || !ready.ValidUntil.Equal(now.Add(30*time.Second)) {
		t.Fatalf("PLAN READY timing = %#v, want fixed 30-second terminal outcome", ready)
	}
	now = now.Add(30*time.Second - time.Millisecond)
	refreshed := findStateCard(t, reducer.Cards(), "PLAN READY")
	if !refreshed.ObservedAt.Equal(ready.ObservedAt) || !refreshed.ValidUntil.Equal(ready.ValidUntil) {
		t.Fatalf("PLAN READY lifetime was renewed: first=%#v refreshed=%#v", ready, refreshed)
	}
	now = now.Add(time.Millisecond)
	if hasStateWord(reducer.Cards(), "PLAN READY") || hasStateWord(reducer.Cards(), "DONE") {
		t.Fatalf("expired PLAN READY revealed a terminal fallback: %#v", reducer.Cards())
	}
}

func TestReducerDoesNotApplyCompletedPlanToAnotherTurn(t *testing.T) {
	now := time.Date(2026, 9, 3, 22, 43, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Status: appserver.ThreadStatus{Type: "active"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-current", Status: "inProgress"},
	}})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "item/completed",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-old","completedAtMs":1788494580000,"item":{"id":"plan-old","type":"plan"}}`),
	}})
	if hasStateWord(reducer.Cards(), "PLAN READY") {
		t.Fatalf("stale plan completion was published for current turn: %#v", reducer.Cards())
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/completed",
		Params: json.RawMessage(fmt.Sprintf(`{"threadId":"thread-1","turn":{"id":"turn-current","status":"completed","items":[],"completedAt":%d}}`, now.Unix())),
	}})

	if !hasStateWord(reducer.Cards(), "DONE") || hasStateWord(reducer.Cards(), "PLAN READY") {
		t.Fatalf("stale plan completion changed current turn outcome: %#v", reducer.Cards())
	}
}

func TestReducerRecognizesPlanFromCompletedTurnSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 3, 22, 43, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/completed",
		Params: json.RawMessage(fmt.Sprintf(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"completed","items":[{"id":"plan-1","type":"plan","status":"completed"}],"completedAt":%d}}`, now.Unix())),
	}})

	if !hasStateWord(reducer.Cards(), "PLAN READY") || hasStateWord(reducer.Cards(), "DONE") {
		t.Fatalf("completed turn snapshot did not preserve plan outcome: %#v", reducer.Cards())
	}
}

func TestReducerDoesNotRestorePlanReadyFromUnsuccessfulTurn(t *testing.T) {
	now := time.Date(2026, 9, 3, 22, 43, 0, 0, time.UTC)
	completedAt := now.Unix()
	for status, want := range map[string]string{"failed": "FAIL", "interrupted": "STOP"} {
		t.Run(status, func(t *testing.T) {
			reducer := NewReducer(func() time.Time { return now })
			reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
			reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
				ID: "thread-1", Status: appserver.ThreadStatus{Type: "idle"},
				LatestTurn: &appserver.TurnSnapshot{
					ID: "turn-1", Status: status, CompletedAt: &completedAt,
					Items: []appserver.ItemSnapshot{{ID: "plan-1", Type: "plan", Status: "completed"}},
				},
			}})

			if !hasStateWord(reducer.Cards(), want) || hasStateWord(reducer.Cards(), "PLAN READY") {
				t.Fatalf("%s plan turn cards = %#v, want only %s outcome", status, reducer.Cards(), want)
			}
		})
	}
}

func TestReducerNewTurnClearsPlanReadyOutcome(t *testing.T) {
	now := time.Date(2026, 9, 3, 22, 43, 0, 0, time.UTC)
	completedAt := now.Unix()
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Status: appserver.ThreadStatus{Type: "idle"},
		LatestTurn: &appserver.TurnSnapshot{
			ID: "turn-1", Status: "completed", CompletedAt: &completedAt,
			Items: []appserver.ItemSnapshot{{ID: "plan-1", Type: "plan", Status: "completed"}},
		},
	}})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/started",
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-2","status":"inProgress","items":[]}}`),
	}})

	if !hasStateWord(reducer.Cards(), "RUN") || hasStateWord(reducer.Cards(), "PLAN READY") || hasStateWord(reducer.Cards(), "DONE") {
		t.Fatalf("new turn retained completed plan outcome: %#v", reducer.Cards())
	}
}

func TestReducerMapsExactTurnCompletionStatuses(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	for status, word := range map[string]string{"completed": "DONE", "interrupted": "STOP", "failed": "FAIL"} {
		t.Run(status, func(t *testing.T) {
			reducer := NewReducer(func() time.Time { return now })
			reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
			reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
				ID: "thread-1", Name: "Safe title", Status: appserver.ThreadStatus{Type: "active"},
				LatestTurn: &appserver.TurnSnapshot{ID: "turn-1", Status: "inProgress"},
			}})
			payload := fmt.Sprintf(`{"threadId":"thread-1","turn":{"id":"turn-1","status":%q,"items":[],"startedAt":%d,"completedAt":%d,"durationMs":1000,"error":{"message":"secret failure"}}}`, status, now.Add(-time.Second).Unix(), now.Unix())
			reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
				Kind: appserver.IncomingNotification, Method: "turn/completed", Params: json.RawMessage(payload),
			}})
			if !hasStateWord(reducer.Cards(), word) {
				t.Fatalf("completion cards = %#v, want %q", reducer.Cards(), word)
			}
			for _, card := range reducer.Cards() {
				if card.ContextLine == "secret failure" || card.DetailLine == "secret failure" {
					t.Fatal("turn error text leaked into a card")
				}
			}
		})
	}
}

func TestReducerTurnStartedReplacesPriorTerminalState(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	completedAt := now.Unix()
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Name: "Safe title", Status: appserver.ThreadStatus{Type: "idle"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-1", Status: "completed", CompletedAt: &completedAt},
	}})
	if !hasStateWord(reducer.Cards(), "DONE") {
		t.Fatalf("initial cards = %#v", reducer.Cards())
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/started",
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-2","status":"inProgress","items":[]}}`),
	}})
	if !hasStateWord(reducer.Cards(), "RUN") || hasStateWord(reducer.Cards(), "DONE") {
		t.Fatalf("new turn cards = %#v", reducer.Cards())
	}
}

func TestReducerPublishesOneBoundedRunCuePerTurn(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/started",
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"inProgress","items":[],"startedAt":1787486400}}`),
	}})
	first := findStateCard(t, reducer.Cards(), "RUN")
	if !first.ObservedAt.Equal(now) || !first.ValidUntil.Equal(now.Add(runVisibilityWindow)) {
		t.Fatalf("first RUN timing = %#v", first)
	}
	if threadID, turnID, ok := reducer.InterruptTarget(first.Key); !ok || threadID != "thread-1" || turnID != "turn-1" {
		t.Fatalf("first RUN target = %q/%q/%v", threadID, turnID, ok)
	}

	now = now.Add(runVisibilityWindow - time.Millisecond)
	refreshed := findStateCard(t, reducer.Cards(), "RUN")
	if refreshed.Key != first.Key || !refreshed.ValidUntil.Equal(first.ValidUntil) {
		t.Fatalf("RUN lifetime was refreshed: first=%#v refreshed=%#v", first, refreshed)
	}
	now = now.Add(time.Millisecond)
	if hasStateWord(reducer.Cards(), "RUN") {
		t.Fatalf("expired RUN cards = %#v", reducer.Cards())
	}

	now = now.Add(time.Second)
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/started",
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-2","status":"inProgress","items":[]}}`),
	}})
	second := findStateCard(t, reducer.Cards(), "RUN")
	if second.Key == first.Key || !second.ObservedAt.Equal(now) || !second.ValidUntil.Equal(now.Add(runVisibilityWindow)) {
		t.Fatalf("second RUN = %#v; first = %#v", second, first)
	}
}

func TestReducerLiveCardFollowsLatestValidThreadBeyondBackgroundRunWindow(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-time.Minute).Unix()
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-one", Name: "First session", CWD: "/safe/first",
		Status: appserver.ThreadStatus{Type: "active"}, LatestTurn: &appserver.TurnSnapshot{ID: "turn-one", Status: "inProgress", StartedAt: &startedAt},
	}})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-two", Name: "Latest session", CWD: "/safe/latest",
		Status: appserver.ThreadStatus{Type: "active"}, LatestTurn: &appserver.TurnSnapshot{ID: "turn-two", Status: "inProgress", StartedAt: &startedAt},
	}})
	if hasStateWord(reducer.Cards(), "RUN") {
		t.Fatalf("expired runs leaked into background cards: %#v", reducer.Cards())
	}
	live := reducer.LiveCard()
	if live.StateWord != "RUN" || live.SessionLine != "Latest session" || live.ProjectLine != "latest" {
		t.Fatalf("live card = %#v, want latest thread run", live)
	}

	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/started",
		Params: json.RawMessage(`{"threadId":"thread-one","turn":{"id":"","status":"inProgress","items":[]}}`),
	}})
	if afterMalformed := reducer.LiveCard(); afterMalformed.SessionLine != "Latest session" {
		t.Fatalf("malformed event changed live focus: %#v", afterMalformed)
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/started",
		Params: json.RawMessage(`{"threadId":"thread-one","turn":{"id":"turn-three","status":"inProgress","items":[]}}`),
	}})
	if afterValid := reducer.LiveCard(); afterValid.SessionLine != "First session" || afterValid.StateWord != "RUN" {
		t.Fatalf("valid event did not move live focus: %#v", afterValid)
	}

	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerDisconnected})
	if disconnected := reducer.LiveCard(); disconnected.Channel != ChannelConnection || disconnected.StateWord != "CODEX ..." {
		t.Fatalf("disconnected live card = %#v", disconnected)
	}
}

func TestReducerLiveCardUsesLatestPendingRequestArrivalInsteadOfRequestIDOrder(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})

	olderID, err := appserver.ParseRawID(json.RawMessage(`9`))
	if err != nil {
		t.Fatal(err)
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingServerRequest, ID: olderID, Method: "item/fileChange/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"file-1"}`),
	}})

	newerID, err := appserver.ParseRawID(json.RawMessage(`10`))
	if err != nil {
		t.Fatal(err)
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingServerRequest, ID: newerID, Method: "item/commandExecution/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"command-1"}`),
	}})

	if got := reducer.LiveCard().StateWord; got != "WAIT CMD" {
		t.Fatalf("live card = %q, want latest pending request WAIT CMD", got)
	}
}

func TestReducerDoesNotReplayExpiredRunFromRecoveredSnapshot(t *testing.T) {
	startedAt := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	now := startedAt.Add(time.Minute)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	startedUnix := startedAt.Unix()
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Status: appserver.ThreadStatus{Type: "active"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-1", Status: "inProgress", StartedAt: &startedUnix},
	}})
	if hasStateWord(reducer.Cards(), "RUN") {
		t.Fatalf("recovered old RUN cards = %#v", reducer.Cards())
	}
}

func TestReducerPublishesAuthoritativeThreadSystemError(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Name: "Safe title", Status: appserver.ThreadStatus{Type: "systemError"},
	}})
	if !hasStateWord(reducer.Cards(), "FAIL") {
		t.Fatalf("system error cards = %#v", reducer.Cards())
	}
}

func TestReducerPublishesActiveThreadCountOverview(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	for _, threadID := range []string{"thread-1", "thread-2"} {
		reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
			ID: threadID, Status: appserver.ThreadStatus{Type: "active"},
		}})
	}
	if !hasStateWord(reducer.Cards(), "2 ACT") {
		t.Fatalf("overview cards = %#v", reducer.Cards())
	}
}

func TestReducerRemovesStateForThreadsNoLongerLoaded(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Status: appserver.ThreadStatus{Type: "active"},
	}})
	id, err := appserver.ParseRawID(json.RawMessage(`"request-1"`))
	if err != nil {
		t.Fatal(err)
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingServerRequest, ID: id, Method: "item/fileChange/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1770000000000}`),
	}})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadsReconciled, ThreadIDs: []string{}})
	if hasStateWord(reducer.Cards(), "RUN") || hasStateWord(reducer.Cards(), "WAIT FILE") {
		t.Fatalf("removed thread cards = %#v", reducer.Cards())
	}
}

func TestReducerRetainsSafeStateDuringReconnectGraceThenShowsProviderUnavailable(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	startedAt := now.Unix()
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Status: appserver.ThreadStatus{Type: "active"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-1", Status: "inProgress", StartedAt: &startedAt},
	}})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerDisconnected})
	cards := reducer.Cards()
	if !hasStateWord(cards, "CODEX ...") || !hasStateWord(cards, "RUN") || hasStateWord(cards, "CODEX OFF") {
		t.Fatalf("reconnect-grace cards = %#v", cards)
	}
	now = now.Add(reconnectGrace)
	cards = reducer.Cards()
	if len(cards) != 1 || cards[0].StateWord != "CODEX OFF" {
		t.Fatalf("expired reconnect cards = %#v", cards)
	}
}

func TestReducerReconnectGraceClearsUnsafeStateAndDoesNotSlide(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	reducer := NewReducerWithQuota(func() time.Time { return now }, QuotaOptions{Enabled: true})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/started",
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","status":"inProgress","items":[]}}`),
	}})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/plan/updated",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","plan":[{"status":"inProgress"}]}`),
	}})
	id, err := appserver.ParseRawID(json.RawMessage(`"request-1"`))
	if err != nil {
		t.Fatal(err)
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingServerRequest, ID: id, Method: "item/fileChange/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1"}`),
	}})

	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerDisconnected})
	deadline, ok := reducer.ReconnectDeadline()
	if !ok || !deadline.Equal(now.Add(reconnectGrace)) {
		t.Fatalf("reconnect deadline = %v/%v", deadline, ok)
	}
	if cards := reducer.Cards(); hasStateWord(cards, "WAIT FILE") || !hasStateWord(cards, "PLAN 0/1") || !hasStateWord(cards, "CODEX ...") {
		t.Fatalf("reconnect cards = %#v", cards)
	}

	now = now.Add(2 * time.Second)
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerDisconnected})
	if repeated, ok := reducer.ReconnectDeadline(); !ok || !repeated.Equal(deadline) {
		t.Fatalf("repeated disconnect moved deadline = %v/%v, want %v", repeated, ok, deadline)
	}
}

func TestReducerClearsPendingRequestsBeforeReconnectReplay(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	id, err := appserver.ParseRawID(json.RawMessage(`"request-1"`))
	if err != nil {
		t.Fatal(err)
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingServerRequest, ID: id, Method: "item/fileChange/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1"}`),
	}})
	if !hasStateWord(reducer.Cards(), "WAIT FILE") {
		t.Fatalf("pending cards = %#v", reducer.Cards())
	}

	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerDisconnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	if hasStateWord(reducer.Cards(), "WAIT FILE") {
		t.Fatalf("stale request survived reconnect = %#v", reducer.Cards())
	}
}

func TestReducerProjectsOnlyBoundedSupportedRequestActions(t *testing.T) {
	reducer := NewReducer(time.Now)
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	id, err := appserver.ParseRawID(json.RawMessage(`"request-actions"`))
	if err != nil {
		t.Fatal(err)
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingServerRequest, ID: id, Method: "item/commandExecution/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","startedAtMs":1,"availableDecisions":["accept","acceptForSession","decline","cancel",{"applyNetworkPolicyAmendment":{}}]}`),
	}})
	request, ok := reducer.PendingRequest(observationKey("request", id.Key()))
	if !ok {
		t.Fatal("pending request was not retained")
	}
	want := []string{"accept", "decline", "cancel"}
	if fmt.Sprint(request.Actions) != fmt.Sprint(want) {
		t.Fatalf("actions = %v, want %v", request.Actions, want)
	}
}

func TestReducerRejectsSecretOrFreeformOnlyQuestionControls(t *testing.T) {
	for name, question := range map[string]string{
		"secret":   `{"id":"q","header":"Choice","question":"Choose","isSecret":true,"options":[{"label":"A","description":"First"}]}`,
		"freeform": `{"id":"q","header":"Choice","question":"Choose"}`,
	} {
		t.Run(name, func(t *testing.T) {
			reducer := NewReducer(time.Now)
			reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
			id, _ := appserver.ParseRawID(json.RawMessage(`"question"`))
			reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
				Kind: appserver.IncomingServerRequest, ID: id, Method: "item/tool/requestUserInput",
				Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","questions":[` + question + `],"isBlocking":true}`),
			}})
			request, ok := reducer.PendingRequest(observationKey("request", id.Key()))
			if !ok || request.Interactive {
				t.Fatalf("unsafe question request = %#v/%v", request, ok)
			}
		})
	}
}

func TestReducerPublishesUnsupportedTypedQuestionAsGuidance(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	id, _ := appserver.ParseRawID(json.RawMessage(`"question"`))
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingServerRequest, ID: id, Method: "item/tool/requestUserInput",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","itemId":"item-1","isBlocking":true,"questions":[{"id":"q","header":"Choice","question":"Choose","isSecret":true,"options":[{"label":"A"}]}]}`),
	}})
	card := findStateCard(t, reducer.Cards(), "OPEN CODEX")
	if card.Channel != "guidance" || card.Disposition != protocol.DispositionNotable || card.Impact != protocol.ImpactNotable || card.ReasonCode != "codex_wait_question" {
		t.Fatalf("unsupported question guidance = %#v", card)
	}
}

func TestReducerRejectsQuestionControlsThatCannotBeSafelyRoundTripped(t *testing.T) {
	for name, question := range map[string]string{
		"long id":    `{"id":"` + strings.Repeat("q", 129) + `","header":"Choice","question":"Choose","options":[{"label":"A"}]}`,
		"long label": `{"id":"q","header":"Choice","question":"Choose","options":[{"label":"` + strings.Repeat("A", 257) + `"}]}`,
		"control":    `{"id":"q","header":"Choice","question":"Choose","options":[{"label":"A\nB"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			questions, interactive := projectQuestions(json.RawMessage(`{"questions":[` + question + `],"isBlocking":true}`))
			if interactive || questions != nil {
				t.Fatalf("unsafe projected questions = %#v/%v", questions, interactive)
			}
		})
	}
}

func TestThreadIdentityUsesSanitizedSessionAndProjectWithoutPreviewOrFullPath(t *testing.T) {
	t.Parallel()

	session, project := threadIdentity(&threadState{
		Name:    "  Session\nName  ",
		Preview: "Forbidden preview",
		CWD:     "/private/hidden/My Project",
	})
	if session != "Session Name" || project != "My Project" {
		t.Fatalf("identity = %q / %q", session, project)
	}

	for name, thread := range map[string]*threadState{
		"empty":        {},
		"root":         {Name: "\x00", Preview: "Forbidden preview", CWD: "/"},
		"relative cwd": {CWD: "private/hidden/project"},
		"invalid cwd":  {CWD: "/private/hidden/\x00project"},
	} {
		t.Run(name, func(t *testing.T) {
			session, project := threadIdentity(thread)
			if session != "Codex session" || project != "Project" {
				t.Fatalf("identity = %q / %q", session, project)
			}
		})
	}

	session, project = threadIdentity(&threadState{Name: "Same", CWD: "/work/Same"})
	if session != "Same" || project != "Same" {
		t.Fatalf("equal identity was deduplicated: %q / %q", session, project)
	}
}

func TestThreadBoundCardsCarrySeparateSessionAndProjectIdentity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	completedAt := now.Unix()
	thread := &threadState{
		ID: "thread-1", Name: "Safe session", Preview: "Forbidden preview", CWD: "/private/hidden/Safe project",
		Status:     appserver.ThreadStatus{Type: "active"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-1", Status: "completed", CompletedAt: &completedAt},
		OutcomeAt:  now, PlanTotal: 2, PlanDone: 1,
	}
	request := &pendingRequest{Kind: requestQuestion, Key: "request.1", StartedAt: now, Interactive: true}

	requestValue := requestCard(request, thread, now)
	outcomeValue, _ := outcomeCard(thread, now)
	progressValue, _ := planCard(thread, now)
	for name, card := range map[string]Card{
		"request":  requestValue,
		"outcome":  outcomeValue,
		"progress": progressValue,
	} {
		t.Run(name, func(t *testing.T) {
			if card.SessionLine != "Safe session" || card.ProjectLine != "Safe project" {
				t.Fatalf("card identity = %q / %q", card.SessionLine, card.ProjectLine)
			}
		})
	}
}

func TestThreadSummariesAreBoundedAndSkipUnsafeIdentifiers(t *testing.T) {
	reducer := NewReducer(time.Now)
	for index := 0; index < 140; index++ {
		id := fmt.Sprintf("thread-%03d", index)
		reducer.threads[id] = &threadState{ID: id, Name: "Safe", Status: appserver.ThreadStatus{Type: "active"}}
	}
	reducer.threads[strings.Repeat("x", 129)] = &threadState{ID: strings.Repeat("x", 129), Name: "Unsafe", Status: appserver.ThreadStatus{Type: "active"}}
	summaries := reducer.ThreadSummaries()
	if len(summaries) != 128 {
		t.Fatalf("thread summaries = %d, want 128", len(summaries))
	}
	for _, summary := range summaries {
		if len(summary.ThreadID) > 128 {
			t.Fatalf("unsafe thread summary = %#v", summary)
		}
	}
}

func TestReducerRestoresCompletedPlanItemFromThreadSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	completedAt := now.Unix()
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Name: "Safe title", Status: appserver.ThreadStatus{Type: "idle"},
		LatestTurn: &appserver.TurnSnapshot{
			ID: "turn-1", Status: "completed", CompletedAt: &completedAt,
			Items: []appserver.ItemSnapshot{{ID: "plan-1", Type: "plan", Status: "completed"}},
		},
	}})
	if !hasStateWord(reducer.Cards(), "PLAN READY") || hasStateWord(reducer.Cards(), "DONE") {
		t.Fatalf("snapshot cards = %#v", reducer.Cards())
	}
	now = now.Add(30 * time.Second)
	if hasStateWord(reducer.Cards(), "PLAN READY") || hasStateWord(reducer.Cards(), "DONE") {
		t.Fatalf("expired snapshot outcome cards = %#v", reducer.Cards())
	}
}

func TestReducerReportsCanonicalCompactionLifecycleAndExpiresOutcome(t *testing.T) {
	now := time.Date(2026, 8, 27, 16, 30, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Name: "Safe title", Status: appserver.ThreadStatus{Type: "active"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-1", Status: "inProgress"},
	}})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "item/started",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","startedAtMs":1787848198000,"item":{"id":"compact-1","type":"contextCompaction"}}`),
	}})
	active := findStateCard(t, reducer.Cards(), "COMPACT")
	if active.Channel != ChannelProgress || active.Disposition != protocol.DispositionNotable || active.DetailLine != "Compacting context" || active.ReasonCode != "codex_compacting" || hasStateWord(reducer.Cards(), "RUN") {
		t.Fatalf("active compaction cards = %#v", reducer.Cards())
	}

	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "item/completed",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":1787848200000,"item":{"id":"compact-1","type":"contextCompaction"}}`),
	}})
	completed := findStateCard(t, reducer.Cards(), "COMPACTED")
	if completed.Channel != ChannelOutcome || completed.Disposition != protocol.DispositionNotable || completed.DetailLine != "Context compacted" || completed.ReasonCode != "codex_compacted" || !completed.ObservedAt.Equal(now) || hasStateWord(reducer.Cards(), "COMPACT") {
		t.Fatalf("completed compaction cards = %#v", reducer.Cards())
	}

	now = now.Add(31 * time.Second)
	if hasStateWord(reducer.Cards(), "COMPACTED") {
		t.Fatalf("expired compaction cards = %#v", reducer.Cards())
	}
}

func TestReducerKeepsSafetyStateAndExactCompactionIdentity(t *testing.T) {
	now := time.Date(2026, 8, 27, 16, 30, 0, 0, time.UTC)
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-1", Name: "Safe title",
		Status:     appserver.ThreadStatus{Type: "active", ActiveFlags: []string{"waitingOnApproval"}},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-1", Status: "inProgress"},
	}})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "item/started",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","startedAtMs":1787848198000,"item":{"id":"compact-1","type":"contextCompaction"}}`),
	}})
	if !hasStateWord(reducer.Cards(), "WAIT") || hasStateWord(reducer.Cards(), "COMPACT") {
		t.Fatalf("compaction hid safety state: %#v", reducer.Cards())
	}

	for _, incoming := range []appserver.Incoming{
		{Kind: appserver.IncomingNotification, Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":1787848200000,"item":{"id":"compact-other","type":"contextCompaction"}}`)},
		{Kind: appserver.IncomingNotification, Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":1787848200000,"item":{"id":"message-1","type":"agentMessage"}}`)},
		{Kind: appserver.IncomingNotification, Method: "thread/compacted", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1"}`)},
		{Kind: appserver.IncomingNotification, Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","item":{"id":"compact-1","type":"contextCompaction"}}`)},
	} {
		reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: incoming})
	}
	if hasStateWord(reducer.Cards(), "COMPACTED") {
		t.Fatalf("inexact compaction completed: %#v", reducer.Cards())
	}

	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "item/completed",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","completedAtMs":1787848200000,"item":{"id":"compact-1","type":"contextCompaction"}}`),
	}})
	if !hasStateWord(reducer.Cards(), "COMPACTED") {
		t.Fatalf("exact compaction completion cards = %#v", reducer.Cards())
	}
}

func TestReducerRestoresOnlyTerminalCompactionAndNewTurnClearsIt(t *testing.T) {
	now := time.Date(2026, 8, 27, 16, 30, 0, 0, time.UTC)
	completedAt := now.Unix()
	reducer := NewReducer(func() time.Time { return now })
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerConnected})
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-completed", Name: "Completed", Status: appserver.ThreadStatus{Type: "idle"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-1", Status: "completed", CompletedAt: &completedAt, Items: []appserver.ItemSnapshot{{ID: "compact-1", Type: "contextCompaction"}}},
	}})
	if !hasStateWord(reducer.Cards(), "COMPACTED") {
		t.Fatalf("terminal snapshot cards = %#v", reducer.Cards())
	}
	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerIncoming, Incoming: appserver.Incoming{
		Kind: appserver.IncomingNotification, Method: "turn/started",
		Params: json.RawMessage(`{"threadId":"thread-completed","turn":{"id":"turn-2","status":"inProgress","items":[]}}`),
	}})
	if hasStateWord(reducer.Cards(), "COMPACTED") {
		t.Fatalf("new turn retained compaction outcome: %#v", reducer.Cards())
	}

	reducer.Apply(appserver.ManagerEvent{Kind: appserver.ManagerThreadAttached, Thread: &appserver.ThreadSnapshot{
		ID: "thread-active", Name: "Active", Status: appserver.ThreadStatus{Type: "active"},
		LatestTurn: &appserver.TurnSnapshot{ID: "turn-3", Status: "inProgress", Items: []appserver.ItemSnapshot{{ID: "compact-2", Type: "contextCompaction"}}},
	}})
	if hasStateWord(reducer.Cards(), "COMPACT") || hasStateWord(reducer.Cards(), "COMPACTED") {
		t.Fatalf("in-progress snapshot guessed compaction lifecycle: %#v", reducer.Cards())
	}
}

func hasStateWord(cards []Card, word string) bool {
	for _, card := range cards {
		if card.StateWord == word {
			return true
		}
	}
	return false
}

func hasCard(cards []Card, channel, key string) bool {
	for _, card := range cards {
		if card.Channel == channel && card.Key == key {
			return true
		}
	}
	return false
}

func findCard(t *testing.T, cards []Card, channel, key string) Card {
	t.Helper()
	for _, card := range cards {
		if card.Channel == channel && card.Key == key {
			return card
		}
	}
	t.Fatalf("missing card %s/%s", channel, key)
	return Card{}
}

func findStateCard(t *testing.T, cards []Card, state string) Card {
	t.Helper()
	for _, card := range cards {
		if card.StateWord == state {
			return card
		}
	}
	t.Fatalf("missing state card %q", state)
	return Card{}
}

func sceneElement(t *testing.T, scene protocol.Scene, id string) protocol.Element {
	t.Helper()
	for _, element := range scene.Elements {
		if element.ID == id {
			return element
		}
	}
	t.Fatalf("missing scene element %q", id)
	return protocol.Element{}
}
