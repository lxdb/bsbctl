package codex

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/plugins/codex/internal/appserver"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestPermissionInteractionReturnsExactRequestedProfileForTurnOrEmptyDecline(t *testing.T) {
	id, _ := appserver.ParseRawID(json.RawMessage(`"permission-1"`))
	request := &pendingRequest{
		ID: id, Kind: requestPermission, Interactive: true,
		Actions:     []string{"grantTurn", "decline"},
		Permissions: json.RawMessage(`{"network":{"enabled":true},"fileSystem":{"write":["/project"]}}`),
	}
	for name, index := range map[string]int{"grant": 0, "decline": 1} {
		t.Run(name, func(t *testing.T) {
			session := &interactionSession{request: request, actions: request.Actions, actionIndex: index}
			effect := session.responseEffect()
			encoded, err := json.Marshal(effect.result)
			if err != nil {
				t.Fatal(err)
			}
			want := `{"permissions":{"network":{"enabled":true},"fileSystem":{"write":["/project"]}},"scope":"turn"}`
			if name == "decline" {
				want = `{"permissions":{},"scope":"turn"}`
			}
			if string(encoded) != want {
				t.Fatalf("response = %s, want %s", encoded, want)
			}
		})
	}
}

func TestTypedQuestionInteractionSelectsAndSubmitsEveryExplicitOption(t *testing.T) {
	id, _ := appserver.ParseRawID(json.RawMessage(`"question-1"`))
	request := &pendingRequest{ID: id, Kind: requestQuestion, Interactive: true, Questions: []typedQuestion{
		{ID: "first", Question: "First?", Options: []requestOption{{Label: "A"}, {Label: "B"}}},
		{ID: "second", Question: "Second?", Options: []requestOption{{Label: "C"}, {Label: "D"}}},
	}}
	session := &interactionSession{request: request, answers: make(map[string]string)}
	if !session.navigate(1) {
		t.Fatal("first question option did not move")
	}
	if effect, refresh := session.ok(); effect != nil || !refresh || session.questionIndex != 1 {
		t.Fatalf("first answer state = effect %#v refresh %v question %d", effect, refresh, session.questionIndex)
	}
	if !session.navigate(1) {
		t.Fatal("second question option did not move")
	}
	effect, refresh := session.ok()
	if effect == nil || refresh || session.staged {
		t.Fatalf("final answer did not submit: effect %#v refresh %v staged %v", effect, refresh, session.staged)
	}
	encoded, err := json.Marshal(effect.result)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"answers":{"first":{"answers":["B"]},"second":{"answers":["D"]}}}` {
		t.Fatalf("question response = %s", encoded)
	}
}

func TestTypedQuestionHandoffDoesNotSubmitEarlierAnswersOrMatchByLabel(t *testing.T) {
	questions, interactive := projectQuestions(json.RawMessage(`{"isBlocking":false,"questions":[{"id":"first","header":"First","question":"First?","options":[{"label":"A"}]},{"id":"second","header":"Second","question":"Second?","isOther":true,"options":[{"label":"Answer in Codex"}]}]}`))
	if !interactive {
		t.Fatal("explicit choices were rejected")
	}
	id, _ := appserver.ParseRawID(json.RawMessage(`"multi-question"`))
	for _, handoff := range []bool{false, true} {
		session := &interactionSession{request: &pendingRequest{ID: id, Kind: requestQuestion, Interactive: true, Questions: questions}, answers: make(map[string]string)}
		if effect, refresh := session.ok(); effect != nil || !refresh {
			t.Fatal("first answer submitted before the final question")
		}
		if handoff {
			session.navigate(1)
		}
		effect, refresh := session.ok()
		if effect == nil || refresh {
			t.Fatal("final selection produced no effect")
		}
		if handoff {
			if !effect.answerInCodex || effect.requestID.Valid() || effect.result != nil {
				t.Fatalf("handoff submitted partial answers: %#v", effect)
			}
		} else {
			encoded, _ := json.Marshal(effect.result)
			if effect.answerInCodex || string(encoded) != `{"answers":{"first":{"answers":["A"]},"second":{"answers":["Answer in Codex"]}}}` {
				t.Fatalf("explicit option was mistaken for handoff: %s", encoded)
			}
		}
	}
}

func TestTypedQuestionStartIsNoOpWhileNonQuestionStartKeepsConfirmation(t *testing.T) {
	t.Parallel()
	question := &interactionSession{request: &pendingRequest{
		Kind: requestQuestion, Interactive: true,
		Questions: []typedQuestion{{ID: "choice", Options: []requestOption{{Label: "A"}}}},
	}}
	if effect, refresh := question.start(); effect != nil || refresh || question.staged {
		t.Fatalf("typed ASK START changed state: %#v / %v / %v", effect, refresh, question.staged)
	}

	approval := &interactionSession{request: &pendingRequest{Kind: requestFile}, actions: []string{"accept"}}
	if effect, refresh := approval.start(); effect != nil || !refresh || !approval.staged {
		t.Fatalf("approval START did not stage confirmation: %#v / %v / %v", effect, refresh, approval.staged)
	}
}

func TestTypedQuestionDetailRendersPositionsFullMarqueeOptionAndBoundedDescription(t *testing.T) {
	t.Parallel()
	sharedPrefix := strings.Repeat("s", 248)
	wantedLabel := sharedPrefix + "option-b"
	request := &pendingRequest{Kind: requestQuestion, Interactive: true, Questions: []typedQuestion{
		{
			ID: "first", Question: "First?",
			Options: []requestOption{
				{Label: sharedPrefix + "option-a"},
				{Label: wantedLabel, Description: strings.Repeat("bounded description ", 10)},
			},
		},
		{ID: "second", Question: "Second?", Options: []requestOption{{Label: "C"}}},
	}}
	session := &interactionSession{
		card:    Card{StateWord: "ASK", SessionLine: "Safe session", ProjectLine: "Safe project"},
		request: request, answers: make(map[string]string),
	}
	if !session.navigate(1) {
		t.Fatal("option did not move")
	}
	detail := session.detailCard(time.Now())
	if detail.SessionLine != "Safe session" || detail.ProjectLine != "Safe project" {
		t.Fatalf("detail identity = %q / %q", detail.SessionLine, detail.ProjectLine)
	}
	if got := cardElement(t, detail.Scene, "back-question-position").Text.Value; got != "QUESTION 1/2" {
		t.Fatalf("question position = %q", got)
	}
	if got := cardElement(t, detail.Scene, "back-option-position").Text.Value; got != "OPTION 2/2" {
		t.Fatalf("option position = %q", got)
	}
	option := cardElement(t, detail.Scene, "back-option-label")
	if option.Text.Value != wantedLabel || option.Text.Width == 0 || option.Text.Marquee == nil {
		t.Fatalf("option marquee = %#v", option)
	}
	description := cardElement(t, detail.Scene, "back-option-description").Text.Value
	if description == "" || len([]rune(description)) > 96 {
		t.Fatalf("bounded description = %q", description)
	}
}

func TestTypedQuestionDetailRendersActualQuestionAsNativeMarquee(t *testing.T) {
	t.Parallel()
	questionText := strings.TrimSpace(strings.Repeat("Long question text ", 5))
	session := &interactionSession{
		card: Card{StateWord: "ASK", SessionLine: "Safe session", ProjectLine: "Safe project"},
		request: &pendingRequest{Kind: requestQuestion, Interactive: true, Questions: []typedQuestion{{
			ID: "choice", Question: questionText, Options: []requestOption{{Label: "A"}},
		}}},
		answers: make(map[string]string),
	}
	question := cardElement(t, session.detailCard(time.Now()).Scene, "back-question")
	if question.Text.Value != questionText || question.Text.Width == 0 || question.Text.Marquee == nil {
		t.Fatalf("question marquee = %#v", question)
	}
}

func TestTypedQuestionDetailUsesSelectedOptionFrontAndTaskFirstBack(t *testing.T) {
	t.Parallel()
	sessionName := strings.Repeat("S", 80)
	projectName := strings.Repeat("P", 80)
	session := &interactionSession{
		card: Card{StateWord: "ASK", SessionLine: sessionName, ProjectLine: projectName},
		request: &pendingRequest{Kind: requestQuestion, Interactive: true, Questions: []typedQuestion{{
			ID: "choice", Question: "Choose", Options: []requestOption{{Label: "A"}},
		}}},
		answers: make(map[string]string),
	}
	scene := session.detailCard(time.Now()).Scene
	option := cardElement(t, scene, "front-option-label")
	if option.Text.Value != "A" || option.Text.Font != "normal" || option.Text.Width == 0 || option.Text.Marquee == nil {
		t.Fatalf("typed ASK front selected option = %#v", option)
	}
	for _, id := range []string{"back-question", "back-option-label"} {
		if got := cardElement(t, scene, id); got.Text.Font != "normal" || got.Text.Width == 0 || got.Text.Marquee == nil {
			t.Fatalf("typed ASK %s = %#v", id, got)
		}
	}
	for _, element := range scene.Elements {
		if element.ID == "back-session" || element.ID == "back-session-label" {
			t.Fatalf("typed ASK back retained competing session element %#v", element)
		}
	}
}

func TestPlanReadyInteractionIsDisplayOnly(t *testing.T) {
	session := &interactionSession{card: Card{StateWord: "PLAN READY", ContextLine: "Thread"}, detailKey: "session.plan"}
	if effect, refresh := session.ok(); effect != nil || refresh || session.staged {
		t.Fatalf("PLAN READY produced action state: effect %#v refresh %v staged %v", effect, refresh, session.staged)
	}
	detail := session.detailCard(time.Now())
	if detail.Disposition != protocol.DispositionSnapshot || detail.DetailLine != "Display only" {
		t.Fatalf("PLAN READY detail = %#v", detail)
	}
}

func TestRequestContextHidesCommandAndPathUnlessExplicitlyEnabled(t *testing.T) {
	request := pendingRequest{Kind: requestCommand, Params: json.RawMessage(`{"command":"rm -rf /hidden","cwd":"/hidden/project","reason":"Needs hidden access"}`)}
	if got := requestContext(request, false); got != "Codex request" {
		t.Fatalf("default context = %q", got)
	}
	if got := requestContext(request, true); !strings.Contains(got, "rm -rf /hidden") {
		t.Fatalf("opt-in context = %q", got)
	}
}

func TestRequestContextNeverUsesFullCWDAsIdentity(t *testing.T) {
	t.Parallel()
	request := pendingRequest{Kind: requestFile, Params: json.RawMessage(`{"cwd":"/private/hidden/project"}`)}
	if got := requestContext(request, true); got != "Codex request" {
		t.Fatalf("full CWD context = %q", got)
	}
}

func TestInterruptInteractionRequiresConfirmationAndKeepsExactTurn(t *testing.T) {
	session := &interactionSession{threadID: "thread-1", turnID: "turn-7", actions: []string{"interrupt"}}
	if effect, refresh := session.ok(); effect != nil || !refresh || !session.staged {
		t.Fatalf("interrupt did not stage: %#v/%v/%v", effect, refresh, session.staged)
	}
	effect, _ := session.ok()
	if effect == nil || effect.threadID != "thread-1" || effect.turnID != "turn-7" {
		t.Fatalf("interrupt effect = %#v", effect)
	}
}
