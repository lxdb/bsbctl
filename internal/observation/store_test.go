package observation

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lxdb/bsbctl/internal/presentation"
	"github.com/lxdb/bsbctl/sdk/protocol"
)

func TestStoreOwnsItsNativeBusyTimerPresentation(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	store := NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	value := validObservation(now)
	value.Scene = nil
	value.BusyTimer = &protocol.BusyTimerPresentation{Theme: "meeting"}
	if err := store.Publish(Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}
	value.BusyTimer.Theme = "mutated"
	first := store.Snapshot()
	if len(first) != 1 || first[0].Observation.BusyTimer == nil || first[0].Observation.BusyTimer.Theme != "meeting" {
		t.Fatalf("stored timer = %#v", first)
	}
	first[0].Observation.BusyTimer.Theme = "snapshot-mutated"
	second := store.Snapshot()
	if second[0].Observation.BusyTimer.Theme != "meeting" {
		t.Fatalf("snapshot shared timer pointer: %#v", second[0].Observation.BusyTimer)
	}
}

func TestStoreOwnsNestedScenePresentation(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	store := NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	value := validObservation(now)
	value.Scene = &protocol.Scene{Elements: []protocol.Element{
		{ID: "text", Display: protocol.DisplayFront, Text: &protocol.TextElement{Value: "original", Font: "normal", Marquee: &protocol.Marquee{PixelsPerMinute: 60}}},
		{ID: "image", Display: protocol.DisplayFront, Image: &protocol.ImageElement{Asset: protocol.AssetRef{StockName: "clock_5x5.image"}}},
		{ID: "animation", Display: protocol.DisplayBack, Animation: &protocol.AnimationElement{Asset: protocol.AssetRef{StockName: "calendar_event_16x16.anim"}}},
		{ID: "rectangle", Display: protocol.DisplayBack, Rectangle: &protocol.RectangleElement{Width: 4, Height: 2, Color: "#FFFFFFFF"}},
		{ID: "countdown", Display: protocol.DisplayFront, Countdown: &protocol.CountdownElement{EndsAtUnixSeconds: now.Add(time.Minute).Unix(), ShowHours: protocol.CountdownShowHoursWhenNonZero, Color: "#FFFFFFFF"}},
	}}
	if err := store.Publish(Source{PluginID: "plugin", Generation: 1}, value); err != nil {
		t.Fatal(err)
	}

	value.Scene.Elements[0].Text.Value = "mutated"
	value.Scene.Elements[0].Text.Marquee.PixelsPerMinute = 999
	value.Scene.Elements[1].Image.Asset.StockName = "mutated.image"
	value.Scene.Elements[2].Animation.Asset.StockName = "mutated.anim"
	value.Scene.Elements[3].Rectangle.Width = 99
	value.Scene.Elements[4].Countdown.Color = "#00000000"
	first := store.Snapshot()
	assertOwnedScene(t, first[0].Observation.Scene)

	first[0].Observation.Scene.Elements[0].Text.Value = "snapshot-mutated"
	first[0].Observation.Scene.Elements[0].Text.Marquee.PixelsPerMinute = 1
	first[0].Observation.Scene.Elements[1].Image.Asset.StockName = "snapshot.image"
	first[0].Observation.Scene.Elements[2].Animation.Asset.StockName = "snapshot.anim"
	first[0].Observation.Scene.Elements[3].Rectangle.Width = 1
	first[0].Observation.Scene.Elements[4].Countdown.Color = "#11111111"
	second := store.Snapshot()
	assertOwnedScene(t, second[0].Observation.Scene)
}

func assertOwnedScene(t *testing.T, scene *protocol.Scene) {
	t.Helper()
	if scene == nil || len(scene.Elements) != 5 {
		t.Fatalf("scene = %#v", scene)
	}
	if text := scene.Elements[0].Text; text == nil || text.Value != "original" || text.Marquee == nil || text.Marquee.PixelsPerMinute != 60 {
		t.Fatalf("text = %#v", text)
	}
	if image := scene.Elements[1].Image; image == nil || image.Asset.StockName != "clock_5x5.image" {
		t.Fatalf("image = %#v", image)
	}
	if animation := scene.Elements[2].Animation; animation == nil || animation.Asset.StockName != "calendar_event_16x16.anim" {
		t.Fatalf("animation = %#v", animation)
	}
	if rectangle := scene.Elements[3].Rectangle; rectangle == nil || rectangle.Width != 4 {
		t.Fatalf("rectangle = %#v", rectangle)
	}
	if countdown := scene.Elements[4].Countdown; countdown == nil || countdown.Color != "#FFFFFFFF" {
		t.Fatalf("countdown = %#v", countdown)
	}
}

func TestStoreAttachesAuthenticatedIdentityAndCoalescesRevisions(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	store := NewStore(func(pluginID, instanceID string) (uint64, bool) {
		return 3, pluginID == "dev.bsbctl.test" && instanceID == "app"
	}, func() time.Time { return now })
	source := Source{PluginID: "dev.bsbctl.test", Generation: 3}
	first := validObservation(now)
	if err := store.Publish(source, first); err != nil {
		t.Fatalf("Publish first: %v", err)
	}
	newer := first
	newer.Revision = 2
	newer.Scene.Elements[0].Text.Value = "new"
	if err := store.Publish(source, newer); err != nil {
		t.Fatalf("Publish newer: %v", err)
	}
	if err := store.Publish(source, first); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale revision error = %v", err)
	}
	got := store.Snapshot()
	if len(got) != 1 || got[0].PluginID != source.PluginID || got[0].Generation != 3 || got[0].Observation.Revision != 2 {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestStoreSequenceAdvancesOnlyForAcceptedMutations(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	store := NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	source := Source{PluginID: "dev.bsbctl.test", Generation: 1}
	value := validObservation(now)
	if got := store.Sequence(); got != 0 {
		t.Fatalf("initial sequence = %d, want 0", got)
	}
	if err := store.Publish(source, value); err != nil {
		t.Fatal(err)
	}
	if got := store.Sequence(); got != 1 {
		t.Fatalf("sequence after publish = %d, want 1", got)
	}
	if got := store.Snapshot()[0].AdmissionSequence; got != 1 {
		t.Fatalf("first admission sequence = %d, want 1", got)
	}
	_ = store.Snapshot()
	store.Signal()
	if err := store.Publish(source, value); err != nil {
		t.Fatal(err)
	}
	if got := store.Sequence(); got != 1 {
		t.Fatalf("read, signal, or idempotent publish advanced sequence to %d", got)
	}
	if got := store.Snapshot()[0].AdmissionSequence; got != 1 {
		t.Fatalf("idempotent publication changed admission sequence to %d", got)
	}
	stale := value
	stale.Revision = 0
	if err := store.Publish(source, stale); err == nil {
		t.Fatal("invalid observation was accepted")
	}
	if got := store.Sequence(); got != 1 {
		t.Fatalf("rejected publish advanced sequence to %d", got)
	}
	id := Record{PluginID: source.PluginID, Generation: source.Generation, Observation: value}.Identity()
	if !store.ExcludeRevisionWithoutSignal(id, source.Generation, value.Revision) {
		t.Fatal("current revision was not excluded")
	}
	if got := store.Sequence(); got != 1 {
		t.Fatalf("internal exclusion advanced observation sequence to %d", got)
	}
	value.Revision++
	value.UpdatedAt = now.Add(time.Second)
	if err := store.Publish(source, value); err != nil {
		t.Fatal(err)
	}
	if got := store.Sequence(); got != 2 {
		t.Fatalf("sequence after replacement = %d, want 2", got)
	}
	if got := store.Snapshot()[0].AdmissionSequence; got != 2 {
		t.Fatalf("replacement admission sequence = %d, want 2", got)
	}
	if err := store.Withdraw(source.PluginID, value.Instance.ID, value.Channel, value.Key); err != nil {
		t.Fatal(err)
	}
	if got := store.Sequence(); got != 3 {
		t.Fatalf("sequence after withdrawal = %d, want 3", got)
	}
	if err := store.Withdraw(source.PluginID, value.Instance.ID, value.Channel, value.Key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second withdrawal = %v, want not found", err)
	}
	if got := store.Sequence(); got != 3 {
		t.Fatalf("rejected withdrawal advanced sequence to %d", got)
	}
}

func TestStoreIneligibilityIsScopedToOneRevision(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	store := NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	source := Source{PluginID: "dev.bsbctl.test", Generation: 1}
	value := validObservation(now)
	if err := store.Publish(source, value); err != nil {
		t.Fatal(err)
	}
	id := Record{PluginID: source.PluginID, Generation: source.Generation, Observation: value}.Identity()
	if store.ExcludeRevisionWithoutSignal(id, source.Generation+1, value.Revision) {
		t.Fatal("revision exclusion ignored generation identity")
	}
	if !store.ExcludeRevisionWithoutSignal(id, source.Generation, value.Revision) {
		t.Fatal("ExcludeRevisionWithoutSignal did not match the current revision")
	}
	if got := store.Snapshot(); len(got) != 0 {
		t.Fatalf("ineligible revision remained selectable: %#v", got)
	}
	value.Revision++
	value.UpdatedAt = now.Add(time.Second)
	if err := store.Publish(source, value); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(); len(got) != 1 || got[0].Observation.Revision != 2 {
		t.Fatalf("new revision did not restore eligibility: %#v", got)
	}
}

func TestStoreExactExclusionSurvivesWithdrawalAndSameTupleRepublication(t *testing.T) {
	now := time.Date(2026, 9, 4, 15, 0, 0, 0, time.UTC)
	store := NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	source := Source{PluginID: "dev.bsbctl.test", Generation: 1}
	value := validObservation(now)
	id := Record{PluginID: source.PluginID, Generation: source.Generation, Observation: value}.Identity()
	if err := store.Publish(source, value); err != nil {
		t.Fatal(err)
	}
	if !store.ReserveExactRevisionWithoutSignal(id, source.Generation, value.Revision) {
		t.Fatal("exact revision was not reserved while live")
	}
	if err := store.Withdraw(source.PluginID, value.Instance.ID, value.Channel, value.Key); err != nil {
		t.Fatal(err)
	}
	if !store.ExcludeExactRevisionWithoutSignal(id, source.Generation, value.Revision) {
		t.Fatal("exact revision was not tombstoned without a live record")
	}
	if err := store.Publish(source, value); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(); len(got) != 0 {
		t.Fatalf("tombstoned tuple became eligible after republication: %#v", got)
	}
	if err := store.Withdraw(source.PluginID, value.Instance.ID, value.Channel, value.Key); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(source, value); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(); len(got) != 0 {
		t.Fatalf("withdrawal erased exact tombstone: %#v", got)
	}
	newer := value
	newer.Revision++
	newer.UpdatedAt = now.Add(time.Second)
	if err := store.Publish(source, newer); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(); len(got) != 1 || got[0].Observation.Revision != newer.Revision {
		t.Fatalf("new revision did not remain eligible: %#v", got)
	}
}

func TestStoreReleasedBackReservationDoesNotRetainWithdrawnIdentity(t *testing.T) {
	now := time.Date(2026, 9, 4, 15, 30, 0, 0, time.UTC)
	store := NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	source := Source{PluginID: "dev.bsbctl.test", Generation: 1}
	value := validObservation(now)
	id := Record{PluginID: source.PluginID, Generation: source.Generation, Observation: value}.Identity()
	if err := store.Publish(source, value); err != nil {
		t.Fatal(err)
	}
	if !store.ReserveExactRevisionWithoutSignal(id, source.Generation, value.Revision) {
		t.Fatal("exact revision was not reserved while live")
	}
	if err := store.Withdraw(source.PluginID, value.Instance.ID, value.Channel, value.Key); err != nil {
		t.Fatal(err)
	}
	store.ReleaseExactRevisionReservationWithoutSignal(id, source.Generation, value.Revision)
	if store.ExcludeExactRevisionWithoutSignal(id, source.Generation, value.Revision) {
		t.Fatal("released reservation retained authority to exclude a withdrawn revision")
	}
}

func TestStoreRevisionWatermarkSurvivesLaterExclusionsAndWithdrawal(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 0, 0, 0, time.UTC)
	store := NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	source := Source{PluginID: "dev.bsbctl.test", Generation: 1}
	first := validObservation(now)
	id := Record{PluginID: source.PluginID, Generation: source.Generation, Observation: first}.Identity()
	if err := store.Publish(source, first); err != nil {
		t.Fatal(err)
	}
	if !store.ExcludeExactRevisionWithoutSignal(id, source.Generation, first.Revision) {
		t.Fatal("first revision was not excluded")
	}
	second := first
	second.Revision = 2
	second.UpdatedAt = now.Add(time.Second)
	if err := store.Publish(source, second); err != nil {
		t.Fatal(err)
	}
	if !store.ExcludeRevisionWithoutSignal(id, source.Generation, second.Revision) {
		t.Fatal("second revision was not excluded")
	}
	if err := store.Withdraw(source.PluginID, first.Instance.ID, first.Channel, first.Key); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(source, first); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(); len(got) != 0 {
		t.Fatalf("older excluded revision replayed after withdrawal: %#v", got)
	}
	third := first
	third.Revision = 3
	third.UpdatedAt = now.Add(2 * time.Second)
	if err := store.Publish(source, third); err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot(); len(got) != 1 || got[0].Observation.Revision != third.Revision {
		t.Fatalf("new revision did not pass watermark: %#v", got)
	}
}

func TestStoreBoundsExcludedIdentityChurnAcrossWithdrawals(t *testing.T) {
	now := time.Date(2026, 9, 4, 17, 0, 0, 0, time.UTC)
	store := NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	source := Source{PluginID: "plugin", Generation: 1}

	for index := range pluginInstanceChannelCapacity {
		value := observationForIdentity(now, "instance", "channel", fmt.Sprintf("key-%d", index))
		if err := store.Publish(source, value); err != nil {
			t.Fatalf("Publish identity %d: %v", index, err)
		}
		id := Record{PluginID: source.PluginID, Generation: source.Generation, Observation: value}.Identity()
		if !store.ExcludeRevisionWithoutSignal(id, source.Generation, value.Revision) {
			t.Fatalf("Exclude identity %d", index)
		}
		if err := store.Withdraw(source.PluginID, value.Instance.ID, value.Channel, value.Key); err != nil {
			t.Fatalf("Withdraw identity %d: %v", index, err)
		}
	}

	overflow := observationForIdentity(now, "instance", "channel", "overflow")
	if err := store.Publish(source, overflow); !errors.Is(err, ErrCapacity) {
		t.Fatalf("overflow after excluded identity churn = %v, want ErrCapacity", err)
	}

	store.WithdrawInstance(source.PluginID, "instance", source.Generation)
	if err := store.Publish(source, overflow); err != nil {
		t.Fatalf("Publish after instance retirement: %v", err)
	}
}

func TestStoreExcludeRevisionWithoutSignalDoesNotWakeSelection(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store := NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	source := Source{PluginID: "plugin", Generation: 1}
	value := validObservation(now)
	if err := store.Publish(source, value); err != nil {
		t.Fatal(err)
	}
	<-store.Changes()
	id := Record{PluginID: source.PluginID, Generation: source.Generation, Observation: value}.Identity()
	if !store.ExcludeRevisionWithoutSignal(id, source.Generation, value.Revision) {
		t.Fatal("ExcludeRevisionWithoutSignal did not match current revision")
	}
	select {
	case <-store.Changes():
		t.Fatal("terminal ineligibility signaled immediate fallback selection")
	default:
	}
}

func TestStoreResolvedObservationWithdrawsAndGenerationWithdrawalIsIsolated(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	store := NewStore(func(string, string) (uint64, bool) { return 2, true }, func() time.Time { return now })
	source := Source{PluginID: "dev.bsbctl.test", Generation: 2}
	if err := store.Publish(source, validObservation(now)); err != nil {
		t.Fatal(err)
	}
	resolved := validObservation(now)
	resolved.Revision = 2
	resolved.Disposition = protocol.DispositionResolved
	resolved.ValidUntil = time.Time{}
	resolved.Scene = nil
	if err := store.Publish(source, resolved); err != nil {
		t.Fatalf("Publish resolved: %v", err)
	}
	if got := store.Snapshot(); len(got) != 0 {
		t.Fatalf("snapshot after resolution = %#v", got)
	}
}

func TestStoreIdentityTuplesCannotCollideThroughSeparators(t *testing.T) {
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	store := NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	first := validObservation(now)
	first.Instance.ID = "a/b"
	second := validObservation(now)
	second.Instance.ID = "a"
	second.Channel = "b/main"
	if err := store.Publish(Source{PluginID: "plugin", Generation: 1}, first); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(Source{PluginID: "plugin", Generation: 1}, second); err != nil {
		t.Fatal(err)
	}
	records := store.Snapshot()
	if len(records) != 2 || records[0].ID() == records[1].ID() {
		t.Fatalf("identities collided: %#v", records)
	}
}

func TestStoreRejectsNewIdentitiesAtEveryCapacityWithoutEviction(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		count     int
		identity  func(int) (pluginID, instanceID, channel, key string)
		rejection func() (pluginID, instanceID, channel, key string)
	}{
		{
			name: "plugin instance channel", count: 128,
			identity: func(index int) (string, string, string, string) {
				return "plugin", "instance", "channel", fmt.Sprintf("key-%d", index)
			},
			rejection: func() (string, string, string, string) { return "plugin", "instance", "channel", "overflow" },
		},
		{
			name: "plugin instance", count: 256,
			identity: func(index int) (string, string, string, string) {
				return "plugin", "instance", fmt.Sprintf("channel-%d", index/128), fmt.Sprintf("key-%d", index)
			},
			rejection: func() (string, string, string, string) { return "plugin", "instance", "channel-overflow", "overflow" },
		},
		{
			name: "plugin", count: 512,
			identity: func(index int) (string, string, string, string) {
				return "plugin", fmt.Sprintf("instance-%d", index/256), fmt.Sprintf("channel-%d", (index%256)/128), fmt.Sprintf("key-%d", index)
			},
			rejection: func() (string, string, string, string) { return "plugin", "instance-overflow", "channel", "overflow" },
		},
		{
			name: "global", count: 2048,
			identity: func(index int) (string, string, string, string) {
				return fmt.Sprintf("plugin-%d", index/512), fmt.Sprintf("instance-%d", (index%512)/256), fmt.Sprintf("channel-%d", (index%256)/128), fmt.Sprintf("key-%d", index)
			},
			rejection: func() (string, string, string, string) { return "plugin-overflow", "instance", "channel", "overflow" },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
			store := NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
			for index := 0; index < test.count; index++ {
				pluginID, instanceID, channel, key := test.identity(index)
				if err := store.Publish(Source{PluginID: pluginID, Generation: 1}, observationForIdentity(now, instanceID, channel, key)); err != nil {
					t.Fatalf("Publish identity %d: %v", index, err)
				}
			}
			pluginID, instanceID, channel, key := test.rejection()
			if err := store.Publish(Source{PluginID: pluginID, Generation: 1}, observationForIdentity(now, instanceID, channel, key)); !errors.Is(err, ErrCapacity) {
				t.Fatalf("overflow error = %v, want ErrCapacity", err)
			}
			if got := len(store.Snapshot()); got != test.count {
				t.Fatalf("live records = %d, want %d", got, test.count)
			}
		})
	}
}

func TestStoreAllowsReplacementAndResolutionAtCapacity(t *testing.T) {
	now := time.Date(2026, 8, 21, 1, 0, 0, 0, time.UTC)
	store := NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	for index := 0; index < 128; index++ {
		if err := store.Publish(Source{PluginID: "plugin", Generation: 1}, observationForIdentity(now, "instance", "channel", fmt.Sprintf("key-%d", index))); err != nil {
			t.Fatal(err)
		}
	}
	replacement := observationForIdentity(now, "instance", "channel", "key-0")
	replacement.Revision = 2
	replacement.Scene.Elements[0].Text.Value = "updated"
	if err := store.Publish(Source{PluginID: "plugin", Generation: 1}, replacement); err != nil {
		t.Fatalf("replacement at capacity: %v", err)
	}
	resolved := observationForIdentity(now, "instance", "channel", "key-1")
	resolved.Revision = 2
	resolved.Disposition = protocol.DispositionResolved
	resolved.ValidUntil = time.Time{}
	resolved.Scene = nil
	if err := store.Publish(Source{PluginID: "plugin", Generation: 1}, resolved); err != nil {
		t.Fatalf("resolution at capacity: %v", err)
	}
	if got := store.Snapshot(); len(got) != 127 || got[0].Observation.Scene.Elements[0].Text.Value != "updated" {
		t.Fatalf("snapshot after replacement and resolution = %#v", got)
	}
}

func TestStorePrunesExpiredRecordsBeforeCapacityAndSignalsReevaluation(t *testing.T) {
	now := time.Date(2026, 8, 21, 2, 0, 0, 0, time.UTC)
	store := NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	for index := 0; index < 128; index++ {
		value := observationForIdentity(now, "instance", "channel", fmt.Sprintf("key-%d", index))
		value.ValidUntil = now.Add(time.Second)
		if err := store.Publish(Source{PluginID: "plugin", Generation: 1}, value); err != nil {
			t.Fatal(err)
		}
	}
	drainStoreChanges(store)
	now = now.Add(time.Second)
	if err := store.Publish(Source{PluginID: "plugin", Generation: 1}, observationForIdentity(now, "instance", "channel", "replacement")); err != nil {
		t.Fatalf("Publish after expiry: %v", err)
	}
	if got := store.Snapshot(); len(got) != 1 || got[0].Observation.Key != "replacement" {
		t.Fatalf("snapshot after publish pruning = %#v", got)
	}
	select {
	case <-store.Changes():
	default:
		t.Fatal("pruning and replacement did not signal reevaluation")
	}
}

func TestStoreSnapshotPrunesExpiredRecordsAndReportsBoundedDiagnostics(t *testing.T) {
	now := time.Date(2026, 8, 21, 3, 0, 0, 0, time.UTC)
	store := NewStore(func(string, string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	expiring := observationForIdentity(now, "instance", "channel", "expiring")
	expiring.ValidUntil = now.Add(time.Second)
	if err := store.Publish(Source{PluginID: "plugin", Generation: 1}, expiring); err != nil {
		t.Fatal(err)
	}
	drainStoreChanges(store)
	now = now.Add(time.Second)
	if got := store.Snapshot(); len(got) != 0 {
		t.Fatalf("expired snapshot = %#v", got)
	}
	select {
	case <-store.Changes():
	default:
		t.Fatal("snapshot pruning did not signal reevaluation")
	}

	for index := 0; index < 128; index++ {
		if err := store.Publish(Source{PluginID: "plugin", Generation: 1}, observationForIdentity(now, "instance", "channel", fmt.Sprintf("key-%d", index))); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Publish(Source{PluginID: "plugin", Generation: 1}, observationForIdentity(now, "instance", "channel", "overflow")); !errors.Is(err, ErrCapacity) {
		t.Fatalf("overflow error = %v", err)
	}
	diagnostics := store.Diagnostics()
	if diagnostics.LiveCount != 128 || diagnostics.CapacityRejections != 1 || !diagnostics.LastRejectionAt.Equal(now) || diagnostics.LastRejectionCode != CapacityRejectionCode {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}

func TestStoreWithdrawInstanceRemovesOnlyOwnedRecordsAndSignals(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	store := NewStore(func(_, _ string) (uint64, bool) { return 1, true }, func() time.Time { return now })
	for _, source := range []struct{ plugin, instance, key string }{
		{plugin: "plugin", instance: "deleted", key: "one"},
		{plugin: "plugin", instance: "deleted", key: "two"},
		{plugin: "plugin", instance: "retained", key: "three"},
		{plugin: "other", instance: "deleted", key: "four"},
	} {
		if err := store.Publish(Source{PluginID: source.plugin, Generation: 1}, observationForIdentity(now, source.instance, "channel", source.key)); err != nil {
			t.Fatal(err)
		}
	}
	for len(store.Changes()) > 0 {
		<-store.Changes()
	}
	store.WithdrawInstance("plugin", "deleted", 1)
	if records := store.Snapshot(); len(records) != 2 {
		t.Fatalf("records after withdrawal = %#v", records)
	}
	select {
	case <-store.Changes():
	default:
		t.Fatal("instance withdrawal did not signal reevaluation")
	}
}

func TestStoreWithdrawInstanceDoesNotRemoveRecreatedGeneration(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	currentGeneration := uint64(1)
	store := NewStore(func(_, _ string) (uint64, bool) { return currentGeneration, true }, func() time.Time { return now })
	old := observationForIdentity(now, "recreated", "channel", "key")
	if err := store.Publish(Source{PluginID: "plugin", Generation: 1}, old); err != nil {
		t.Fatal(err)
	}
	currentGeneration = 2
	newer := old
	newer.Revision = 2
	if err := store.Publish(Source{PluginID: "plugin", Generation: 2}, newer); err != nil {
		t.Fatal(err)
	}
	store.WithdrawInstance("plugin", "recreated", 1)
	records := store.Snapshot()
	if len(records) != 1 || records[0].Generation != 2 {
		t.Fatalf("recreated generation was removed: %#v", records)
	}
}

func TestStoreWithdrawInstanceRemovesEveryGenerationThroughCutoffAndResetsRevision(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	currentGeneration := uint64(1)
	store := NewStore(func(_, _ string) (uint64, bool) { return currentGeneration, true }, func() time.Time { return now })
	for generation := uint64(1); generation <= 3; generation++ {
		currentGeneration = generation
		value := observationForIdentity(now, "recreated", "channel", fmt.Sprintf("old-%d", generation))
		value.Revision = 50 + generation
		if err := store.Publish(Source{PluginID: "plugin", Generation: generation}, value); err != nil {
			t.Fatal(err)
		}
	}
	currentGeneration = 4
	newer := observationForIdentity(now, "recreated", "channel", "new")
	newer.Revision = 1
	if err := store.Publish(Source{PluginID: "plugin", Generation: 4}, newer); err != nil {
		t.Fatalf("publish recreated revision one: %v", err)
	}
	store.WithdrawInstance("plugin", "recreated", 3)
	records := store.Snapshot()
	if len(records) != 1 || records[0].Generation != 4 || records[0].Observation.Revision != 1 {
		t.Fatalf("records after cutoff cleanup = %#v", records)
	}
}

func observationForIdentity(now time.Time, instanceID, channel, key string) protocol.Observation {
	value := validObservation(now)
	value.Instance.ID = instanceID
	value.Channel = channel
	value.Key = key
	return value
}

func drainStoreChanges(store *Store) {
	for {
		select {
		case <-store.Changes():
		default:
			return
		}
	}
}

func validObservation(now time.Time) protocol.Observation {
	return protocol.Observation{
		Instance: protocol.InstanceRef{ID: "app", Generation: 1}, Channel: "main", Key: "state", Revision: 1,
		Disposition: protocol.DispositionNotable, Impact: protocol.ImpactNormal, ReasonCode: "changed",
		ObservedAt: now, UpdatedAt: now, ValidUntil: now.Add(time.Minute),
		Scene: &presentation.Scene{Elements: []presentation.Element{{ID: "text", Display: protocol.DisplayFront, Text: &protocol.TextElement{Value: "old", Font: "normal"}}}},
	}
}
