package codex

import (
	"encoding/json"
	"time"

	"github.com/lxdb/bsbctl/plugins/codex/internal/appserver"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

const (
	PluginID      = "dev.bsbctl.codex"
	PluginVersion = "0.1.0"
	AppID         = "codex"

	ChannelAttention     = "attention"
	ChannelGuidance      = "guidance"
	ChannelOutcome       = "outcome"
	ChannelActivity      = "activity"
	ChannelProgress      = "progress"
	ChannelOverview      = "overview"
	ChannelConnection    = "connection"
	ChannelDetail        = "detail"
	ChannelQuotaSummary  = "quota-summary"
	ChannelQuotaPressure = "quota-pressure"

	OperationSessions = "sessions"
	OperationPin      = "pin"
	OperationUnpin    = "unpin"
)

type Card struct {
	Channel     string
	Key         string
	StateWord   string
	ContextLine string
	SessionLine string
	ProjectLine string
	DetailLine  string
	Disposition protocol.Disposition
	Impact      protocol.Impact
	ReasonCode  string
	ObservedAt  time.Time
	ValidUntil  time.Time
	Scene       protocol.Scene
}

type requestKind string

const (
	requestCommand    requestKind = "command"
	requestFile       requestKind = "file"
	requestPermission requestKind = "permission"
	requestQuestion   requestKind = "question"
)

type pendingRequest struct {
	Key             string
	ID              appserver.RawID
	ArrivalSequence uint64
	Kind            requestKind
	ThreadID        string
	TurnID          string
	ItemID          string
	StartedAt       time.Time
	Params          json.RawMessage
	Actions         []string
	Questions       []typedQuestion
	Permissions     json.RawMessage
	Interactive     bool
}

type typedQuestion struct {
	ID       string
	Header   string
	Question string
	Options  []requestOption
}

type requestOption struct {
	Label         string
	Description   string
	AnswerInCodex bool
}

type threadState struct {
	ID                        string
	Name                      string
	Preview                   string
	CWD                       string
	Status                    appserver.ThreadStatus
	LatestTurn                *appserver.TurnSnapshot
	RunStartedAt              time.Time
	PlanTotal                 int
	PlanDone                  int
	CompletedPlanTurnID       string
	OutcomeAt                 time.Time
	CompactionItemID          string
	CompactionTurnID          string
	CompactionStartedAt       time.Time
	CompletedCompactionItemID string
	CompactionCompletedAt     time.Time
	liveSequence              uint64
}

type ThreadSummary struct {
	ThreadID string `json:"thread_id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Pinned   bool   `json:"pinned"`
}
