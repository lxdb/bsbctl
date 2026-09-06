package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCheckpointHandlingProposalIsDurableOnlyAfterCommit(t *testing.T) {
	s := fixtureState(t, `,"rear_details":true`)
	message := `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"body-canary xoxp-token-canary"}`
	applyFixture(t, s, "Ev1", message, fixtureNow)
	item := s.items()[0]
	proposed, raw, err := s.proposeHandle(item.ID, item.Revision, item.Fingerprint, fixtureNow.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if s.items()[0].Handled || !proposed.items()[0].Handled {
		t.Fatal("proposal changed committed handling")
	}
	for _, canary := range []string{"body-canary", "xoxp-token-canary", "D123", "U456", "T123"} {
		if strings.Contains(string(raw), canary) {
			t.Fatalf("checkpoint leaked %q", canary)
		}
	}
	if len(raw) >= 65536 {
		t.Fatal("oversized checkpoint")
	}
	// A failed saver discards proposed. Retrying the original selection remains valid.
	retry, _, err := s.proposeHandle(item.ID, item.Revision, item.Fingerprint, fixtureNow.Add(2*time.Second))
	if err != nil || !retry.items()[0].Handled || s.items()[0].Handled {
		t.Fatal("failed persistence was not retryable")
	}
	s = proposed
	applyFixture(t, s, "Ev2", `{"type":"message","channel":"D123","channel_type":"im","user":"U123","ts":"2.000001","thread_ts":"1.000001","text":"own reply"}`, fixtureNow.Add(3*time.Second))
	if !s.items()[0].Handled {
		t.Fatal("own message rearmed attention")
	}
	applyFixture(t, s, "Ev3", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"3.000001","thread_ts":"1.000001","text":"new reply"}`, fixtureNow.Add(4*time.Second))
	if got := s.items()[0]; got.Handled || got.Fingerprint == item.Fingerprint || !got.ObservedAt.Equal(fixtureNow.Add(4*time.Second)) {
		t.Fatalf("new episode: %#v", got)
	}
	if _, _, err := s.proposeHandle(item.ID, item.Revision, item.Fingerprint, fixtureNow); !errors.Is(err, errStaleActivity) {
		t.Fatalf("stale action accepted: %v", err)
	}
}

func TestCheckpointRestartSuppressesRetriesAndValidatesScopeVersion(t *testing.T) {
	s := fixtureState(t, "")
	message := `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001","text":"<@U123>"}`
	applyFixture(t, s, "Ev1", message, fixtureNow)
	raw, err := s.checkpoint(fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	restored := fixtureState(t, "")
	if err := restored.restoreCheckpoint(raw, fixtureNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if applyFixture(t, restored, "EvNewCallback", message, fixtureNow.Add(time.Minute)) || len(restored.items()) != 0 {
		t.Fatal("restart replay created attention")
	}
	applyFixture(t, restored, "EvReply", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"2.000001","thread_ts":"1.000001","text":"new activity"}`, fixtureNow.Add(2*time.Minute))
	if got := restored.items(); len(got) != 1 || got[0].Kind != "thread" {
		t.Fatalf("restored watch lost: %#v", got)
	}
	for _, bad := range []json.RawMessage{json.RawMessage(`{"schema_version":2}`), json.RawMessage(`{"schema_version":1,"scope":"other"}`), json.RawMessage(`null`), json.RawMessage(strings.Repeat(" ", 65536))} {
		target := fixtureState(t, "")
		if err := target.restoreCheckpoint(raw, fixtureNow); err != nil {
			t.Fatal(err)
		}
		if err := target.restoreCheckpoint(bad, fixtureNow); err == nil {
			t.Fatal("accepted incompatible checkpoint")
		}
		after, err := target.checkpoint(fixtureNow)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(raw) {
			t.Fatal("incompatible checkpoint changed recovery metadata")
		}
		if applyFixture(t, target, "EvRetry", message, fixtureNow) || len(target.items()) != 0 {
			t.Fatal("incompatible checkpoint lost retry suppression")
		}
	}
	other := newState(s.config, "U999")
	if err := other.restoreCheckpoint(raw, fixtureNow); err == nil {
		t.Fatal("cross-user restore accepted")
	}
}

func TestCheckpointExpiresWatchesAndHandledEpisodes(t *testing.T) {
	s := fixtureState(t, "")
	applyFixture(t, s, "Ev1", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001","text":"<@U123>"}`, fixtureNow)
	item := s.items()[0]
	proposal, raw, err := s.proposeHandle(item.ID, item.Revision, item.Fingerprint, fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	restored := fixtureState(t, "")
	if err := restored.restoreCheckpoint(raw, fixtureNow.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(restored.handled) != 0 || len(restored.watches) != 0 {
		t.Fatal("expired recovery metadata survived")
	}
	proposal.prune(fixtureNow.Add(7 * 24 * time.Hour))
	if proposal.items()[0].Handled {
		t.Fatal("expired local handling survived")
	}
}

func TestCheckpointByteBudgetAndRecentRecovery(t *testing.T) {
	s := fixtureState(t, "")
	for i := range 150 {
		applyFixture(t, s, fmt.Sprintf("Ev%d", i), fmt.Sprintf(`{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"%d.000001","text":"<@U123>"}`, i+1), fixtureNow.Add(time.Duration(i)*time.Second))
		item := s.items()[0]
		next, _, err := s.proposeHandle(item.ID, item.Revision, item.Fingerprint, fixtureNow.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		s = next
	}
	raw, err := s.checkpoint(fixtureNow.Add(3 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.checkpoint(fixtureNow.Add(3 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(again) || len(raw) >= 65536 {
		t.Fatalf("nondeterministic or oversized checkpoint: %d", len(raw))
	}
	restored := fixtureState(t, "")
	if err := restored.restoreCheckpoint(raw, fixtureNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(restored.fingerprints) > 128 || len(restored.watches) != 128 || len(restored.handled) != 128 || !restored.truncated {
		t.Fatal("unbounded recovery or lost watches/handling before replay byte trimming")
	}
	// 150 chronological observations retain observation 23 through 150. The
	// independent times catch eviction of the newest rather than oldest metadata.
	for _, w := range restored.watches {
		if w.LastActivity.Before(fixtureNow.Add(22*time.Second)) || w.LastActivity.After(fixtureNow.Add(149*time.Second)) {
			t.Fatalf("wrong watch survivor: %#v", w)
		}
	}
	for _, h := range restored.handled {
		if h.HandledAt.Before(fixtureNow.Add(22*time.Second)) || h.HandledAt.After(fixtureNow.Add(149*time.Second)) {
			t.Fatalf("wrong handled survivor: %#v", h)
		}
	}
}

func TestStateMaterialEditInvalidatesActionWithoutNewEpisode(t *testing.T) {
	s := fixtureState(t, "")
	applyFixture(t, s, "Ev1", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"first"}`, fixtureNow)
	first := s.items()[0]
	next, _, err := s.proposeHandle(first.ID, first.Revision, first.Fingerprint, fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	s = next
	applyFixture(t, s, "Ev2", `{"type":"message","subtype":"message_changed","channel":"D123","channel_type":"im","event_ts":"2.000001","message":{"user":"U456","ts":"1.000001","text":"changed","edited":{"ts":"2.000001"}}}`, fixtureNow.Add(time.Second))
	got := s.items()[0]
	if !got.Handled || got.Fingerprint != first.Fingerprint || !got.ObservedAt.Equal(first.ObservedAt) || got.Revision <= first.Revision {
		t.Fatalf("edit episode: %#v", got)
	}
	if _, _, err := s.proposeHandle(first.ID, first.Revision, first.Fingerprint, fixtureNow); !errors.Is(err, errStaleActivity) {
		t.Fatalf("edited action accepted: %v", err)
	}
}

func TestCheckpointHandlingAndExpiryAdvanceMaterialRevision(t *testing.T) {
	s := fixtureState(t, "")
	applyFixture(t, s, "Ev1", `{"type":"message","channel":"D123","channel_type":"im","user":"U456","ts":"1.000001","text":"incoming"}`, fixtureNow)
	first := s.items()[0]
	proposed, _, err := s.proposeHandle(first.ID, first.Revision, first.Fingerprint, fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	handled := proposed.items()[0]
	if handled.Revision <= first.Revision || !handled.ObservedAt.Equal(first.ObservedAt) {
		t.Fatal("handling changed material state without revision")
	}
	proposed.prune(fixtureNow.Add(7 * 24 * time.Hour))
	expired := proposed.items()[0]
	if expired.Revision <= handled.Revision || expired.Handled {
		t.Fatal("handling expiry changed state without revision")
	}
}

func TestCheckpointRejectsCorruptMetadataAtomically(t *testing.T) {
	s := fixtureState(t, "")
	message := `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"1.000001","text":"<@U123>"}`
	applyFixture(t, s, "Ev1", message, fixtureNow)
	item := s.items()[0]
	s, _, err := s.proposeHandle(item.ID, item.Revision, item.Fingerprint, fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	// All mutable recovery fields are populated, including truncation and revision.
	s.truncated = true
	raw, err := s.checkpoint(fixtureNow)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"unknown private field", func(w map[string]any) { w["text"] = "body-canary" }},
		{"invalid fingerprint", func(w map[string]any) { w["fingerprints"].([]any)[0].(map[string]any)["digest"] = "xoxp-token-canary" }},
		{"duplicate fingerprint", func(w map[string]any) { a := w["fingerprints"].([]any); w["fingerprints"] = append(a, a[0]) }},
		{"future watermark", func(w map[string]any) {
			w["fingerprints"].([]any)[0].(map[string]any)["seen_at"] = "2099-01-01T00:00:00Z"
		}},
		{"invalid timestamp", func(w map[string]any) { w["fingerprints"].([]any)[0].(map[string]any)["version"] = "1e99" }},
		{"invalid watch after valid fingerprints", func(w map[string]any) { w["watches"].([]any)[0].(map[string]any)["key"] = "invalid-watch" }},
		{"invalid handled entry after valid watches", func(w map[string]any) { w["handled"].([]any)[0].(map[string]any)["fingerprint"] = "invalid-episode" }},
		{"unbounded records", func(w map[string]any) {
			a := w["fingerprints"].([]any)
			items := make([]any, 129)
			for i := range items {
				items[i] = a[0]
			}
			w["fingerprints"] = items
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire map[string]any
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			// A bad checkpoint tries to replace every other field with different,
			// otherwise valid metadata. Applying even an early portion must be caught.
			wire["revision"] = float64(99)
			wire["truncated"] = false
			wire["fingerprints"].([]any)[0].(map[string]any)["key"] = strings.Repeat("a", 64)
			wire["watches"].([]any)[0].(map[string]any)["key"] = strings.Repeat("b", 64)
			wire["handled"].([]any)[0].(map[string]any)["fingerprint"] = strings.Repeat("c", 64)
			tc.mutate(wire)
			bad, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			target := fixtureState(t, "")
			if err := target.restoreCheckpoint(raw, fixtureNow); err != nil {
				t.Fatal(err)
			}
			err = target.restoreCheckpoint(bad, fixtureNow)
			if err == nil || strings.Contains(err.Error(), "canary") {
				t.Fatalf("bad checkpoint error: %v", err)
			}
			after, err := target.checkpoint(fixtureNow)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(raw) {
				t.Fatalf("rejected restore changed recoverable metadata\nbefore %s\nafter %s", raw, after)
			}
			// No retained message exists in target, so only restored fingerprints can
			// suppress this retry. Comparing items on the original state would miss it.
			if applyFixture(t, target, "EvRetry", message, fixtureNow) || len(target.items()) != 0 {
				t.Fatal("rejected restore lost replay suppression")
			}
			applyFixture(t, target, "EvReply", `{"type":"message","channel":"C123","channel_type":"channel","user":"U456","ts":"2.000001","thread_ts":"1.000001","text":"fresh reply"}`, fixtureNow.Add(time.Second))
			if got := target.items(); len(got) != 1 || got[0].Kind != "thread" || got[0].MessageTS != "2.000001" {
				t.Fatalf("rejected restore lost participated watch: %#v", got)
			}
		})
	}
}

func TestCheckpointEvictsExactOldestMetadataWithStableIDTies(t *testing.T) {
	// These fixture keys are independent opaque identifiers. The hand-selected
	// survivor interval is 2..129: key 1 must be the sole eviction in every case.
	want := make([]string, 0, 128)
	for i := 2; i <= 129; i++ {
		want = append(want, fmt.Sprintf("%064x", i))
	}
	for _, kind := range []string{"fingerprints", "watches", "handled"} {
		for _, tied := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/tied=%v", kind, tied), func(t *testing.T) {
				var first json.RawMessage
				for _, reverse := range []bool{false, true} {
					s := fixtureState(t, "")
					for offset := range 128 {
						ordinal := offset + 1
						if reverse {
							ordinal = 128 - offset
						}
						key := fmt.Sprintf("%064x", ordinal)
						at := fixtureNow.Add(time.Duration(ordinal) * time.Second)
						if tied {
							at = fixtureNow
						}
						switch kind {
						case "fingerprints":
							s.fingerprints[key] = messageFingerprint{Key: key, Version: "1.000001", Digest: strings.Repeat("d", 64), SeenAt: at}
						case "watches":
							s.watches[key] = watch{Key: key, LastActivity: at}
						case "handled":
							s.handled[key] = handledEpisode{Fingerprint: key, ObservedAt: fixtureNow, HandledAt: at}
						}
					}
					newest := "0000000000000000000000000000000000000000000000000000000000000081"
					at := fixtureNow.Add(129 * time.Second)
					if tied {
						at = fixtureNow
					}
					switch kind {
					case "fingerprints":
						s.remember(newest, normalizedEvent{version: "1.000001", digest: strings.Repeat("d", 64)}, at)
					case "watches":
						s.touchWatch(newest, at)
					case "handled":
						s.aggregates["item-fixture"] = activity{ID: "item-fixture", Revision: 1, Fingerprint: newest, ObservedAt: fixtureNow}
						proposed, _, err := s.proposeHandle("item-fixture", 1, newest, at)
						if err != nil {
							t.Fatal(err)
						}
						s = proposed
					}
					raw, err := s.checkpoint(fixtureNow.Add(3 * time.Minute))
					if err != nil {
						t.Fatal(err)
					}
					var wire struct {
						Fingerprints []struct {
							Key string `json:"key"`
						} `json:"fingerprints"`
						Watches []struct {
							Key string `json:"key"`
						} `json:"watches"`
						Handled []struct {
							Fingerprint string `json:"fingerprint"`
						} `json:"handled"`
					}
					if err := json.Unmarshal(raw, &wire); err != nil {
						t.Fatal(err)
					}
					got := make([]string, 0, 128)
					switch kind {
					case "fingerprints":
						for _, entry := range wire.Fingerprints {
							got = append(got, entry.Key)
						}
					case "watches":
						for _, entry := range wire.Watches {
							got = append(got, entry.Key)
						}
					case "handled":
						for _, entry := range wire.Handled {
							got = append(got, entry.Fingerprint)
						}
					}
					if !slices.Equal(got, want) {
						t.Fatalf("wrong ordered survivors (reverse=%v): got %v, want exactly keys 2..129", reverse, got)
					}
					if first == nil {
						first = raw
					} else if string(first) != string(raw) {
						t.Fatal("equivalent insertion orders produced different checkpoints")
					}
				}
			})
		}
	}
}
