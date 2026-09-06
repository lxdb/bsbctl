package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/lxdb/bsbctl/internal/codexusage"
	"github.com/lxdb/bsbctl/plugins/codex/internal/appserver"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	reconnectGrace          = 5 * time.Second
	runVisibilityWindow     = 8 * time.Second
	outcomeVisibilityWindow = 30 * time.Second
)

type Reducer struct {
	now                func() time.Time
	connected          bool
	connection         appserver.Connection
	disconnectedAt     time.Time
	awaitingReconcile  bool
	pending            map[string]*pendingRequest
	threads            map[string]*threadState
	pinned             string
	quotaOptions       QuotaOptions
	quota              *codexusage.Snapshot
	quotaSignal        codexusage.Signal
	quotaPressureUntil time.Time
	rateLimits         appserver.RateLimitSnapshot
	liveSequence       uint64
	requestSequence    uint64
}

type QuotaOptions struct {
	Enabled      bool
	AssetPath    string
	Presentation codexusage.PresentationConfig
}

func NewReducerWithQuota(now func() time.Time, quota QuotaOptions) *Reducer {
	if now == nil {
		now = time.Now
	}
	return &Reducer{
		now: now, disconnectedAt: now().UTC(), pending: make(map[string]*pendingRequest),
		threads: make(map[string]*threadState), quotaOptions: quota,
	}
}

func (r *Reducer) Apply(event appserver.ManagerEvent) {
	switch event.Kind {
	case appserver.ManagerConnected:
		r.connection = event.Connection
		now := r.now().UTC()
		r.awaitingReconcile = !r.disconnectedAt.IsZero() && !now.Before(r.disconnectedAt.Add(reconnectGrace))
		if r.awaitingReconcile {
			clear(r.threads)
		}
		r.connected = true
		r.disconnectedAt = time.Time{}
		clear(r.pending)
	case appserver.ManagerDisconnected:
		if r.connected || r.disconnectedAt.IsZero() {
			r.disconnectedAt = r.now().UTC()
		}
		r.connected = false
		r.awaitingReconcile = false
		clear(r.pending)
		for _, thread := range r.threads {
			thread.Status.ActiveFlags = nil
		}
		r.quota = nil
		r.quotaSignal = codexusage.SignalNone
		r.quotaPressureUntil = time.Time{}
		r.rateLimits = appserver.RateLimitSnapshot{}
	case appserver.ManagerRateLimitsSnapshot:
		if r.quotaOptions.Enabled && event.RateLimits != nil {
			r.applyRateLimits(*event.RateLimits)
		}
	case appserver.ManagerThreadAttached:
		r.applyThreadSnapshot(event.Thread)
	case appserver.ManagerThreadsReconciled:
		r.reconcileThreads(event.ThreadIDs)
		r.awaitingReconcile = false
	case appserver.ManagerIncoming:
		if event.Incoming.Kind == appserver.IncomingServerRequest {
			r.applyServerRequest(event.Incoming)
		} else {
			switch event.Incoming.Method {
			case "serverRequest/resolved":
				r.applyServerRequestResolved(event.Incoming.Params)
			case "thread/status/changed":
				r.applyThreadStatus(event.Incoming.Params)
			case "turn/plan/updated":
				r.applyPlanUpdate(event.Incoming.Params)
			case "turn/started":
				r.applyTurnStarted(event.Incoming.Params)
			case "turn/completed":
				r.applyTurnCompleted(event.Incoming.Params)
			case "item/started":
				r.applyItemStarted(event.Incoming.Params)
			case "item/completed":
				r.applyItemCompleted(event.Incoming.Params)
			case "account/rateLimits/updated":
				r.applyRateLimitsUpdate(event.Incoming.Params)
			}
		}
	}
}

func (r *Reducer) applyRateLimitsUpdate(raw json.RawMessage) {
	if !r.quotaOptions.Enabled {
		return
	}
	var notification struct {
		RateLimits appserver.RateLimitSnapshot `json:"rateLimits"`
	}
	if json.Unmarshal(raw, &notification) != nil || (notification.RateLimits.LimitID != "" && notification.RateLimits.LimitID != "codex") {
		return
	}
	r.applyRateLimits(appserver.MergeRateLimits(r.rateLimits, notification.RateLimits))
}

func (r *Reducer) applyRateLimits(snapshot appserver.RateLimitSnapshot) {
	raw := make([]codexusage.RawWindow, 0, 2)
	for _, window := range []*appserver.RateLimitWindow{snapshot.Primary, snapshot.Secondary} {
		if window == nil {
			continue
		}
		raw = append(raw, codexusage.RawWindow{
			UsedPercent: window.UsedPercent,
			Duration:    time.Duration(window.WindowDurationMinutes) * time.Minute,
			ResetsAt:    time.Unix(window.ResetsAt, 0).UTC(),
		})
	}
	normalized, err := codexusage.NormalizeWindows(raw, r.now())
	if err != nil {
		return
	}
	r.rateLimits = appserver.MergeRateLimits(r.rateLimits, snapshot)
	nextSignal := codexusage.SignalFor(normalized, r.quotaOptions.Presentation)
	if nextSignal != r.quotaSignal {
		r.quotaPressureUntil = r.now().UTC().Add(quotaPressureVisibilityWindow)
	}
	r.quotaSignal = nextSignal
	r.quota = &normalized
}

func (r *Reducer) reconcileThreads(threadIDs []string) {
	loaded := make(map[string]struct{}, len(threadIDs))
	for _, threadID := range threadIDs {
		if threadID != "" {
			loaded[threadID] = struct{}{}
		}
	}
	for threadID := range r.threads {
		if _, exists := loaded[threadID]; !exists {
			delete(r.threads, threadID)
		}
	}
	for key, request := range r.pending {
		if _, exists := loaded[request.ThreadID]; !exists {
			delete(r.pending, key)
		}
	}
	if _, exists := loaded[r.pinned]; r.pinned != "" && !exists {
		r.pinned = ""
	}
}

func (r *Reducer) applyTurnStarted(raw json.RawMessage) {
	var params struct {
		ThreadID string                 `json:"threadId"`
		Turn     appserver.TurnSnapshot `json:"turn"`
	}
	if json.Unmarshal(raw, &params) != nil || params.ThreadID == "" || params.Turn.ID == "" || params.Turn.Status != "inProgress" {
		return
	}
	thread := r.thread(params.ThreadID)
	turn := params.Turn
	turn.Items = slices.Clone(params.Turn.Items)
	thread.LatestTurn = &turn
	thread.RunStartedAt = r.now().UTC()
	if turn.StartedAt != nil {
		thread.RunStartedAt = time.Unix(*turn.StartedAt, 0).UTC()
	}
	thread.OutcomeAt = time.Time{}
	thread.PlanTotal, thread.PlanDone, thread.CompletedPlanTurnID = 0, 0, ""
	thread.CompactionItemID, thread.CompactionTurnID = "", ""
	thread.CompactionStartedAt = time.Time{}
	thread.CompletedCompactionItemID = ""
	thread.CompactionCompletedAt = time.Time{}
	r.markThreadChanged(thread)
}

func (r *Reducer) applyTurnCompleted(raw json.RawMessage) {
	var params struct {
		ThreadID string                 `json:"threadId"`
		Turn     appserver.TurnSnapshot `json:"turn"`
	}
	if json.Unmarshal(raw, &params) != nil || params.ThreadID == "" || params.Turn.ID == "" || !terminalTurnStatus(params.Turn.Status) {
		return
	}
	thread := r.thread(params.ThreadID)
	turn := params.Turn
	turn.Items = slices.Clone(params.Turn.Items)
	completedPlanTurnID := ""
	if turn.Status == "completed" && (thread.CompletedPlanTurnID == turn.ID || hasPlanItem(turn.Items)) {
		completedPlanTurnID = turn.ID
	}
	thread.LatestTurn = &turn
	thread.RunStartedAt = time.Time{}
	thread.PlanTotal, thread.PlanDone, thread.CompletedPlanTurnID = 0, 0, completedPlanTurnID
	thread.OutcomeAt = r.now().UTC()
	if turn.CompletedAt != nil {
		thread.OutcomeAt = time.Unix(*turn.CompletedAt, 0).UTC()
	}
	if thread.CompactionTurnID == turn.ID {
		thread.CompactionItemID, thread.CompactionTurnID = "", ""
		thread.CompactionStartedAt = time.Time{}
	}
	r.markThreadChanged(thread)
}

func terminalTurnStatus(status string) bool {
	switch status {
	case "completed", "interrupted", "failed":
		return true
	default:
		return false
	}
}

func hasPlanItem(items []appserver.ItemSnapshot) bool {
	for _, item := range items {
		if item.Type == "plan" {
			return true
		}
	}
	return false
}

func (r *Reducer) applyPlanUpdate(raw json.RawMessage) {
	var params struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Plan     []struct {
			Status string `json:"status"`
		} `json:"plan"`
	}
	if json.Unmarshal(raw, &params) != nil || params.ThreadID == "" || params.TurnID == "" {
		return
	}
	completed := 0
	for _, step := range params.Plan {
		switch step.Status {
		case "completed":
			completed++
		case "pending", "inProgress":
		default:
			return
		}
	}
	thread := r.thread(params.ThreadID)
	thread.PlanTotal, thread.PlanDone, thread.CompletedPlanTurnID = len(params.Plan), completed, ""
	r.markThreadChanged(thread)
}

func (r *Reducer) applyItemStarted(raw json.RawMessage) {
	var params struct {
		ThreadID  string `json:"threadId"`
		TurnID    string `json:"turnId"`
		StartedAt *int64 `json:"startedAtMs"`
		Item      struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &params) != nil || params.ThreadID == "" || params.TurnID == "" || params.StartedAt == nil || params.Item.ID == "" || params.Item.Type != "contextCompaction" {
		return
	}
	thread := r.thread(params.ThreadID)
	thread.CompactionItemID = params.Item.ID
	thread.CompactionTurnID = params.TurnID
	thread.CompactionStartedAt = time.UnixMilli(*params.StartedAt).UTC()
	thread.CompletedCompactionItemID = ""
	thread.CompactionCompletedAt = time.Time{}
	r.markThreadChanged(thread)
}

func (r *Reducer) applyItemCompleted(raw json.RawMessage) {
	var params struct {
		ThreadID    string `json:"threadId"`
		TurnID      string `json:"turnId"`
		CompletedAt *int64 `json:"completedAtMs"`
		Item        struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &params) != nil || params.ThreadID == "" || params.TurnID == "" || params.Item.ID == "" {
		return
	}
	switch params.Item.Type {
	case "plan":
		thread := r.thread(params.ThreadID)
		if thread.LatestTurn != nil && thread.LatestTurn.ID != params.TurnID {
			return
		}
		thread.CompletedPlanTurnID = params.TurnID
		r.markThreadChanged(thread)
	case "contextCompaction":
		if params.CompletedAt == nil {
			return
		}
		thread := r.thread(params.ThreadID)
		if thread.CompactionItemID != "" && (thread.CompactionItemID != params.Item.ID || thread.CompactionTurnID != params.TurnID) {
			return
		}
		completedAt := time.UnixMilli(*params.CompletedAt).UTC()
		if thread.CompactionItemID == "" && !thread.CompactionCompletedAt.IsZero() && !completedAt.After(thread.CompactionCompletedAt) {
			return
		}
		thread.CompactionItemID, thread.CompactionTurnID = "", ""
		thread.CompactionStartedAt = time.Time{}
		thread.CompletedCompactionItemID = params.Item.ID
		thread.CompactionCompletedAt = completedAt
		r.markThreadChanged(thread)
	}
}

func (r *Reducer) thread(threadID string) *threadState {
	thread := r.threads[threadID]
	if thread == nil {
		thread = &threadState{ID: threadID}
		r.threads[threadID] = thread
	}
	return thread
}

func (r *Reducer) applyThreadStatus(raw json.RawMessage) {
	var params struct {
		ThreadID string                 `json:"threadId"`
		Status   appserver.ThreadStatus `json:"status"`
	}
	if json.Unmarshal(raw, &params) != nil || params.ThreadID == "" || !validThreadStatus(params.Status.Type) {
		return
	}
	thread := r.threads[params.ThreadID]
	if thread == nil {
		thread = &threadState{ID: params.ThreadID}
		r.threads[params.ThreadID] = thread
	}
	thread.Status = params.Status
	r.markThreadChanged(thread)
}

func validThreadStatus(status string) bool {
	switch status {
	case "notLoaded", "idle", "systemError", "active":
		return true
	default:
		return false
	}
}

func (r *Reducer) applyThreadSnapshot(snapshot *appserver.ThreadSnapshot) {
	if snapshot == nil || snapshot.ID == "" {
		return
	}
	state := &threadState{
		ID: snapshot.ID, Name: snapshot.Name, Preview: snapshot.Preview, CWD: snapshot.CWD,
		Status: snapshot.Status,
	}
	if snapshot.LatestTurn != nil {
		turn := *snapshot.LatestTurn
		turn.Items = slices.Clone(snapshot.LatestTurn.Items)
		state.LatestTurn = &turn
		if turn.Status == "inProgress" && turn.StartedAt != nil {
			state.RunStartedAt = time.Unix(*turn.StartedAt, 0).UTC()
		}
		if (turn.Status == "inProgress" || turn.Status == "completed") && hasPlanItem(turn.Items) {
			state.CompletedPlanTurnID = turn.ID
		}
		if terminalTurnStatus(turn.Status) {
			state.OutcomeAt = r.now().UTC()
			if turn.CompletedAt != nil {
				state.OutcomeAt = time.Unix(*turn.CompletedAt, 0).UTC()
			}
			for _, item := range turn.Items {
				if item.Type == "contextCompaction" && item.ID != "" {
					state.CompletedCompactionItemID = item.ID
					state.CompactionCompletedAt = state.OutcomeAt
				}
			}
		}
	}
	r.threads[state.ID] = state
	r.markThreadChanged(state)
}

func (r *Reducer) applyServerRequestResolved(raw json.RawMessage) {
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return
	}
	id, err := appserver.ParseRawID(params.RequestID)
	if err != nil {
		return
	}
	key := id.Key()
	if request := r.pending[key]; request != nil {
		r.markThreadChanged(r.threads[request.ThreadID])
	}
	delete(r.pending, key)
}

func (r *Reducer) applyServerRequest(incoming appserver.Incoming) {
	kind, ok := requestKindForMethod(incoming.Method)
	if !ok || !incoming.ID.Valid() {
		return
	}
	var params struct {
		ThreadID    string `json:"threadId"`
		TurnID      string `json:"turnId"`
		ItemID      string `json:"itemId"`
		StartedAtMS *int64 `json:"startedAtMs"`
	}
	if json.Unmarshal(incoming.Params, &params) != nil || params.ThreadID == "" || params.TurnID == "" || params.ItemID == "" {
		return
	}
	startedAt := r.now().UTC()
	if params.StartedAtMS != nil {
		startedAt = time.UnixMilli(*params.StartedAtMS).UTC()
	}
	key := incoming.ID.Key()
	r.requestSequence++
	request := &pendingRequest{
		Key: observationKey("request", key), ID: incoming.ID, ArrivalSequence: r.requestSequence, Kind: kind,
		ThreadID: params.ThreadID, TurnID: params.TurnID, ItemID: params.ItemID,
		StartedAt: startedAt, Params: append(json.RawMessage(nil), incoming.Params...),
	}
	switch kind {
	case requestCommand:
		request.Actions = commandRequestActions(incoming.Params)
		request.Interactive = len(request.Actions) != 0
	case requestFile:
		request.Actions = []string{"accept", "decline", "cancel"}
		request.Interactive = true
	case requestPermission:
		var permission struct {
			Permissions json.RawMessage `json:"permissions"`
		}
		if json.Unmarshal(incoming.Params, &permission) != nil || !jsonObject(permission.Permissions) {
			return
		}
		request.Actions = []string{"grantTurn", "decline"}
		request.Permissions = append(json.RawMessage(nil), permission.Permissions...)
		request.Interactive = true
	case requestQuestion:
		request.Questions, request.Interactive = projectQuestions(incoming.Params)
	}
	r.pending[key] = request
	r.markThreadChanged(r.thread(params.ThreadID))
}

func (r *Reducer) markThreadChanged(thread *threadState) {
	if thread == nil {
		return
	}
	r.liveSequence++
	thread.liveSequence = r.liveSequence
}

func commandRequestActions(raw json.RawMessage) []string {
	var params struct {
		Available []json.RawMessage `json:"availableDecisions"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return nil
	}
	if params.Available == nil {
		return []string{"accept", "decline", "cancel"}
	}
	result := make([]string, 0, 3)
	for _, rawDecision := range params.Available {
		var decision string
		if json.Unmarshal(rawDecision, &decision) != nil || (decision != "accept" && decision != "decline" && decision != "cancel") || contains(result, decision) {
			continue
		}
		result = append(result, decision)
	}
	return result
}

func projectQuestions(raw json.RawMessage) ([]typedQuestion, bool) {
	var params struct {
		Questions []struct {
			ID       string `json:"id"`
			Header   string `json:"header"`
			Question string `json:"question"`
			IsOther  bool   `json:"isOther"`
			IsSecret bool   `json:"isSecret"`
			Options  []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(raw, &params) != nil || len(params.Questions) == 0 || len(params.Questions) > 8 {
		return nil, false
	}
	result := make([]typedQuestion, 0, len(params.Questions))
	ids := make(map[string]struct{}, len(params.Questions))
	for _, question := range params.Questions {
		if !safeExactText(question.ID, 128) || safeLine(question.Header) == "" || safeLine(question.Question) == "" || question.IsSecret || len(question.Options) == 0 || len(question.Options) > 8 {
			return nil, false
		}
		if _, exists := ids[question.ID]; exists {
			return nil, false
		}
		ids[question.ID] = struct{}{}
		projected := typedQuestion{ID: question.ID, Header: safeLine(question.Header), Question: safeLine(question.Question)}
		labels := make(map[string]struct{}, len(question.Options))
		for _, option := range question.Options {
			if !safeExactText(option.Label, 256) {
				return nil, false
			}
			if _, exists := labels[option.Label]; exists {
				return nil, false
			}
			labels[option.Label] = struct{}{}
			projected.Options = append(projected.Options, requestOption{Label: option.Label, Description: safeLine(option.Description)})
		}
		if question.IsOther {
			projected.Options = append(projected.Options, requestOption{Label: "Answer in Codex", Description: "Enter a custom answer in Codex", AnswerInCodex: true})
		}
		result = append(result, projected)
	}
	return result, true
}

func jsonObject(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}' && json.Valid(raw)
}

func (r *Reducer) PendingRequest(observationKey string) (pendingRequest, bool) {
	for _, request := range r.pending {
		if request.Key != observationKey {
			continue
		}
		copy := *request
		copy.Params = append(json.RawMessage(nil), request.Params...)
		copy.Permissions = append(json.RawMessage(nil), request.Permissions...)
		copy.Actions = slices.Clone(request.Actions)
		copy.Questions = cloneQuestions(request.Questions)
		return copy, true
	}
	return pendingRequest{}, false
}

func cloneQuestions(source []typedQuestion) []typedQuestion {
	result := make([]typedQuestion, len(source))
	for index, question := range source {
		result[index] = question
		result[index].Options = slices.Clone(question.Options)
	}
	return result
}

func (r *Reducer) InterruptTarget(key string) (threadID, turnID string, ok bool) {
	for _, thread := range r.threads {
		if thread.LatestTurn == nil || thread.LatestTurn.Status != "inProgress" {
			continue
		}
		if observationKey("thread", thread.ID) == key || runObservationKey(thread) == key {
			return thread.ID, thread.LatestTurn.ID, thread.ID != "" && thread.LatestTurn.ID != ""
		}
	}
	return "", "", false
}

func requestKindForMethod(method string) (requestKind, bool) {
	switch method {
	case "item/commandExecution/requestApproval":
		return requestCommand, true
	case "item/fileChange/requestApproval":
		return requestFile, true
	case "item/permissions/requestApproval":
		return requestPermission, true
	case "item/tool/requestUserInput":
		return requestQuestion, true
	default:
		return "", false
	}
}

func (r *Reducer) RestorePinnedThread(threadID string) { r.pinned = threadID }

func (r *Reducer) PinThread(threadID string) bool {
	if !safeThreadID(threadID) {
		return false
	}
	thread := r.threads[threadID]
	if thread == nil || (thread.Status.Type != "active" && (thread.LatestTurn == nil || thread.LatestTurn.Status != "inProgress")) {
		return false
	}
	r.pinned = threadID
	return true
}

func (r *Reducer) UnpinThread() { r.pinned = "" }

func (r *Reducer) PinnedThread() string { return r.pinned }

func (r *Reducer) ThreadSummaries() []ThreadSummary {
	ids := make([]string, 0, len(r.threads))
	for id := range r.threads {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	result := make([]ThreadSummary, 0, len(ids))
	for _, id := range ids {
		if !safeThreadID(id) {
			continue
		}
		thread := r.threads[id]
		result = append(result, ThreadSummary{ThreadID: id, Title: threadContext(thread), Status: thread.Status.Type, Pinned: id == r.pinned})
		if len(result) == 128 {
			break
		}
	}
	return result
}

func (r *Reducer) hasPendingForThread(threadID string) bool {
	for _, request := range r.pending {
		if request.ThreadID == threadID {
			return true
		}
	}
	return false
}

func threadContext(thread *threadState) string {
	session, _ := threadIdentity(thread)
	return session
}

func applyThreadIdentity(card *Card, thread *threadState) {
	card.SessionLine, card.ProjectLine = threadIdentity(thread)
	card.ContextLine = card.SessionLine
}

func threadIdentity(thread *threadState) (session, project string) {
	session, project = "Codex session", "Project"
	if thread == nil {
		return session, project
	}
	if value := safeLine(thread.Name); value != "" {
		session = value
	}
	if !safeExactText(thread.CWD, 4096) || !filepath.IsAbs(thread.CWD) {
		return session, project
	}
	cleaned := filepath.Clean(thread.CWD)
	if cleaned == string(filepath.Separator) {
		return session, project
	}
	if value := safeLine(filepath.Base(cleaned)); value != "" && value != "." && value != string(filepath.Separator) {
		project = value
	}
	return session, project
}

func safeLine(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Map(func(char rune) rune {
		if char < 0x20 || char == 0x7f {
			return ' '
		}
		return char
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 96 {
		value = string(runes[:96])
	}
	return value
}

func safeThreadID(value string) bool { return safeExactText(value, 128) }

func safeExactText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	return strings.IndexFunc(value, func(char rune) bool { return char < 0x20 || char == 0x7f }) < 0
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func connectionCard(connected bool, now time.Time) Card {
	card := Card{
		Channel: ChannelConnection, Key: "provider", StateWord: "CODEX OFF",
		ContextLine: "App server", DetailLine: "Reconnecting",
		Disposition: protocol.DispositionNotable, Impact: protocol.ImpactNotable,
		ReasonCode: "codex_disconnected", ObservedAt: now, ValidUntil: now.Add(45 * time.Second),
	}
	if connected {
		card.StateWord, card.DetailLine = "CODEX ON", "Connected"
		card.Disposition, card.Impact, card.ReasonCode = protocol.DispositionSnapshot, protocol.ImpactLow, "codex_connected"
	}
	return card
}

func reconnectingCard(observedAt, validUntil time.Time) Card {
	return Card{
		Channel: ChannelConnection, Key: "provider", StateWord: "CODEX ...",
		ContextLine: "App server", DetailLine: "Reconnecting",
		Disposition: protocol.DispositionSnapshot, Impact: protocol.ImpactLow,
		ReasonCode: "codex_reconnecting", ObservedAt: observedAt, ValidUntil: validUntil,
	}
}

func runObservationKey(thread *threadState) string {
	if thread == nil || thread.LatestTurn == nil {
		return ""
	}
	return observationKey("run", thread.ID+"\x00"+thread.LatestTurn.ID)
}

func requestCard(request *pendingRequest, thread *threadState, now time.Time) Card {
	card := Card{
		Channel: ChannelAttention, Key: request.Key, ContextLine: "Codex request",
		Disposition: protocol.DispositionActionable, Impact: protocol.ImpactCritical,
		ObservedAt: request.StartedAt, ValidUntil: now.Add(45 * time.Second),
	}
	applyThreadIdentity(&card, thread)
	switch request.Kind {
	case requestCommand:
		card.StateWord, card.DetailLine, card.ReasonCode = "WAIT CMD", "Command approval", "codex_wait_command"
	case requestFile:
		card.StateWord, card.DetailLine, card.ReasonCode = "WAIT FILE", "File approval", "codex_wait_file"
	case requestPermission:
		card.StateWord, card.DetailLine, card.ReasonCode = "WAIT PERM", "Permission approval", "codex_wait_permission"
	case requestQuestion:
		card.Channel, card.StateWord, card.DetailLine, card.ReasonCode = ChannelGuidance, "OPEN CODEX", "Use Codex", "codex_wait_question"
		card.Disposition, card.Impact = protocol.DispositionNotable, protocol.ImpactNotable
		if request.Interactive {
			card.Channel, card.StateWord, card.DetailLine = ChannelAttention, "ASK", "START TO ANSWER"
			card.Disposition, card.Impact = protocol.DispositionActionable, protocol.ImpactCritical
		}
	}
	return card
}

func observationKey(prefix, value string) string {
	digest := sha256.Sum256([]byte(value))
	return prefix + "." + hex.EncodeToString(digest[:8])
}
