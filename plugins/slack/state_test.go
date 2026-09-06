package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var fixtureNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func fixtureState(t *testing.T, extra string) *state {
	t.Helper()
	cfg, err := decodeConfig(json.RawMessage(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","channels":[{"id":"C123","alias":"BUILD"},{"id":"G123","alias":"PRIVATE"}]` + extra + `}`))
	if err != nil {
		t.Fatal(err)
	}
	return newState(cfg, "U123")
}
func callback(eventID, event string) json.RawMessage {
	return json.RawMessage(`{"type":"event_callback","api_app_id":"A123","team_id":"T123","event_id":"` + eventID + `","authorizations":[{"team_id":"T123","user_id":"U123","is_bot":false}],"event":` + event + `}`)
}
func applyFixture(t *testing.T, s *state, id, event string, at time.Time) bool {
	t.Helper()
	changed, err := s.apply(callback(id, event), at)
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

func TestStateHumanMentionsAndAuthorizedDomains(t *testing.T) {
	for _, tc := range []struct {
		name, event, extra, want string
		mention                  bool
	}{
		{"exact mention", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001","text":"Hi <@U123>"}`, "", "channel", true},
		{"prefix is not mention", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001","text":"Hi <@U1234>"}`, "", "channel", false},
		{"rich user node", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001","blocks":[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"user","user_id":"U123"}]}]}]}`, "", "channel", true},
		{"own mention", `{"type":"message","channel":"C123","channel_type":"channel","user":"U123","ts":"1.000001","text":"<@U123>"}`, "", "", false},
		{"bot", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","bot_id":"B123","ts":"1.000001","text":"<@U123>"}`, "", "", false},
		{"selected private", `{"type":"message","channel":"G123","channel_type":"group","user":"U456","ts":"1.000001","text":"<@U123>"}`, "", "channel", true},
		{"unselected private", `{"type":"message","channel":"G999","channel_type":"group","user":"U456","ts":"1.000001","text":"<@U123>"}`, ",\"group_direct_messages\":true", "", false},
		{"unselected public", `{"type":"message","channel":"C999","channel_type":"channel","user":"U456","ts":"1.000001","text":"<@U123>"}`, "", "", false},
		{"DM", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"hello"}`, "", "dm", false},
		{"disabled DM", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"hello"}`, ",\"direct_messages\":false", "", false},
		{"group DM default", `{"type":"message","channel":"G456","channel_type":"mpim","user":"U456","ts":"1.000001","text":"hello"}`, "", "", false},
		{"group DM enabled", `{"type":"message","channel":"G456","channel_type":"mpim","user":"U456","ts":"1.000001","text":"hello"}`, ",\"group_direct_messages\":true", "dm", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := fixtureState(t, tc.extra)
			applyFixture(t, s, "Ev1", tc.event, fixtureNow)
			items := s.items()
			if tc.want == "" {
				if len(items) != 0 {
					t.Fatalf("unexpected attention: %#v", items)
				}
				return
			}
			if len(items) != 1 || items[0].Kind != tc.want || items[0].Mention != tc.mention || items[0].Preview != "" || items[0].Handled {
				t.Fatalf("items: %#v", items)
			}
		})
	}
}

func TestStateAllChannelsAdmitsVisibleChannelsWithFallbackAlias(t *testing.T) {
	cfg, err := decodeConfig(json.RawMessage(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","all_channels":true,"channels":[{"id":"C123","alias":"BUILD"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	s := newState(cfg, "U123")
	for index, event := range []string{
		`{"type":"message","channel":"C999","channel_type":"channel","user":"U456","ts":"1.000001","text":"public"}`,
		`{"type":"message","channel":"G999","channel_type":"group","user":"U456","ts":"2.000001","text":"private"}`,
		`{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"3.000001","text":"selected"}`,
	} {
		applyFixture(t, s, fmt.Sprintf("Ev%d", index), event, fixtureNow.Add(time.Duration(index)*time.Second))
	}
	aliases := make(map[string]string)
	for _, item := range s.items() {
		aliases[item.ChannelID] = item.Alias
	}
	if len(aliases) != 3 || aliases["C999"] != "CHANNEL" || aliases["G999"] != "CHANNEL" || aliases["C123"] != "BUILD" {
		t.Fatalf("channel aliases = %v", aliases)
	}
}

func TestStateResolvedChannelNameReplacesOnlyFallbackAlias(t *testing.T) {
	cfg, err := decodeConfig(json.RawMessage(`{"app_id":"A123","workspace_id":"T123","user_id":"U123","all_channels":true,"channels":[{"id":"C123","alias":"BUILD"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	s := newState(cfg, "U123")
	applyFixture(t, s, "Ev1", `{"type":"message","channel":"C999","channel_type":"channel","user":"U456","ts":"1.000001","text":"public"}`, fixtureNow)
	applyFixture(t, s, "Ev2", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"2.000001","text":"selected"}`, fixtureNow)
	beforeRevision := uint64(0)
	for _, item := range s.items() {
		if item.ChannelID == "C999" {
			beforeRevision = item.Revision
		}
	}
	if !s.setChannelName("C999", "engineering-platform") {
		t.Fatal("resolved channel name did not change the fallback alias")
	}
	if s.setChannelName("C123", "ignored-name") {
		t.Fatal("resolved channel name replaced an explicit alias")
	}
	aliases := make(map[string]activity)
	for _, item := range s.items() {
		aliases[item.ChannelID] = item
	}
	if aliases["C999"].Alias != "engineering-platform" || aliases["C123"].Alias != "BUILD" || aliases["C999"].Revision <= beforeRevision {
		t.Fatalf("resolved aliases = %#v", aliases)
	}
}

func TestStateAppMentionReportsUnsupportedWithoutAttention(t *testing.T) {
	s := fixtureState(t, "")
	changed, err := s.apply(callback("Ev1", `{"type":"app_mention","channel":"C123","user":"U456","ts":"1.000001","text":"<@U123>"}`), fixtureNow)
	if !errors.Is(err, errUnsupportedEvent) || changed || len(s.items()) != 0 {
		t.Fatalf("app mention created attention or lost diagnostic classification: changed=%t err=%v items=%v", changed, err, s.items())
	}
}

func TestStateRejectsUnprovenAuthorizationWithoutLeakingBodies(t *testing.T) {
	for _, auth := range []string{``, `,"authorizations":[]`, `,"authorizations":[{"team_id":"T123","user_id":"U999","is_bot":false}]`, `,"authorizations":[{"team_id":"T123","user_id":"U123","is_bot":true}]`, `,"authorizations":[{"team_id":"T999","user_id":"U123","is_bot":false}]`} {
		s := fixtureState(t, "")
		raw := json.RawMessage(`{"type":"event_callback","api_app_id":"A123","team_id":"T123","event_id":"Ev1"` + auth + `,"event":{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"canary-body"}}`)
		if _, err := s.apply(raw, fixtureNow); err == nil || err.Error() != "unproven Slack event authorization" {
			t.Fatalf("scope error: %v", err)
		}
		if len(s.items()) != 0 {
			t.Fatal("cross-user DM admitted")
		}
	}
	s := fixtureState(t, "")
	wrongApp := json.RawMessage(`{"type":"event_callback","api_app_id":"A999","team_id":"T123","event_id":"Ev1","authorizations":[{"team_id":"T123","user_id":"U123","is_bot":false}],"event":{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"canary-body"}}`)
	if _, err := s.apply(wrongApp, fixtureNow); !errors.Is(err, errAuthorization) || len(s.items()) != 0 {
		t.Fatalf("wrong Slack app admitted: %v", err)
	}
}

func TestStateRepliesBroadcastEditsAndDeletionPreserveIdentity(t *testing.T) {
	s := fixtureState(t, "")
	root := `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001","text":"<@U123>"}`
	applyFixture(t, s, "Ev1", root, fixtureNow)
	first := s.items()[0]
	reply := `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"2.000001","thread_ts":"1.000001","text":"reply"}`
	applyFixture(t, s, "Ev2", reply, fixtureNow.Add(time.Second))
	second := s.items()[0]
	if second.ID != first.ID || second.Count != 2 || second.Fingerprint == first.Fingerprint || !second.ObservedAt.Equal(fixtureNow.Add(time.Second)) {
		t.Fatalf("reply aggregate: %#v", second)
	}
	if applyFixture(t, s, "Ev2", reply, fixtureNow.Add(2*time.Second)) {
		t.Fatal("exact callback changed state")
	}
	broadcast := strings.Replace(reply, `"type":"message"`, `"type":"message","subtype":"thread_broadcast"`, 1)
	if applyFixture(t, s, "Ev3", broadcast, fixtureNow.Add(3*time.Second)) {
		t.Fatal("broadcast rearmed reply")
	}
	if got := s.items()[0]; got.Revision != second.Revision || got.Count != 2 {
		t.Fatalf("broadcast: %#v", got)
	}
	edit := `{"type":"message","subtype":"message_changed","channel":"C123","channel_type":"channel","event_ts":"3.000001","message":{"type":"message","user":"U456","ts":"1.000001","text":"mention removed","edited":{"ts":"3.000001"}}}`
	applyFixture(t, s, "Ev4", edit, fixtureNow.Add(4*time.Second))
	if got := s.items()[0]; got.Kind != "thread" || got.Count != 2 || got.Mention || got.Fingerprint != second.Fingerprint {
		t.Fatalf("mention removal: %#v", got)
	}
	if applyFixture(t, s, "Ev5", root, fixtureNow.Add(5*time.Second)) {
		t.Fatal("old original restored removed mention")
	}
	deletion := `{"type":"message","subtype":"message_deleted","channel":"C123","channel_type":"channel","event_ts":"4.000001","deleted_ts":"2.000001"}`
	applyFixture(t, s, "Ev6", deletion, fixtureNow.Add(6*time.Second))
	if got := s.items(); len(got) != 1 || got[0].Kind != "channel" {
		t.Fatal("deleted reply did not preserve channel root")
	}
	if applyFixture(t, s, "Ev7", reply, fixtureNow.Add(7*time.Second)) || len(s.items()) != 1 {
		t.Fatal("old reply resurrected deleted message")
	}
}

func TestStateEditAddsMentionAndParticipationWatchesExpire(t *testing.T) {
	s := fixtureState(t, "")
	applyFixture(t, s, "Ev1", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001","text":"ordinary"}`, fixtureNow)
	applyFixture(t, s, "Ev2", `{"type":"message","subtype":"message_changed","channel":"C123","channel_type":"channel","event_ts":"2.000001","message":{"user":"U456","ts":"1.000001","text":"<@U123>","edited":{"ts":"2.000001"}}}`, fixtureNow.Add(time.Second))
	if got := s.items(); len(got) != 1 || !got[0].Mention {
		t.Fatalf("added mention: %#v", got)
	}
	own := `{"type":"message","channel":"C123","channel_type":"channel","user":"U123","ts":"3.000001","thread_ts":"3.000001","text":"start"}`
	applyFixture(t, s, "Ev3", own, fixtureNow)
	reply := `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"4.000001","thread_ts":"3.000001","text":"reply"}`
	applyFixture(t, s, "Ev4", reply, fixtureNow.Add(72*time.Hour))
	if len(s.items()) != 1 {
		t.Fatal("expired participation root accepted reply")
	}
	explicit := fixtureState(t, `,"watched_threads":[{"channel_id":"C123","thread_ts":"3.000001"}],"watch_participated_threads":false`)
	applyFixture(t, explicit, "Ev4", reply, fixtureNow.Add(100*time.Hour))
	if got := explicit.items(); len(got) != 1 || got[0].Kind != "thread" {
		t.Fatalf("explicit root expired: %#v", got)
	}
}

func TestStateBoundsOrderingAndTransientPreview(t *testing.T) {
	s := fixtureState(t, `,"rear_details":true`)
	for i := range 140 {
		applyFixture(t, s, fmt.Sprintf("Ev%d", i), fmt.Sprintf(`{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"%d.000001","text":"canary\n%s"}`, i+1, strings.Repeat("界", 100)), fixtureNow.Add(time.Duration(i)*time.Second))
	}
	items := s.items()
	if len(items) != 128 || !s.truncated || items[0].MessageTS != "140.000001" || items[127].MessageTS != "13.000001" {
		t.Fatalf("bounded order: count %d newest %s oldest %s", len(items), items[0].MessageTS, items[len(items)-1].MessageTS)
	}
	if len(items[0].Preview) > 160 || strings.Contains(items[0].Preview, "\n") {
		t.Fatalf("unsafe preview %q", items[0].Preview)
	}
	applyFixture(t, s, "EvMention", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001","text":"<@U123>"}`, fixtureNow.Add(200*time.Second))
	if !s.items()[0].Mention {
		t.Fatal("mention not ordered before newer DMs")
	}
}

func TestStateDeletionBeforeOriginalCannotResurrectMessage(t *testing.T) {
	s := fixtureState(t, "")
	applyFixture(t, s, "EvDelete", `{"type":"message","subtype":"message_deleted","channel":"D123","channel_type":"im","event_ts":"2.000001","deleted_ts":"1.000001"}`, fixtureNow)
	applyFixture(t, s, "EvOld", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"deleted"}`, fixtureNow.Add(time.Second))
	if len(s.items()) != 0 {
		t.Fatal("out-of-order original resurrected deletion")
	}
}

func TestStateParticipationIsCheckpointChangeAndCallbackDedupIsFIFO(t *testing.T) {
	s := fixtureState(t, "")
	own := `{"type":"message","channel":"C123","channel_type":"group","user":"U123","ts":"1.000001","text":"participation"}`
	if !applyFixture(t, s, "EvFirst", own, fixtureNow) {
		t.Fatal("participation watch did not request checkpoint")
	}
	if applyFixture(t, s, "EvFirst", own, fixtureNow.Add(73*time.Hour)) {
		t.Fatal("exact callback was not deduplicated")
	}
	for i := range 1024 {
		applyFixture(t, s, fmt.Sprintf("EvIgnore%d", i), `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"2.000001","text":"ordinary"}`, fixtureNow.Add(73*time.Hour))
	}
	if !applyFixture(t, s, "EvFirst", own, fixtureNow.Add(73*time.Hour)) {
		t.Fatal("FIFO did not evict oldest callback")
	}
	applyFixture(t, s, "EvReply", `{"type":"message","channel":"C123","channel_type":"group","user":"U456","ts":"3.000001","thread_ts":"1.000001","text":"private reply"}`, fixtureNow.Add(73*time.Hour+time.Second))
	if got := s.items(); len(got) != 2 || got[1].Kind != "thread" {
		t.Fatalf("C private participation not watched: %#v", got)
	}
}

func TestStateReorderedActivitySortsByProviderTime(t *testing.T) {
	s := fixtureState(t, "")
	applyFixture(t, s, "EvNew", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"10.000001","text":"newer"}`, fixtureNow)
	applyFixture(t, s, "EvOld", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"9.000001","text":"older"}`, fixtureNow.Add(time.Second))
	if got := s.items(); len(got) != 2 || got[0].MessageTS != "10.000001" {
		t.Fatalf("arrival order overrode activity time: %#v", got)
	}
}

func TestStateRuntimeWatchCapacityEvictsOldestRoot(t *testing.T) {
	s := fixtureState(t, "")
	for i := range 129 {
		applyFixture(t, s, fmt.Sprintf("EvOwn%d", i), fmt.Sprintf(`{"type":"message","channel":"C123","channel_type":"channel","user":"U123","ts":"%d.000001","text":"own"}`, i+1), fixtureNow.Add(time.Duration(i)*time.Second))
	}
	applyFixture(t, s, "EvEvicted", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"130.000001","thread_ts":"1.000001","text":"reply"}`, fixtureNow.Add(130*time.Second))
	if len(s.items()) != 0 {
		t.Fatal("evicted runtime root remained watched")
	}
	applyFixture(t, s, "EvRetained", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"131.000001","thread_ts":"2.000001","text":"reply"}`, fixtureNow.Add(131*time.Second))
	if got := s.items(); len(got) != 1 || got[0].RootTS != "2.000001" || !s.truncated {
		t.Fatalf("retained watch: %#v", got)
	}
}

func TestStateRetainedMessageRejectsOlderEditAfterFingerprintEviction(t *testing.T) {
	s := fixtureState(t, "")
	applyFixture(t, s, "Ev1", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"original"}`, fixtureNow)
	applyFixture(t, s, "Ev2", `{"type":"message","subtype":"message_changed","channel":"D123","channel_type":"im","event_ts":"4.000001","message":{"user":"U456","ts":"1.000001","text":"edited","edited":{"ts":"4.000001"}}}`, fixtureNow.Add(time.Second))
	first := s.items()[0]
	for i := range 128 {
		applyFixture(t, s, fmt.Sprintf("EvDelete%d", i), fmt.Sprintf(`{"type":"message","subtype":"message_deleted","channel":"D123","channel_type":"im","event_ts":"500.000001","deleted_ts":"%d.000001"}`, i+10), fixtureNow.Add(2*time.Second))
	}
	if applyFixture(t, s, "EvOldEdit", `{"type":"message","subtype":"message_changed","channel":"D123","channel_type":"im","event_ts":"3.000001","message":{"user":"U456","ts":"1.000001","text":"old edit","edited":{"ts":"3.000001"}}}`, fixtureNow.Add(3*time.Second)) {
		t.Fatal("older edit replaced retained newer version")
	}
	if s.items()[0].Revision != first.Revision {
		t.Fatal("older edit changed activity revision")
	}
}

func TestStateRetainedReplyEditRefreshesExpiredWatchWithoutNewEpisode(t *testing.T) {
	s := fixtureState(t, "")
	applyFixture(t, s, "EvOwn", `{"type":"message","channel":"C123","channel_type":"channel","user":"U123","ts":"1.000001","text":"participation"}`, fixtureNow)
	applyFixture(t, s, "EvReply", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"2.000001","thread_ts":"1.000001","text":"reply"}`, fixtureNow)
	before := s.items()[0]
	later := fixtureNow.Add(73 * time.Hour)
	// New activity cannot reactivate a root whose watch has expired.
	applyFixture(t, s, "EvUnwatched", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"3.000001","thread_ts":"1.000001","text":"unwatched reply"}`, later)
	if got := s.items(); len(got) != 1 || got[0] != before {
		t.Fatalf("unwatched new reply changed retained item: %#v", got)
	}
	applyFixture(t, s, "EvEdit", `{"type":"message","subtype":"message_changed","channel":"C123","channel_type":"channel","event_ts":"4.000001","message":{"user":"U456","ts":"2.000001","thread_ts":"1.000001","text":"edited reply","edited":{"ts":"4.000001"}}}`, later.Add(time.Second))
	got := s.items()
	if len(got) != 1 {
		t.Fatalf("retained edit removed activity: %#v", got)
	}
	if got[0].ID != before.ID || got[0].MessageTS != "2.000001" || got[0].Count != 1 || got[0].Kind != "thread" || got[0].Revision <= before.Revision || got[0].Fingerprint != before.Fingerprint || !got[0].ObservedAt.Equal(before.ObservedAt) {
		t.Fatalf("edit lost identity or duplicated episode: %#v", got[0])
	}
	// Updating known retained activity refreshes its root for subsequent replies.
	applyFixture(t, s, "EvFresh", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"5.000001","thread_ts":"1.000001","text":"fresh reply"}`, later.Add(2*time.Second))
	if got := s.items(); len(got) != 1 || got[0].Count != 2 || got[0].MessageTS != "5.000001" {
		t.Fatalf("retained edit did not refresh watch: %#v", got)
	}
}
